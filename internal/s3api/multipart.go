package s3api

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gocql/gocql"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

func (s *Server) initiateMultipart(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string) {
	class := r.Header.Get("x-amz-storage-class")
	if class == "" {
		class = b.DefaultClass
	}
	checksumAlgo := normalizeChecksumAlgo(r.Header.Get("x-amz-checksum-algorithm"))
	checksumType := strings.ToUpper(strings.TrimSpace(r.Header.Get("x-amz-checksum-type")))
	if checksumAlgo != "" && checksumType == "" {
		// AWS default: COMPOSITE for SHA-family, FULL_OBJECT for CRC-family
		// in modern SDKs. Default COMPOSITE — the wire response carries the
		// type back so clients see the resolved value.
		checksumType = "COMPOSITE"
	}
	if checksumType != "" && checksumType != "COMPOSITE" && checksumType != "FULL_OBJECT" {
		writeError(w, r, ErrInvalidArgument)
		return
	}
	mu := &meta.MultipartUpload{
		BucketID:          b.ID,
		UploadID:          gocql.TimeUUID().String(),
		Key:               key,
		StorageClass:      class,
		ContentType:       r.Header.Get("Content-Type"),
		InitiatedAt:       time.Now().UTC(),
		Status:            "uploading",
		ChecksumAlgorithm: checksumAlgo,
		ChecksumType:      checksumType,
	}
	// US-010 backend pass-through: when the data backend can map a Strata
	// multipart 1:1 onto its own multipart upload (s3-over-s3), initiate
	// the backend session up-front and persist the opaque handle on the
	// meta row so subsequent UploadPart / Complete / Abort calls can
	// resume it. Threads the bucket UUID via context so the s3 backend
	// can build its <bucket-uuid>/<object-uuid> key (US-009).
	if mb, ok := s.Data.(data.MultipartBackend); ok {
		ctx := data.WithBucketID(r.Context(), b.ID)
		handle, err := mb.CreateBackendMultipart(ctx, class)
		if err != nil {
			writeError(w, r, ErrInternal)
			return
		}
		mu.BackendUploadID = handle
	}
	if err := s.Meta.CreateMultipartUpload(r.Context(), mu); err != nil {
		// Best-effort cleanup of the backend session if meta persistence
		// fails — leaves no orphan multipart in the backend bucket.
		if mb, ok := s.Data.(data.MultipartBackend); ok && mu.BackendUploadID != "" {
			_ = mb.AbortBackendMultipart(r.Context(), mu.BackendUploadID)
		}
		mapMetaErr(w, r, err)
		return
	}
	writeXML(w, http.StatusOK, initiateMultipartResult{
		Bucket:   b.Name,
		Key:      key,
		UploadID: mu.UploadID,
	})
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key, uploadID string) {
	pnStr := r.URL.Query().Get("partNumber")
	partNumber, err := strconv.Atoi(pnStr)
	if err != nil || partNumber < 1 || partNumber > 10000 {
		writeError(w, r, ErrInvalidArgument)
		return
	}
	mu, err := s.Meta.GetMultipartUpload(r.Context(), b.ID, uploadID)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if r.Header.Get("x-amz-copy-source") != "" {
		s.uploadPartCopy(w, r, b, uploadID, mu, partNumber)
		return
	}
	// US-003 FlexibleChecksum: when the multipart session was initiated
	// with a checksum algorithm, capture the per-part digest the client
	// supplied via `x-amz-checksum-<algo>`. The value is replayed on
	// CompleteMultipartUpload to compute the COMPOSITE checksum and
	// echoed on ?partNumber=N HEAD/GET when ChecksumMode=ENABLED.
	var partChecksumValue, partChecksumAlgo string
	if mu.ChecksumAlgorithm != "" {
		if hdr := checksumHeader(mu.ChecksumAlgorithm); hdr != "" {
			if v := r.Header.Get(hdr); v != "" {
				partChecksumValue = v
				partChecksumAlgo = mu.ChecksumAlgorithm
			}
		}
	}
	// US-010 backend pass-through: when the multipart session was
	// initiated against the s3 backend's own multipart upload, stream
	// this part straight to the backend's UploadPart instead of through
	// PutChunks (which would write a separate backend object per part
	// and break the 1:1 invariant).
	if mu.BackendUploadID != "" {
		mb, ok := s.Data.(data.MultipartBackend)
		if !ok {
			writeError(w, r, ErrInternal)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		etag, err := mb.UploadBackendPart(ctx, mu.BackendUploadID, int32(partNumber), r.Body, r.ContentLength)
		if err != nil {
			if apiErr, ok := MapBodyError(err); ok {
				writeError(w, r, apiErr)
				return
			}
			writeError(w, r, ErrInternal)
			return
		}
		part := &meta.MultipartPart{
			PartNumber:        partNumber,
			ETag:              etag,
			Size:              r.ContentLength,
			BackendETag:       etag,
			ChecksumValue:     partChecksumValue,
			ChecksumAlgorithm: partChecksumAlgo,
		}
		if err := s.Meta.SavePart(r.Context(), b.ID, uploadID, part); err != nil {
			mapMetaErr(w, r, err)
			return
		}
		w.Header().Set("ETag", `"`+etag+`"`)
		if partChecksumValue != "" {
			w.Header().Set(checksumHeader(partChecksumAlgo), partChecksumValue)
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	ctx, cancel := context.WithTimeout(data.WithBucketID(r.Context(), b.ID), 10*time.Minute)
	defer cancel()
	manifest, err := s.Data.PutChunks(ctx, r.Body, mu.StorageClass)
	if err != nil {
		if apiErr, ok := MapBodyError(err); ok {
			writeError(w, r, apiErr)
			return
		}
		if strings.Contains(err.Error(), "unknown storage class") {
			writeError(w, r, ErrInvalidStorageClass)
			return
		}
		writeError(w, r, ErrInternal)
		return
	}
	part := &meta.MultipartPart{
		PartNumber:        partNumber,
		ETag:              manifest.ETag,
		Size:              manifest.Size,
		Manifest:          manifest,
		ChecksumValue:     partChecksumValue,
		ChecksumAlgorithm: partChecksumAlgo,
	}
	if err := s.Meta.SavePart(r.Context(), b.ID, uploadID, part); err != nil {
		_ = s.Data.Delete(r.Context(), manifest)
		mapMetaErr(w, r, err)
		return
	}
	w.Header().Set("ETag", `"`+manifest.ETag+`"`)
	if partChecksumValue != "" {
		w.Header().Set(checksumHeader(partChecksumAlgo), partChecksumValue)
	}
	w.WriteHeader(http.StatusOK)
}

type copyPartResult struct {
	XMLName      xml.Name `xml:"CopyPartResult"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

func (s *Server) uploadPartCopy(w http.ResponseWriter, r *http.Request, b *meta.Bucket, uploadID string, mu *meta.MultipartUpload, partNumber int) {
	srcBucket, srcKey, srcVersion, ok := parseCopySource(r.Header.Get("x-amz-copy-source"))
	if !ok {
		writeError(w, r, ErrInvalidArgument)
		return
	}
	sb, err := s.Meta.GetBucket(r.Context(), srcBucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	srcObj, err := s.Meta.GetObject(r.Context(), sb.ID, srcKey, srcVersion)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}

	offset := int64(0)
	length := srcObj.Size
	if rangeSpec := r.Header.Get("x-amz-copy-source-range"); rangeSpec != "" {
		start, end, ok := parseCopySourceRange(rangeSpec, srcObj.Size)
		if !ok {
			writeError(w, r, ErrInvalidArgument)
			return
		}
		offset = start
		length = end - start + 1
	}

	rc, err := s.Data.GetChunks(r.Context(), srcObj.Manifest, offset, length)
	if err != nil {
		writeError(w, r, ErrInternal)
		return
	}
	defer rc.Close()

	// US-004 FlexibleChecksum on multipart copy: when the request carries
	// `x-amz-checksum-algorithm`, recompute the named digest over the
	// streamed copy bytes via TeeReader (no buffering). The result is
	// echoed on the response, validated against any client-supplied
	// `x-amz-checksum-<algo>` value (mismatch → BadDigest), and stored on
	// the multipart_parts row so US-001's PartChunks carries it forward
	// for the COMPOSITE Complete computation.
	checksumAlgo := normalizeChecksumAlgo(r.Header.Get("x-amz-checksum-algorithm"))
	if checksumAlgo == "" && mu.ChecksumAlgorithm != "" {
		checksumAlgo = mu.ChecksumAlgorithm
	}
	var (
		hasher       hash.Hash
		hdrName      string
		clientDigest string
		copySrc      io.Reader = rc
	)
	if checksumAlgo != "" {
		hasher, hdrName = newChecksumHasher(checksumAlgo)
		if hasher != nil {
			copySrc = io.TeeReader(rc, hasher)
			clientDigest = r.Header.Get(hdrName)
		}
	}

	ctx, cancel := context.WithTimeout(data.WithBucketID(r.Context(), b.ID), 10*time.Minute)
	defer cancel()
	manifest, err := s.Data.PutChunks(ctx, copySrc, mu.StorageClass)
	if err != nil {
		writeError(w, r, ErrInternal)
		return
	}

	var partChecksumValue string
	if hasher != nil {
		partChecksumValue = base64.StdEncoding.EncodeToString(hasher.Sum(nil))
		if clientDigest != "" && clientDigest != partChecksumValue {
			_ = s.Data.Delete(r.Context(), manifest)
			writeError(w, r, ErrBadDigest)
			return
		}
	}

	part := &meta.MultipartPart{
		PartNumber:        partNumber,
		ETag:              manifest.ETag,
		Size:              manifest.Size,
		Manifest:          manifest,
		ChecksumValue:     partChecksumValue,
		ChecksumAlgorithm: ifChecksumStored(partChecksumValue, checksumAlgo),
	}
	if err := s.Meta.SavePart(r.Context(), b.ID, uploadID, part); err != nil {
		_ = s.Data.Delete(r.Context(), manifest)
		mapMetaErr(w, r, err)
		return
	}
	if srcObj.VersionID != "" {
		w.Header().Set("x-amz-copy-source-version-id", srcObj.VersionID)
	}
	if partChecksumValue != "" && hdrName != "" {
		w.Header().Set(hdrName, partChecksumValue)
	}
	writeXML(w, http.StatusOK, copyPartResult{
		ETag:         `"` + manifest.ETag + `"`,
		LastModified: time.Now().UTC().Format(time.RFC3339),
	})
}

// ifChecksumStored returns the algo only when a digest was actually
// computed; keeps zero-value rows out of multipart_parts.checksum_algorithm.
func ifChecksumStored(value, algo string) string {
	if value == "" {
		return ""
	}
	return algo
}

func parseCopySourceRange(spec string, size int64) (start, end int64, ok bool) {
	if !strings.HasPrefix(spec, "bytes=") {
		return 0, 0, false
	}
	spec = strings.TrimPrefix(spec, "bytes=")
	lo, hi, has := strings.Cut(spec, "-")
	if !has {
		return 0, 0, false
	}
	var err error
	start, err = strconv.ParseInt(lo, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	if hi == "" {
		end = size - 1
	} else {
		end, err = strconv.ParseInt(hi, 10, 64)
		if err != nil || end < start {
			return 0, 0, false
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true
}

func (s *Server) completeMultipart(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key, uploadID string) {
	// US-008: gate the LWT flip on If-Match / If-None-Match referring to
	// the eventual object's ETag (not the upload ID). Mirrors putObject's
	// shape so a concurrent Complete attempt cannot leak "completing"
	// state — the precondition check happens BEFORE
	// Meta.CompleteMultipartUpload runs.
	if ifMatch := r.Header.Get("If-Match"); ifMatch != "" {
		existing, err := s.Meta.GetObject(r.Context(), b.ID, key, "")
		if err != nil || !etagMatches(ifMatch, `"`+existing.ETag+`"`) {
			writeError(w, r, ErrPreconditionFailed)
			return
		}
	}
	if ifNone := r.Header.Get("If-None-Match"); ifNone != "" {
		existing, err := s.Meta.GetObject(r.Context(), b.ID, key, "")
		if err == nil && (ifNone == "*" || etagMatches(ifNone, `"`+existing.ETag+`"`)) {
			writeError(w, r, ErrPreconditionFailed)
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	var doc completeMultipartBody
	if err := xml.Unmarshal(body, &doc); err != nil {
		writeError(w, r, ErrMalformedXML)
		return
	}
	if len(doc.Parts) == 0 {
		writeError(w, r, ErrMalformedXML)
		return
	}
	parts := make([]meta.CompletePart, 0, len(doc.Parts))
	prev := 0
	for _, p := range doc.Parts {
		if p.PartNumber <= prev {
			writeError(w, r, ErrInvalidPartOrder)
			return
		}
		prev = p.PartNumber
		parts = append(parts, meta.CompletePart{
			PartNumber: p.PartNumber,
			ETag:       strings.Trim(p.ETag, `"`),
		})
	}

	mu, err := s.Meta.GetMultipartUpload(r.Context(), b.ID, uploadID)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}

	finalETag, err := multipartETag(parts)
	if err != nil {
		writeError(w, r, ErrInvalidPart)
		return
	}

	storedParts, err := s.Meta.ListParts(r.Context(), b.ID, uploadID)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	byNumber := make(map[int]*meta.MultipartPart, len(storedParts))
	for _, p := range storedParts {
		byNumber[p.PartNumber] = p
	}

	// US-009 size-too-small: every part except the last must be at least
	// 5 MiB (S3 spec). Validates BEFORE the LWT flip so a small-part
	// Complete cannot leak "completing" state. Single-part uploads exempt
	// (the last part has no minimum). ETag mismatch + missing-part get
	// resolved by the meta layer's existing checks downstream.
	const minPartSize = 5 * 1024 * 1024
	for i, cp := range parts {
		if i == len(parts)-1 {
			break
		}
		p, ok := byNumber[cp.PartNumber]
		if !ok {
			writeError(w, r, ErrInvalidPart)
			return
		}
		if p.Size < minPartSize {
			writeError(w, r, ErrEntityTooSmall)
			return
		}
	}

	// US-003 FlexibleChecksum response shape: when the multipart session
	// was opened with `x-amz-checksum-algorithm`, compute the COMPOSITE
	// hash-of-hashes from the per-part digests captured at UploadPart
	// time, OR adopt the FULL_OBJECT digest the client supplied on the
	// CompleteMultipartUpload request. The result lands on obj.Manifest
	// as a hint for the meta layer (which copies it onto the persisted
	// manifest) and is echoed in the wire response.
	var compositeAlgo, compositeType, compositeValue string
	if mu.ChecksumAlgorithm != "" {
		compositeAlgo = mu.ChecksumAlgorithm
		compositeType = mu.ChecksumType
		if compositeType == "" {
			compositeType = "COMPOSITE"
		}
		switch compositeType {
		case "FULL_OBJECT":
			compositeValue = r.Header.Get(checksumHeader(compositeAlgo))
		case "COMPOSITE":
			pcs := make([]partChecksum, 0, len(parts))
			for _, cp := range parts {
				p, ok := byNumber[cp.PartNumber]
				if !ok {
					writeError(w, r, ErrInvalidPart)
					return
				}
				pcs = append(pcs, partChecksum{algo: p.ChecksumAlgorithm, value: p.ChecksumValue})
			}
			if v, ok := compositeChecksum(compositeAlgo, pcs); ok {
				compositeValue = v
			}
			// US-010: when client supplied `x-amz-checksum-<algo>` on the
			// Complete request, validate the recomputed COMPOSITE against
			// it BEFORE the LWT flip. Mismatch → BadDigest, mirroring
			// per-part validation in uploadPart / uploadPartCopy.
			if client := r.Header.Get(checksumHeader(compositeAlgo)); client != "" {
				if compositeValue == "" || client != compositeValue {
					writeError(w, r, ErrBadDigest)
					return
				}
			}
		}
	}

	obj := &meta.Object{
		BucketID:     b.ID,
		Key:          key,
		ContentType:  mu.ContentType,
		StorageClass: mu.StorageClass,
		ETag:         finalETag,
		Mtime:        time.Now().UTC(),
	}

	// US-010 backend pass-through: finalise the backend's multipart
	// upload and stamp obj.Manifest with the BackendRef-shape result so
	// the meta store skips its own chunks-shape assembly and persists
	// the BackendRef pointer instead.
	if mu.BackendUploadID != "" {
		mb, ok := s.Data.(data.MultipartBackend)
		if !ok {
			writeError(w, r, ErrInternal)
			return
		}
		backendParts := make([]data.BackendCompletedPart, 0, len(parts))
		var totalSize int64
		for _, cp := range parts {
			p, ok := byNumber[cp.PartNumber]
			if !ok {
				writeError(w, r, ErrInvalidPart)
				return
			}
			etag := p.BackendETag
			if etag == "" {
				etag = p.ETag
			}
			backendParts = append(backendParts, data.BackendCompletedPart{
				PartNumber: int32(cp.PartNumber),
				ETag:       etag,
			})
			totalSize += p.Size
		}
		mfst, completeErr := mb.CompleteBackendMultipart(r.Context(), mu.BackendUploadID, backendParts, mu.StorageClass)
		if completeErr != nil {
			writeError(w, r, ErrInternal)
			return
		}
		// Backend's response ETag is authoritative for the stored object
		// — match the object's ETag to it. The backend computes the same
		// composite hash-of-MD5s-suffix as multipartETag for non-SSE-KMS
		// uploads, so the wire response is consistent with what the
		// client computed.
		if mfst.ETag != "" {
			obj.ETag = mfst.ETag
			finalETag = mfst.ETag
		}
		mfst.Size = totalSize
		if mfst.BackendRef != nil {
			mfst.BackendRef.Size = totalSize
		}
		obj.Manifest = mfst
		obj.Size = totalSize
	}

	// US-003 FlexibleChecksum: stamp the composite hints onto obj.Manifest
	// (creating a hints-only manifest in the chunks-shape native path; the
	// meta layer copies the hints over to the manifest it builds).
	if compositeAlgo != "" {
		if obj.Manifest == nil {
			obj.Manifest = &data.Manifest{}
		}
		obj.Manifest.MultipartChecksumAlgorithm = compositeAlgo
		obj.Manifest.MultipartChecksumType = compositeType
		obj.Manifest.MultipartChecksum = compositeValue
	}

	// US-007: Complete on a non-Enabled bucket lands as the literal-"null"
	// version, mirroring putObject. The meta layer recognises the marker
	// and applies the same atomic null-replace invariant on Suspended
	// buckets.
	if b.Versioning != meta.VersioningEnabled {
		obj.VersionID = meta.NullVersionID
	}
	orphans, err := s.Meta.CompleteMultipartUpload(r.Context(), obj, uploadID, parts, meta.IsVersioningActive(b.Versioning))
	if err != nil {
		if errors.Is(err, meta.ErrMultipartPartMissing) || errors.Is(err, meta.ErrMultipartETagMismatch) {
			writeError(w, r, ErrInvalidPart)
			return
		}
		mapMetaErr(w, r, err)
		return
	}
	for _, m := range orphans {
		if m != nil {
			s.enqueueOrphan(r.Context(), m)
		}
	}

	// US-008: surface the materialised version-id when the bucket is
	// versioned (Enabled emits the UUID, Suspended emits the literal
	// "null" — both observable on the AWS wire).
	if meta.IsVersioningActive(b.Versioning) && obj.VersionID != "" {
		w.Header().Set("x-amz-version-id", obj.VersionID)
	}
	resp := completeMultipartResult{
		Location: fmt.Sprintf("/%s/%s", b.Name, key),
		Bucket:   b.Name,
		Key:      key,
		ETag:     `"` + finalETag + `"`,
	}
	if compositeAlgo != "" && compositeValue != "" {
		switch compositeAlgo {
		case "CRC32":
			resp.ChecksumCRC32 = compositeValue
		case "CRC32C":
			resp.ChecksumCRC32C = compositeValue
		case "SHA1":
			resp.ChecksumSHA1 = compositeValue
		case "SHA256":
			resp.ChecksumSHA256 = compositeValue
		}
		resp.ChecksumType = compositeType
	}
	writeXML(w, http.StatusOK, resp)
}

func (s *Server) abortMultipart(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key, uploadID string) {
	// Capture the backend handle BEFORE clearing meta state so the backend
	// session can be aborted even if it doesn't surface in the part-
	// manifest list (US-010 pass-through parts have no chunks-shape
	// manifest to enqueue).
	var backendHandle string
	if mu, getErr := s.Meta.GetMultipartUpload(r.Context(), b.ID, uploadID); getErr == nil {
		backendHandle = mu.BackendUploadID
	}
	manifests, err := s.Meta.AbortMultipartUpload(r.Context(), b.ID, uploadID)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	for _, m := range manifests {
		if m != nil {
			s.enqueueOrphan(r.Context(), m)
		}
	}
	if backendHandle != "" {
		if mb, ok := s.Data.(data.MultipartBackend); ok {
			_ = mb.AbortBackendMultipart(r.Context(), backendHandle)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listParts(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key, uploadID string) {
	mu, err := s.Meta.GetMultipartUpload(r.Context(), b.ID, uploadID)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	parts, err := s.Meta.ListParts(r.Context(), b.ID, uploadID)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })
	resp := listPartsResult{
		Bucket:       b.Name,
		Key:          key,
		UploadID:     uploadID,
		StorageClass: mu.StorageClass,
		MaxParts:     1000,
		IsTruncated:  false,
	}
	for _, p := range parts {
		resp.Parts = append(resp.Parts, partEntry{
			PartNumber:   p.PartNumber,
			LastModified: p.Mtime.UTC().Format(time.RFC3339),
			ETag:         `"` + p.ETag + `"`,
			Size:         p.Size,
		})
	}
	writeXML(w, http.StatusOK, resp)
}

func (s *Server) listMultipartUploads(w http.ResponseWriter, r *http.Request, b *meta.Bucket) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("max-uploads"))
	if limit <= 0 {
		limit = 1000
	}
	ups, err := s.Meta.ListMultipartUploads(r.Context(), b.ID, q.Get("prefix"), limit)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	resp := listUploadsResult{
		Bucket:     b.Name,
		Prefix:     q.Get("prefix"),
		MaxUploads: limit,
	}
	for _, u := range ups {
		resp.Uploads = append(resp.Uploads, uploadEntry{
			Key:          u.Key,
			UploadID:     u.UploadID,
			Initiated:    u.InitiatedAt.UTC().Format(time.RFC3339),
			StorageClass: u.StorageClass,
		})
	}
	writeXML(w, http.StatusOK, resp)
}

func multipartETag(parts []meta.CompletePart) (string, error) {
	h := md5.New()
	for _, p := range parts {
		b, err := hex.DecodeString(p.ETag)
		if err != nil {
			return "", err
		}
		if _, err := h.Write(b); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)) + "-" + strconv.Itoa(len(parts)), nil
}

var _ = data.DefaultChunkSize
