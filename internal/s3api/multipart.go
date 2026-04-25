package s3api

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
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
	sse, sseErr, sseOK := s.resolveSSE(r, b)
	if !sseOK {
		writeError(w, r, sseErr)
		return
	}
	mu := &meta.MultipartUpload{
		BucketID:     b.ID,
		UploadID:     gocql.TimeUUID().String(),
		Key:          key,
		StorageClass: class,
		ContentType:  r.Header.Get("Content-Type"),
		InitiatedAt:  time.Now().UTC(),
		Status:       "uploading",
		SSE:          sse,
	}
	if err := s.Meta.CreateMultipartUpload(r.Context(), mu); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if sse != "" {
		w.Header().Set("x-amz-server-side-encryption", sse)
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
	checksumEntries, cerr := parseRequestChecksums(r)
	if cerr != nil {
		writeError(w, r, ErrInvalidArgument)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	body := io.Reader(r.Body)
	if len(checksumEntries) > 0 {
		body = io.TeeReader(r.Body, checksumWriter(checksumEntries))
	}
	manifest, err := s.Data.PutChunks(ctx, body, mu.StorageClass)
	if err != nil {
		if strings.Contains(err.Error(), "unknown storage class") {
			writeError(w, r, ErrInvalidStorageClass)
			return
		}
		writeError(w, r, ErrInternal)
		return
	}
	sums, verr := verifyChecksums(checksumEntries)
	if verr != nil {
		_ = s.Data.Delete(r.Context(), manifest)
		writeError(w, r, ErrBadDigest)
		return
	}
	part := &meta.MultipartPart{
		PartNumber: partNumber,
		ETag:       manifest.ETag,
		Size:       manifest.Size,
		Manifest:   manifest,
		Checksums:  sums,
	}
	if err := s.Meta.SavePart(r.Context(), b.ID, uploadID, part); err != nil {
		_ = s.Data.Delete(r.Context(), manifest)
		mapMetaErr(w, r, err)
		return
	}
	w.Header().Set("ETag", `"`+manifest.ETag+`"`)
	writeChecksumHeaders(w.Header(), sums)
	if mu.SSE != "" {
		w.Header().Set("x-amz-server-side-encryption", mu.SSE)
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

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	manifest, err := s.Data.PutChunks(ctx, rc, mu.StorageClass)
	if err != nil {
		writeError(w, r, ErrInternal)
		return
	}
	part := &meta.MultipartPart{
		PartNumber: partNumber,
		ETag:       manifest.ETag,
		Size:       manifest.Size,
		Manifest:   manifest,
	}
	if err := s.Meta.SavePart(r.Context(), b.ID, uploadID, part); err != nil {
		_ = s.Data.Delete(r.Context(), manifest)
		mapMetaErr(w, r, err)
		return
	}
	if srcObj.VersionID != "" {
		w.Header().Set("x-amz-copy-source-version-id", srcObj.VersionID)
	}
	writeXML(w, http.StatusOK, copyPartResult{
		ETag:         `"` + manifest.ETag + `"`,
		LastModified: time.Now().UTC().Format(time.RFC3339),
	})
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

	storedParts, err := s.Meta.ListParts(r.Context(), b.ID, uploadID)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	byNumber := make(map[int]*meta.MultipartPart, len(storedParts))
	for _, p := range storedParts {
		byNumber[p.PartNumber] = p
	}
	requested := make([]*checksumPart, 0, len(parts))
	for _, p := range parts {
		sp, ok := byNumber[p.PartNumber]
		if !ok {
			writeError(w, r, ErrInvalidPart)
			return
		}
		requested = append(requested, &checksumPart{Checksums: sp.Checksums})
	}
	composite := composeMultipartChecksums(requested)

	finalETag, err := multipartETag(parts)
	if err != nil {
		writeError(w, r, ErrInvalidPart)
		return
	}

	obj := &meta.Object{
		BucketID:     b.ID,
		Key:          key,
		ContentType:  mu.ContentType,
		StorageClass: mu.StorageClass,
		ETag:         finalETag,
		Mtime:        time.Now().UTC(),
		Checksums:    composite,
		SSE:          mu.SSE,
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
			s.enqueueChunks(r.Context(), m.Chunks)
		}
	}

	writeChecksumHeaders(w.Header(), composite)
	if obj.SSE != "" {
		w.Header().Set("x-amz-server-side-encryption", obj.SSE)
	}
	writeXML(w, http.StatusOK, completeMultipartResult{
		Location:          fmt.Sprintf("/%s/%s", b.Name, key),
		Bucket:            b.Name,
		Key:               key,
		ETag:              `"` + finalETag + `"`,
		ChecksumCRC32:     composite["CRC32"],
		ChecksumCRC32C:    composite["CRC32C"],
		ChecksumSHA1:      composite["SHA1"],
		ChecksumSHA256:    composite["SHA256"],
		ChecksumCRC64NVME: composite["CRC64NVME"],
	})
}

func (s *Server) abortMultipart(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key, uploadID string) {
	manifests, err := s.Meta.AbortMultipartUpload(r.Context(), b.ID, uploadID)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	for _, m := range manifests {
		if m != nil {
			s.enqueueChunks(r.Context(), m.Chunks)
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
