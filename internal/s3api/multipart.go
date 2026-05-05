package s3api

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
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

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/crypto/kms"
	"github.com/danchupin/strata/internal/crypto/master"
	ssecrypto "github.com/danchupin/strata/internal/crypto/sse"
	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/metrics"
)

const multipartCompletionTTL = 10 * time.Minute

// multipartMinPartSize is AWS S3's 5 MiB lower bound on every multipart
// part except the last. Violations on Complete return EntityTooSmall.
// Declared as var (not const) so tests that exercise other multipart
// surfaces with smaller bodies can lower it via export_test.go.
var multipartMinPartSize int64 = 5 * 1024 * 1024

func (s *Server) initiateMultipart(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string) {
	class := r.Header.Get("x-amz-storage-class")
	if class == "" {
		class = b.DefaultClass
	}
	sse, kmsKeyID, sseErr, sseOK := s.resolveSSEWithKey(r, b)
	if !sseOK {
		writeError(w, r, sseErr)
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
		SSE:               sse,
		UserMeta:          extractUserMeta(r.Header),
		CacheControl:      r.Header.Get("Cache-Control"),
		Expires:           r.Header.Get("Expires"),
		ChecksumAlgorithm: strings.ToUpper(r.Header.Get("x-amz-checksum-algorithm")),
		ChecksumType:      normalizeChecksumType(r.Header.Get("x-amz-checksum-type")),
	}
	switch sse {
	case sseAlgorithmAES256:
		if s.Master == nil {
			writeError(w, r, ErrInternal)
			return
		}
		mk, mid, merr := s.Master.Resolve(r.Context())
		if merr != nil {
			writeError(w, r, ErrInternal)
			return
		}
		dek := make([]byte, ssecrypto.KeySize)
		if _, rerr := rand.Read(dek); rerr != nil {
			writeError(w, r, ErrInternal)
			return
		}
		wrapped, werr := ssecrypto.WrapDEK(mk, dek)
		if werr != nil {
			writeError(w, r, ErrInternal)
			return
		}
		mu.SSEKey = wrapped
		mu.SSEKeyID = mid
	case sseAlgorithmAWSKMS:
		if s.KMS == nil {
			writeError(w, r, ErrInternal)
			return
		}
		_, wrapped, kerr := s.KMS.GenerateDataKey(r.Context(), kmsKeyID)
		if kerr != nil {
			writeError(w, r, ErrInternal)
			return
		}
		mu.SSEKey = wrapped
		mu.SSEKeyID = kmsKeyID
	}
	if err := s.Meta.CreateMultipartUpload(r.Context(), mu); err != nil {
		mapMetaErr(w, r, err)
		return
	}
	metrics.MultipartActive.WithLabelValues(b.Name).Inc()
	if sse != "" {
		w.Header().Set("x-amz-server-side-encryption", sse)
	}
	if sse == sseAlgorithmAWSKMS && mu.SSEKeyID != "" {
		w.Header().Set(hdrSSEKMSKeyID, mu.SSEKeyID)
	}
	if mu.ChecksumAlgorithm != "" {
		w.Header().Set("x-amz-checksum-algorithm", mu.ChecksumAlgorithm)
	}
	if mu.ChecksumType != "" {
		w.Header().Set("x-amz-checksum-type", mu.ChecksumType)
	}
	writeXML(w, http.StatusOK, initiateMultipartResult{
		Bucket:            b.Name,
		Key:               key,
		UploadID:          mu.UploadID,
		ChecksumAlgorithm: mu.ChecksumAlgorithm,
		ChecksumType:      mu.ChecksumType,
	})
}

// normalizeChecksumType maps the request x-amz-checksum-type header value to
// the canonical "FULL_OBJECT" or "COMPOSITE" tokens AWS persists. Unknown
// values are dropped (treated as no preference) so a malformed header does not
// cause Initiate to fail; Complete then defaults to COMPOSITE.
func normalizeChecksumType(v string) string {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "FULL_OBJECT":
		return "FULL_OBJECT"
	case "COMPOSITE":
		return "COMPOSITE"
	}
	return ""
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
	var encReader *sseEncryptingReader
	switch mu.SSE {
	case sseAlgorithmAES256:
		if s.Master == nil || len(mu.SSEKey) == 0 {
			writeError(w, r, ErrInternal)
			return
		}
		mk, merr := master.ResolveByID(ctx, s.Master, mu.SSEKeyID)
		if merr != nil {
			writeError(w, r, ErrInternal)
			return
		}
		dek, uerr := ssecrypto.UnwrapDEK(mk, mu.SSEKey)
		if uerr != nil {
			writeError(w, r, ErrInternal)
			return
		}
		encReader = newSSEEncryptingReader(body, dek, multipartPartOID(key, partNumber))
		body = encReader
	case sseAlgorithmAWSKMS:
		if s.KMS == nil || len(mu.SSEKey) == 0 || mu.SSEKeyID == "" {
			writeError(w, r, ErrInternal)
			return
		}
		dek, uerr := s.KMS.UnwrapDEK(ctx, mu.SSEKeyID, mu.SSEKey)
		if errors.Is(uerr, kms.ErrKeyIDMismatch) {
			writeError(w, r, ErrKMSAccessDenied)
			return
		}
		if uerr != nil {
			writeError(w, r, ErrInternal)
			return
		}
		encReader = newSSEEncryptingReader(body, dek, multipartPartOID(key, partNumber))
		body = encReader
	}
	manifest, err := s.Data.PutChunks(ctx, body, mu.StorageClass)
	if err != nil {
		if errors.Is(err, auth.ErrSignatureInvalid) {
			writeError(w, r, ErrSignatureDoesNotMatch)
			return
		}
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
	partETag := manifest.ETag
	partSize := manifest.Size
	if encReader != nil {
		partETag = encReader.PlaintextETag()
		partSize = encReader.PlaintextSize()
	}
	part := &meta.MultipartPart{
		PartNumber: partNumber,
		ETag:       partETag,
		Size:       partSize,
		Manifest:   manifest,
		Checksums:  sums,
	}
	if err := s.Meta.SavePart(r.Context(), b.ID, uploadID, part); err != nil {
		_ = s.Data.Delete(r.Context(), manifest)
		mapMetaErr(w, r, err)
		return
	}
	w.Header().Set("ETag", `"`+partETag+`"`)
	writeChecksumHeaders(w.Header(), sums)
	if mu.SSE != "" {
		w.Header().Set("x-amz-server-side-encryption", mu.SSE)
	}
	w.WriteHeader(http.StatusOK)
}

type copyPartResult struct {
	XMLName           xml.Name `xml:"CopyPartResult"`
	ETag              string   `xml:"ETag"`
	LastModified      string   `xml:"LastModified"`
	ChecksumCRC32     string   `xml:"ChecksumCRC32,omitempty"`
	ChecksumCRC32C    string   `xml:"ChecksumCRC32C,omitempty"`
	ChecksumSHA1      string   `xml:"ChecksumSHA1,omitempty"`
	ChecksumSHA256    string   `xml:"ChecksumSHA256,omitempty"`
	ChecksumCRC64NVME string   `xml:"ChecksumCRC64NVME,omitempty"`
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
			writeError(w, r, ErrInvalidRange)
			return
		}
		offset = start
		length = end - start + 1
	}

	algo := strings.ToUpper(strings.TrimSpace(r.Header.Get("x-amz-checksum-algorithm")))
	var hasher hash.Hash
	if algo != "" {
		h, herr := newChecksumHasher(algo)
		if herr != nil {
			writeError(w, r, ErrInvalidArgument)
			return
		}
		hasher = h
	}
	expected := ""
	if algo != "" {
		expected = r.Header.Get("x-amz-checksum-" + strings.ToLower(algo))
	}

	rc, err := s.Data.GetChunks(r.Context(), srcObj.Manifest, offset, length)
	if err != nil {
		writeError(w, r, ErrInternal)
		return
	}
	defer rc.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	body := io.Reader(rc)
	if hasher != nil {
		body = io.TeeReader(rc, hasher)
	}
	manifest, err := s.Data.PutChunks(ctx, body, mu.StorageClass)
	if err != nil {
		writeError(w, r, ErrInternal)
		return
	}
	var sums map[string]string
	if hasher != nil {
		got := base64.StdEncoding.EncodeToString(hasher.Sum(nil))
		if expected != "" && got != expected {
			_ = s.Data.Delete(r.Context(), manifest)
			writeError(w, r, ErrBadDigest)
			return
		}
		sums = map[string]string{algo: got}
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
	if srcObj.VersionID != "" {
		w.Header().Set("x-amz-copy-source-version-id", srcObj.VersionID)
	}
	w.Header().Set("ETag", `"`+manifest.ETag+`"`)
	writeChecksumHeaders(w.Header(), sums)
	writeXML(w, http.StatusOK, copyPartResult{
		ETag:              `"` + manifest.ETag + `"`,
		LastModified:      time.Now().UTC().Format(time.RFC3339),
		ChecksumCRC32:     sums["CRC32"],
		ChecksumCRC32C:    sums["CRC32C"],
		ChecksumSHA1:      sums["SHA1"],
		ChecksumSHA256:    sums["SHA256"],
		ChecksumCRC64NVME: sums["CRC64NVME"],
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
		if err != nil || end < start || end >= size {
			return 0, 0, false
		}
	}
	return start, end, true
}

func (s *Server) completeMultipart(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key, uploadID string) {
	if cached, err := s.Meta.GetMultipartCompletion(r.Context(), b.ID, uploadID); err == nil && cached != nil {
		writeCachedCompletion(w, cached)
		return
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

	if ifMatch := r.Header.Get("If-Match"); ifMatch != "" {
		existing, gerr := s.Meta.GetObject(r.Context(), b.ID, key, "")
		if gerr != nil || !etagMatches(ifMatch, `"`+existing.ETag+`"`) {
			writeError(w, r, ErrPreconditionFailed)
			return
		}
	}
	if ifNone := r.Header.Get("If-None-Match"); ifNone != "" {
		existing, gerr := s.Meta.GetObject(r.Context(), b.ID, key, "")
		if gerr == nil && (ifNone == "*" || etagMatches(ifNone, `"`+existing.ETag+`"`)) {
			writeError(w, r, ErrPreconditionFailed)
			return
		}
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
	for i, p := range parts {
		sp, ok := byNumber[p.PartNumber]
		if !ok {
			writeError(w, r, ErrInvalidPart)
			return
		}
		if i < len(parts)-1 && sp.Size < multipartMinPartSize {
			writeError(w, r, ErrEntityTooSmall)
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

	checksumType := ""
	switch {
	case mu.ChecksumType != "":
		checksumType = mu.ChecksumType
	case len(composite) > 0:
		checksumType = "COMPOSITE"
	}

	obj := &meta.Object{
		BucketID:     b.ID,
		Key:          key,
		ContentType:  mu.ContentType,
		StorageClass: mu.StorageClass,
		ETag:         finalETag,
		Mtime:        time.Now().UTC(),
		Checksums:    composite,
		ChecksumType: checksumType,
		SSE:          mu.SSE,
		SSEKey:       mu.SSEKey,
		SSEKeyID:     mu.SSEKeyID,
		UserMeta:     mu.UserMeta,
		CacheControl: mu.CacheControl,
		Expires:      mu.Expires,
		PartsCount:   len(parts),
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
	metrics.MultipartActive.WithLabelValues(b.Name).Dec()
	for _, m := range orphans {
		if m != nil {
			s.enqueueChunks(r.Context(), m.Chunks)
		}
	}

	headers := map[string]string{}
	for algo, val := range composite {
		if val != "" {
			headers["x-amz-checksum-"+strings.ToLower(algo)] = val
		}
	}
	if obj.SSE != "" {
		headers["x-amz-server-side-encryption"] = obj.SSE
	}
	if obj.SSE == sseAlgorithmAWSKMS && obj.SSEKeyID != "" {
		headers[hdrSSEKMSKeyID] = obj.SSEKeyID
	}
	if meta.IsVersioningActive(b.Versioning) && obj.VersionID != "" {
		headers["x-amz-version-id"] = wireVersionID(obj)
	}

	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	if err := xml.NewEncoder(&buf).Encode(completeMultipartResult{
		Location:          fmt.Sprintf("/%s/%s", b.Name, key),
		Bucket:            b.Name,
		Key:               key,
		ETag:              `"` + finalETag + `"`,
		ChecksumCRC32:     composite["CRC32"],
		ChecksumCRC32C:    composite["CRC32C"],
		ChecksumSHA1:      composite["SHA1"],
		ChecksumSHA256:    composite["SHA256"],
		ChecksumCRC64NVME: composite["CRC64NVME"],
		ChecksumType:      checksumType,
	}); err != nil {
		writeError(w, r, ErrInternal)
		return
	}
	if checksumType != "" {
		w.Header().Set("x-amz-checksum-type", checksumType)
	}

	rec := &meta.MultipartCompletion{
		BucketID:    b.ID,
		UploadID:    uploadID,
		Key:         key,
		ETag:        finalETag,
		VersionID:   obj.VersionID,
		Body:        buf.Bytes(),
		Headers:     headers,
		CompletedAt: time.Now().UTC(),
	}
	_ = s.Meta.RecordMultipartCompletion(r.Context(), rec, multipartCompletionTTL)

	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/xml")
	if status := s.emitReplicationEvent(r, b, replicationEventDetails{
		EventName: "s3:ObjectCreated:CompleteMultipartUpload",
		Key:       key,
		VersionID: obj.VersionID,
		Tags:      obj.Tags,
	}); status != "" {
		w.Header().Set("x-amz-replication-status", status)
	}
	s.emitNotificationEvent(r, b, notificationEventDetails{
		EventName: "s3:ObjectCreated:CompleteMultipartUpload",
		Key:       key,
		Size:      obj.Size,
		ETag:      obj.ETag,
		VersionID: obj.VersionID,
		SourceIP:  clientSourceIP(r),
		Principal: principalFromContext(r),
	})
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// partOffsetLength returns the (offset, length) byte range covering part
// `partNumber` in an object whose plaintext part sizes are listed in
// `partSizes`. partNumber is 1-indexed; caller must already have validated
// 1 <= partNumber <= len(partSizes).
func partOffsetLength(partSizes []int64, partNumber int) (offset, length int64) {
	for i := 0; i < partNumber-1; i++ {
		offset += partSizes[i]
	}
	return offset, partSizes[partNumber-1]
}

// partChecksumsAt returns the per-part stored checksum map for index `i`
// (0-based) from an object's manifest, or nil if not populated.
func partChecksumsAt(o *meta.Object, i int) map[string]string {
	if o.Manifest == nil {
		return nil
	}
	pc := o.Manifest.PartChecksums
	if i < 0 || i >= len(pc) {
		return nil
	}
	return pc[i]
}

func writeCachedCompletion(w http.ResponseWriter, rec *meta.MultipartCompletion) {
	for k, v := range rec.Headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rec.Body)
}

func (s *Server) abortMultipart(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key, uploadID string) {
	manifests, err := s.Meta.AbortMultipartUpload(r.Context(), b.ID, uploadID)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	metrics.MultipartActive.WithLabelValues(b.Name).Dec()
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
