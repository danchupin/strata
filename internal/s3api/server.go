package s3api

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/metrics"
)

func metricsGCEnqueued(n int) {
	for i := 0; i < n; i++ {
		metrics.GCEnqueued.Inc()
	}
}

type Server struct {
	Data   data.Backend
	Meta   meta.Store
	Region string
	// InvalidateCredential, when set, drops a cached credential lookup so
	// changes made via IAM admin endpoints (DeleteAccessKey, US-007) take
	// effect on the next signed request. Typically wired to MultiStore.Invalidate.
	InvalidateCredential func(accessKey string)
	// STS, when set, enables the ?Action=AssumeRole endpoint and is the
	// backing store for temporary credentials.
	STS *auth.STSStore
}

func New(d data.Backend, m meta.Store) *Server {
	return &Server{Data: d, Meta: m, Region: "default"}
}

func (s *Server) enqueueChunks(ctx context.Context, chunks []data.ChunkRef) {
	if len(chunks) == 0 {
		return
	}
	region := s.Region
	if region == "" {
		region = "default"
	}
	if err := s.Meta.EnqueueChunkDeletion(ctx, region, chunks); err != nil {
		_ = s.Data.Delete(ctx, &data.Manifest{Chunks: chunks})
		return
	}
	metricsGCEnqueued(len(chunks))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bucket, key := splitPath(r.URL.Path)

	switch {
	case bucket == "":
		if action := extractIAMAction(r); action != "" {
			s.handleIAM(w, r, action)
			return
		}
		if r.Method == http.MethodGet {
			s.listBuckets(w, r)
			return
		}
	case key == "":
		s.handleBucket(w, r, bucket)
		return
	default:
		s.handleObject(w, r, bucket, key)
		return
	}
	writeError(w, r, ErrNotImplemented)
}

func splitPath(p string) (bucket, key string) {
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", ""
	}
	if i := strings.Index(p, "/"); i >= 0 {
		return p[:i], p[i+1:]
	}
	return p, ""
}

func (s *Server) listBuckets(w http.ResponseWriter, r *http.Request) {
	buckets, err := s.Meta.ListBuckets(r.Context(), "")
	if err != nil {
		writeError(w, r, ErrInternal)
		return
	}
	resp := listAllMyBucketsResult{Owner: owner{ID: "strata", DisplayName: "strata"}}
	for _, b := range buckets {
		resp.Buckets.Bucket = append(resp.Buckets.Bucket, bucketEntry{
			Name:         b.Name,
			CreationDate: b.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeXML(w, http.StatusOK, resp)
}

func (s *Server) handleBucket(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	if r.Method == http.MethodOptions {
		s.corsPreflight(w, r, bucket)
		return
	}
	if q.Has("delete") && r.Method == http.MethodPost {
		s.deleteObjects(w, r, bucket)
		return
	}
	if q.Has("cors") {
		switch r.Method {
		case http.MethodGet:
			s.getBucketCORS(w, r, bucket)
			return
		case http.MethodPut:
			s.putBucketCORS(w, r, bucket)
			return
		case http.MethodDelete:
			s.deleteBucketCORS(w, r, bucket)
			return
		}
	}
	if q.Has("policy") {
		switch r.Method {
		case http.MethodGet:
			s.getBucketPolicy(w, r, bucket)
			return
		case http.MethodPut:
			s.putBucketPolicy(w, r, bucket)
			return
		case http.MethodDelete:
			s.deleteBucketPolicy(w, r, bucket)
			return
		}
	}
	if q.Has("publicAccessBlock") {
		switch r.Method {
		case http.MethodGet:
			s.getBucketPublicAccessBlock(w, r, bucket)
			return
		case http.MethodPut:
			s.putBucketPublicAccessBlock(w, r, bucket)
			return
		case http.MethodDelete:
			s.deleteBucketPublicAccessBlock(w, r, bucket)
			return
		}
	}
	if q.Has("ownershipControls") {
		switch r.Method {
		case http.MethodGet:
			s.getBucketOwnershipControls(w, r, bucket)
			return
		case http.MethodPut:
			s.putBucketOwnershipControls(w, r, bucket)
			return
		case http.MethodDelete:
			s.deleteBucketOwnershipControls(w, r, bucket)
			return
		}
	}
	if q.Has("acl") {
		switch r.Method {
		case http.MethodGet:
			s.getBucketACL(w, r, bucket)
			return
		case http.MethodPut:
			s.putBucketACL(w, r, bucket)
			return
		}
	}
	if q.Has("uploads") && r.Method == http.MethodGet {
		b, err := s.Meta.GetBucket(r.Context(), bucket)
		if err != nil {
			mapMetaErr(w, r, err)
			return
		}
		s.listMultipartUploads(w, r, b)
		return
	}
	if q.Has("versioning") {
		switch r.Method {
		case http.MethodGet:
			s.getBucketVersioning(w, r, bucket)
			return
		case http.MethodPut:
			s.putBucketVersioning(w, r, bucket)
			return
		}
	}
	if q.Has("versions") && r.Method == http.MethodGet {
		s.listObjectVersions(w, r, bucket)
		return
	}
	if q.Has("lifecycle") {
		switch r.Method {
		case http.MethodGet:
			s.getBucketLifecycle(w, r, bucket)
			return
		case http.MethodPut:
			s.putBucketLifecycle(w, r, bucket)
			return
		case http.MethodDelete:
			s.deleteBucketLifecycle(w, r, bucket)
			return
		}
	}
	switch r.Method {
	case http.MethodPut:
		if !validBucketName(bucket) {
			writeError(w, r, ErrInvalidBucketName)
			return
		}
		owner := auth.FromContext(r.Context()).Owner
		_, err := s.Meta.CreateBucket(r.Context(), bucket, owner, "STANDARD")
		if errors.Is(err, meta.ErrBucketAlreadyExists) {
			writeError(w, r, ErrBucketExists)
			return
		}
		if err != nil {
			writeError(w, r, ErrInternal)
			return
		}
		if aclHdr := r.Header.Get("x-amz-acl"); aclHdr != "" {
			_ = s.Meta.SetBucketACL(r.Context(), bucket, normalizeCannedACL(aclHdr))
		}
		w.Header().Set("Location", "/"+bucket)
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		if err := s.Meta.DeleteBucket(r.Context(), bucket); err != nil {
			mapMetaErr(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodHead:
		if _, err := s.Meta.GetBucket(r.Context(), bucket); err != nil {
			mapMetaErr(w, r, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		s.listObjects(w, r, bucket)
	default:
		writeError(w, r, ErrNotImplemented)
	}
}

func (s *Server) listObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	q := r.URL.Query()
	limit := 1000
	if raw := q.Get("max-keys"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			writeError(w, r, ErrInvalidArgument)
			return
		}
		if v > 0 {
			limit = v
		}
	}
	marker := q.Get("continuation-token")
	if marker == "" {
		marker = q.Get("start-after")
	}
	opts := meta.ListOptions{
		Prefix:    q.Get("prefix"),
		Delimiter: q.Get("delimiter"),
		Marker:    marker,
		Limit:     limit,
	}
	res, err := s.Meta.ListObjects(r.Context(), b.ID, opts)
	if err != nil {
		writeError(w, r, ErrInternal)
		return
	}
	resp := listBucketResultV2{
		Name:                  bucket,
		Prefix:                opts.Prefix,
		Delimiter:             opts.Delimiter,
		MaxKeys:               limit,
		IsTruncated:           res.Truncated,
		NextContinuationToken: res.NextMarker,
		ContinuationToken:     q.Get("continuation-token"),
		StartAfter:            q.Get("start-after"),
	}
	for _, o := range res.Objects {
		resp.Contents = append(resp.Contents, objectEntry{
			Key:          o.Key,
			LastModified: o.Mtime.UTC().Format(time.RFC3339),
			ETag:         `"` + o.ETag + `"`,
			Size:         o.Size,
			StorageClass: o.StorageClass,
		})
	}
	for _, p := range res.CommonPrefixes {
		resp.CommonPrefixes = append(resp.CommonPrefixes, commonPrefixEl{Prefix: p})
	}
	resp.KeyCount = len(resp.Contents) + len(resp.CommonPrefixes)
	writeXML(w, http.StatusOK, resp)
}

func (s *Server) handleObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if r.Method == http.MethodOptions {
		s.corsPreflight(w, r, bucket)
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	q := r.URL.Query()

	if q.Has("uploads") && r.Method == http.MethodPost {
		if !s.requireObjectAccess(w, r, b, key, "s3:PutObject") {
			return
		}
		s.initiateMultipart(w, r, b, key)
		return
	}
	if q.Has("acl") {
		switch r.Method {
		case http.MethodGet:
			s.getObjectACL(w, r, b, key)
			return
		case http.MethodPut:
			s.putObjectACL(w, r, b, key)
			return
		}
	}
	if q.Has("tagging") {
		switch r.Method {
		case http.MethodGet:
			s.getObjectTagging(w, r, b, key)
			return
		case http.MethodPut:
			s.putObjectTagging(w, r, b, key)
			return
		case http.MethodDelete:
			s.deleteObjectTagging(w, r, b, key)
			return
		}
	}
	if q.Has("retention") {
		switch r.Method {
		case http.MethodGet:
			s.getObjectRetention(w, r, b, key)
			return
		case http.MethodPut:
			s.putObjectRetention(w, r, b, key)
			return
		}
	}
	if q.Has("legal-hold") {
		switch r.Method {
		case http.MethodGet:
			s.getObjectLegalHold(w, r, b, key)
			return
		case http.MethodPut:
			s.putObjectLegalHold(w, r, b, key)
			return
		}
	}
	if uploadID := q.Get("uploadId"); uploadID != "" {
		switch r.Method {
		case http.MethodPut:
			if q.Get("partNumber") != "" {
				if !s.requireObjectAccess(w, r, b, key, "s3:PutObject") {
					return
				}
				s.uploadPart(w, r, b, key, uploadID)
				return
			}
		case http.MethodPost:
			if !s.requireObjectAccess(w, r, b, key, "s3:PutObject") {
				return
			}
			s.completeMultipart(w, r, b, key, uploadID)
			return
		case http.MethodDelete:
			if !s.requireObjectAccess(w, r, b, key, "s3:AbortMultipartUpload") {
				return
			}
			s.abortMultipart(w, r, b, key, uploadID)
			return
		case http.MethodGet:
			if !s.requireObjectAccess(w, r, b, key, "s3:ListMultipartUploadParts") {
				return
			}
			s.listParts(w, r, b, key, uploadID)
			return
		}
	}

	switch r.Method {
	case http.MethodPut:
		if r.Header.Get("x-amz-copy-source") != "" {
			if !s.requireObjectAccess(w, r, b, key, "s3:PutObject") {
				return
			}
			s.copyObject(w, r, b, key)
			return
		}
		if !s.requireObjectAccess(w, r, b, key, "s3:PutObject") {
			return
		}
		s.putObject(w, r, b, key)
	case http.MethodGet:
		if !s.requireObjectAccess(w, r, b, key, "s3:GetObject") {
			return
		}
		s.getObject(w, r, b, key, true)
	case http.MethodHead:
		if !s.requireObjectAccess(w, r, b, key, "s3:GetObject") {
			return
		}
		s.getObject(w, r, b, key, false)
	case http.MethodDelete:
		action := "s3:DeleteObject"
		if r.URL.Query().Get("versionId") != "" {
			action = "s3:DeleteObjectVersion"
		}
		if !s.requireObjectAccess(w, r, b, key, action) {
			return
		}
		s.deleteObject(w, r, b, key)
	default:
		writeError(w, r, ErrNotImplemented)
	}
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string) {
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
	class := r.Header.Get("x-amz-storage-class")
	if class == "" {
		class = b.DefaultClass
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	m, err := s.Data.PutChunks(ctx, r.Body, class)
	if err != nil {
		if strings.Contains(err.Error(), "unknown storage class") {
			writeError(w, r, ErrInvalidStorageClass)
			return
		}
		writeError(w, r, ErrInternal)
		return
	}
	obj := &meta.Object{
		BucketID:     b.ID,
		Key:          key,
		Size:         m.Size,
		ETag:         m.ETag,
		ContentType:  r.Header.Get("Content-Type"),
		StorageClass: m.Class,
		Mtime:        time.Now().UTC(),
		Manifest:     m,
	}
	if rm := r.Header.Get("x-amz-object-lock-mode"); rm != "" {
		obj.RetainMode = rm
	}
	if ru := r.Header.Get("x-amz-object-lock-retain-until-date"); ru != "" {
		if t, err := time.Parse(time.RFC3339, ru); err == nil {
			obj.RetainUntil = t
		}
	}
	if r.Header.Get("x-amz-object-lock-legal-hold") == "ON" {
		obj.LegalHold = true
	}
	if tagHdr := r.Header.Get("x-amz-tagging"); tagHdr != "" {
		obj.Tags = parseTagHeader(tagHdr)
	}
	if err := s.Meta.PutObject(r.Context(), obj, meta.IsVersioningActive(b.Versioning)); err != nil {
		_ = s.Data.Delete(r.Context(), m)
		mapMetaErr(w, r, err)
		return
	}
	w.Header().Set("ETag", `"`+m.ETag+`"`)
	if meta.IsVersioningActive(b.Versioning) && obj.VersionID != "" {
		w.Header().Set("x-amz-version-id", obj.VersionID)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string, body bool) {
	versionID := r.URL.Query().Get("versionId")
	o, err := s.Meta.GetObject(r.Context(), b.ID, key, versionID)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	if status, ok := checkConditional(r.Header, `"`+o.ETag+`"`, o.Mtime); !ok {
		w.Header().Set("ETag", `"`+o.ETag+`"`)
		w.Header().Set("Last-Modified", o.Mtime.UTC().Format(http.TimeFormat))
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", firstNonEmpty(o.ContentType, "application/octet-stream"))
	w.Header().Set("ETag", `"`+o.ETag+`"`)
	w.Header().Set("Last-Modified", o.Mtime.UTC().Format(http.TimeFormat))
	w.Header().Set("x-amz-storage-class", o.StorageClass)
	w.Header().Set("Accept-Ranges", "bytes")
	if len(o.Tags) > 0 {
		w.Header().Set("x-amz-tagging-count", strconv.Itoa(len(o.Tags)))
	}
	if !o.RetainUntil.IsZero() {
		w.Header().Set("x-amz-object-lock-retain-until-date", o.RetainUntil.UTC().Format(time.RFC3339))
	}
	if o.RetainMode != "" {
		w.Header().Set("x-amz-object-lock-mode", o.RetainMode)
	}
	if o.LegalHold {
		w.Header().Set("x-amz-object-lock-legal-hold", "ON")
	}
	if o.VersionID != "" && meta.IsVersioningActive(b.Versioning) {
		w.Header().Set("x-amz-version-id", o.VersionID)
	}

	offset, length, status, ok := parseRange(r.Header.Get("Range"), o.Size)
	if !ok {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(o.Size, 10))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if status == http.StatusPartialContent {
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(offset, 10)+"-"+strconv.FormatInt(offset+length-1, 10)+"/"+strconv.FormatInt(o.Size, 10))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))

	if !body {
		w.WriteHeader(status)
		return
	}
	rc, err := s.Data.GetChunks(r.Context(), o.Manifest, offset, length)
	if err != nil {
		writeError(w, r, ErrInternal)
		return
	}
	defer rc.Close()
	w.WriteHeader(status)
	_, _ = io.Copy(w, rc)
}

func parseRange(header string, size int64) (offset, length int64, status int, ok bool) {
	if header == "" {
		return 0, size, http.StatusOK, true
	}
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, 0, false
	}
	spec := strings.TrimPrefix(header, "bytes=")
	if strings.Contains(spec, ",") {
		return 0, 0, 0, false
	}
	startStr, endStr, hasDash := strings.Cut(spec, "-")
	if !hasDash {
		return 0, 0, 0, false
	}
	switch {
	case startStr == "" && endStr == "":
		return 0, 0, 0, false
	case startStr == "":
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, n, http.StatusPartialContent, true
	default:
		start, err := strconv.ParseInt(startStr, 10, 64)
		if err != nil || start < 0 || start >= size {
			return 0, 0, 0, false
		}
		var end int64
		if endStr == "" {
			end = size - 1
		} else {
			end, err = strconv.ParseInt(endStr, 10, 64)
			if err != nil || end < start {
				return 0, 0, 0, false
			}
			if end >= size {
				end = size - 1
			}
		}
		return start, end - start + 1, http.StatusPartialContent, true
	}
}

func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string) {
	versionID := r.URL.Query().Get("versionId")
	versioned := meta.IsVersioningActive(b.Versioning)
	bypassGovernance := strings.EqualFold(r.Header.Get("x-amz-bypass-governance-retention"), "true")

	if versionID != "" {
		if existing, err := s.Meta.GetObject(r.Context(), b.ID, key, versionID); err == nil {
			if objectLockBlocksDelete(existing, bypassGovernance) {
				writeError(w, r, ErrObjectLockedErr)
				return
			}
		}
	} else if !versioned {
		if existing, err := s.Meta.GetObject(r.Context(), b.ID, key, ""); err == nil {
			if objectLockBlocksDelete(existing, bypassGovernance) {
				writeError(w, r, ErrObjectLockedErr)
				return
			}
		}
	}

	o, err := s.Meta.DeleteObject(r.Context(), b.ID, key, versionID, versioned)
	if err != nil {
		if errors.Is(err, meta.ErrObjectNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mapMetaErr(w, r, err)
		return
	}
	if versionID != "" && o != nil && o.Manifest != nil {
		s.enqueueChunks(r.Context(), o.Manifest.Chunks)
	}
	if versionID == "" && !versioned && o != nil && o.Manifest != nil {
		s.enqueueChunks(r.Context(), o.Manifest.Chunks)
	}
	if o != nil && o.VersionID != "" && versioned {
		w.Header().Set("x-amz-version-id", o.VersionID)
		if o.IsDeleteMarker {
			w.Header().Set("x-amz-delete-marker", "true")
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func mapMetaErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, meta.ErrBucketNotFound):
		writeError(w, r, ErrNoSuchBucket)
	case errors.Is(err, meta.ErrObjectNotFound):
		writeError(w, r, ErrNoSuchKey)
	case errors.Is(err, meta.ErrBucketNotEmpty):
		writeError(w, r, ErrBucketNotEmpty)
	case errors.Is(err, meta.ErrBucketAlreadyExists):
		writeError(w, r, ErrBucketExists)
	case errors.Is(err, meta.ErrMultipartNotFound), errors.Is(err, meta.ErrMultipartInProgress):
		writeError(w, r, ErrNoSuchUpload)
	case errors.Is(err, meta.ErrMultipartPartMissing), errors.Is(err, meta.ErrMultipartETagMismatch):
		writeError(w, r, ErrInvalidPart)
	default:
		writeError(w, r, ErrInternal)
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func parseTagHeader(h string) map[string]string {
	out := make(map[string]string)
	for _, pair := range strings.Split(h, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		out[k] = v
	}
	return out
}

func writeXML(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(body)
}
