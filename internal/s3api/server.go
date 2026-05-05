package s3api

import (
	"context"
	"crypto/rand"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/crypto/kms"
	"github.com/danchupin/strata/internal/crypto/master"
	ssecrypto "github.com/danchupin/strata/internal/crypto/sse"
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
	// MFASecrets maps an MFA device serial to its raw TOTP secret. Populated
	// from STRATA_MFA_SECRETS at startup; consulted when MFA Delete is enabled
	// on a bucket and the request is a DeleteObjectVersion.
	MFASecrets map[string][]byte
	// MFAClock, when set, is used instead of time.Now for TOTP validation.
	// Tests inject a deterministic clock through this hook.
	MFAClock func() time.Time
	// Master, when set, supplies the SSE-S3 master key used to wrap per-object
	// DEKs. When nil, requests with x-amz-server-side-encryption=AES256 (or a
	// bucket-default that resolves to AES256) return 500 InternalError.
	Master master.Provider
	// KMS, when set, supplies the SSE-KMS provider used to wrap per-object
	// DEKs against a tenant-named key handle. When nil, requests with
	// x-amz-server-side-encryption=aws:kms (or a bucket-default that resolves
	// to aws:kms) return 500 InternalError.
	KMS kms.Provider
	// VHostPatterns lists virtual-hosted-style host patterns of the form
	// "*.<suffix>" (e.g. "*.s3.local"). When the request Host matches any
	// pattern, the prefix becomes the bucket and the request URL.Path is
	// rewritten to "/<bucket>/<original-path>" before path-style routing.
	// Empty disables vhost extraction (path-style only).
	VHostPatterns []string
}

func New(d data.Backend, m meta.Store) *Server {
	return &Server{Data: d, Meta: m, Region: "default"}
}

// enqueueOrphan dispatches abandoned object bytes to the right cleanup path.
// For S3-over-S3 manifests (BackendRef != nil) the gateway issues an immediate
// backend Delete since there is no chunk-level GC. For chunk-based manifests
// the chunks queue into the GC worker via enqueueChunks.
func (s *Server) enqueueOrphan(ctx context.Context, m *data.Manifest) {
	if m == nil {
		return
	}
	if m.BackendRef != nil {
		_ = s.Data.Delete(ctx, m)
		return
	}
	s.enqueueChunks(ctx, m.Chunks)
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
	if alias, ok := extractAccessPointAlias(r.Host); ok {
		ap, err := s.Meta.GetAccessPointByAlias(r.Context(), alias)
		if err != nil {
			if errors.Is(err, meta.ErrAccessPointNotFound) {
				writeError(w, r, ErrNoSuchAccessPoint)
				return
			}
			writeError(w, r, ErrInternal)
			return
		}
		if ap.NetworkOrigin == accessPointOriginVPC {
			if r.Header.Get(accessPointVPCHeader) != ap.VPCID || ap.VPCID == "" {
				writeError(w, r, ErrAccessDenied)
				return
			}
		}
		path := r.URL.Path
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		r.URL.Path = "/" + ap.Bucket + path
		if r.URL.RawPath != "" {
			rawPath := r.URL.RawPath
			if !strings.HasPrefix(rawPath, "/") {
				rawPath = "/" + rawPath
			}
			r.URL.RawPath = "/" + ap.Bucket + rawPath
		}
		r = r.WithContext(withAccessPoint(r.Context(), ap))
	} else if vhostBucket := extractVHostBucket(r.Host, s.VHostPatterns); vhostBucket != "" {
		path := r.URL.Path
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		r.URL.Path = "/" + vhostBucket + path
		if r.URL.RawPath != "" {
			rawPath := r.URL.RawPath
			if !strings.HasPrefix(rawPath, "/") {
				rawPath = "/" + rawPath
			}
			r.URL.RawPath = "/" + vhostBucket + rawPath
		}
	}
	if strings.HasPrefix(r.URL.Path, "/admin/") {
		s.handleAdmin(w, r, strings.TrimPrefix(r.URL.Path, "/admin/"))
		return
	}
	bucket, key := splitPath(r.URL.Path)

	switch {
	case bucket == "":
		if action := extractIAMAction(r); action != "" {
			s.handleIAM(w, r, action)
			return
		}
		if r.URL.Query().Has("notify-dlq") && r.Method == http.MethodGet {
			s.listNotificationDLQ(w, r)
			return
		}
		if r.URL.Query().Has("audit") && r.Method == http.MethodGet {
			s.listAudit(w, r)
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
	info := auth.FromContext(r.Context())
	principal := ""
	if info != nil {
		principal = info.Owner
	}
	resp := listAllMyBucketsResult{Owner: owner{ID: principal, DisplayName: principal}}
	if info == nil || info.IsAnonymous || principal == "" {
		writeXML(w, http.StatusOK, resp)
		return
	}
	buckets, err := s.Meta.ListBuckets(r.Context(), principal)
	if err != nil {
		writeError(w, r, ErrInternal)
		return
	}
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
	if q.Has("encryption") {
		switch r.Method {
		case http.MethodGet:
			s.getBucketEncryption(w, r, bucket)
			return
		case http.MethodPut:
			s.putBucketEncryption(w, r, bucket)
			return
		case http.MethodDelete:
			s.deleteBucketEncryption(w, r, bucket)
			return
		}
	}
	if q.Has("object-lock") {
		switch r.Method {
		case http.MethodGet:
			s.getBucketObjectLockConfig(w, r, bucket)
			return
		case http.MethodPut:
			s.putBucketObjectLockConfig(w, r, bucket)
			return
		}
	}
	if q.Has("notification") {
		switch r.Method {
		case http.MethodGet:
			s.getBucketNotification(w, r, bucket)
			return
		case http.MethodPut:
			s.putBucketNotification(w, r, bucket)
			return
		}
	}
	if q.Has("website") {
		switch r.Method {
		case http.MethodGet:
			s.getBucketWebsite(w, r, bucket)
			return
		case http.MethodPut:
			s.putBucketWebsite(w, r, bucket)
			return
		case http.MethodDelete:
			s.deleteBucketWebsite(w, r, bucket)
			return
		}
	}
	if q.Has("replication") {
		switch r.Method {
		case http.MethodGet:
			s.getBucketReplication(w, r, bucket)
			return
		case http.MethodPut:
			s.putBucketReplication(w, r, bucket)
			return
		case http.MethodDelete:
			s.deleteBucketReplication(w, r, bucket)
			return
		}
	}
	if q.Has("logging") {
		switch r.Method {
		case http.MethodGet:
			s.getBucketLogging(w, r, bucket)
			return
		case http.MethodPut:
			s.putBucketLogging(w, r, bucket)
			return
		}
	}
	if q.Has("tagging") {
		switch r.Method {
		case http.MethodGet:
			s.getBucketTagging(w, r, bucket)
			return
		case http.MethodPut:
			s.putBucketTagging(w, r, bucket)
			return
		case http.MethodDelete:
			s.deleteBucketTagging(w, r, bucket)
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
	if q.Has("location") && r.Method == http.MethodGet {
		s.getBucketLocation(w, r, bucket)
		return
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
	if q.Has("inventory") {
		s.handleBucketInventory(w, r, bucket)
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
		region, regionErr, regionOK := parseCreateBucketLocation(r)
		if !regionOK {
			writeError(w, r, regionErr)
			return
		}
		owner := auth.FromContext(r.Context()).Owner
		_, err := s.Meta.CreateBucket(r.Context(), bucket, owner, "STANDARD")
		if errors.Is(err, meta.ErrBucketAlreadyExists) {
			if existing, gerr := s.Meta.GetBucket(r.Context(), bucket); gerr == nil {
				if existing.Owner != owner {
					writeError(w, r, ErrBucketTaken)
					return
				}
				w.Header().Set("Location", "/"+bucket)
				w.WriteHeader(http.StatusOK)
				return
			}
			writeError(w, r, ErrBucketExists)
			return
		}
		if err != nil {
			writeError(w, r, ErrInternal)
			return
		}
		if region != "" {
			_ = s.Meta.SetBucketRegion(r.Context(), bucket, region)
		}
		if aclHdr := r.Header.Get("x-amz-acl"); aclHdr != "" {
			_ = s.Meta.SetBucketACL(r.Context(), bucket, normalizeCannedACL(aclHdr))
		}
		if strings.EqualFold(r.Header.Get("x-amz-bucket-object-lock-enabled"), "true") {
			_ = s.Meta.SetBucketObjectLockEnabled(r.Context(), bucket, true)
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
		b, err := s.Meta.GetBucket(r.Context(), bucket)
		if err != nil {
			mapMetaErr(w, r, err)
			return
		}
		w.Header().Set("x-amz-bucket-region", s.bucketRegionFor(b))
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if len(q) == 0 {
			b, err := s.Meta.GetBucket(r.Context(), bucket)
			if err == nil && s.serveWebsiteRoot(w, r, b) {
				return
			}
		}
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
	if !s.requireAccess(w, r, b, "s3:ListBucket", bucketARN(b.Name)) {
		return
	}
	if !s.requireACL(w, r, b, "", "s3:ListBucket") {
		return
	}
	q := r.URL.Query()
	v2 := q.Get("list-type") == "2"

	if strings.EqualFold(q.Get("allow-unordered"), "true") && q.Get("delimiter") != "" {
		writeError(w, r, ErrInvalidArgument)
		return
	}

	hasMaxKeys := q.Has("max-keys")
	maxKeys := 1000
	if raw := q.Get("max-keys"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			writeError(w, r, ErrInvalidArgument)
			return
		}
		maxKeys = v
	}

	encodingType := q.Get("encoding-type")
	if encodingType != "" && encodingType != "url" {
		writeError(w, r, ErrInvalidArgument)
		return
	}

	prefix := q.Get("prefix")
	delim := q.Get("delimiter")
	bucketOwner := firstNonEmpty(b.Owner, "strata")

	var marker, contToken, startAfter string
	if v2 {
		contToken = q.Get("continuation-token")
		startAfter = q.Get("start-after")
		marker = contToken
		if marker == "" {
			marker = startAfter
		}
	} else {
		marker = q.Get("marker")
	}

	opts := meta.ListOptions{
		Prefix:    prefix,
		Delimiter: delim,
		Marker:    marker,
		Limit:     maxKeys,
	}

	var res *meta.ListResult
	if hasMaxKeys && maxKeys == 0 {
		res = &meta.ListResult{}
	} else {
		// US-012: prefer the optional RangeScanStore capability when the
		// backend advertises it (memory + TiKV); fall through to the fan-out
		// path for backends that don't (Cassandra).
		if rs, ok := s.Meta.(meta.RangeScanStore); ok {
			res, err = rs.ScanObjects(r.Context(), b.ID, opts)
		} else {
			res, err = s.Meta.ListObjects(r.Context(), b.ID, opts)
		}
		if err != nil {
			writeError(w, r, ErrInternal)
			return
		}
	}

	contents := make([]objectEntry, 0, len(res.Objects))
	for _, o := range res.Objects {
		entry := objectEntry{
			Key:          maybeURLEncode(o.Key, encodingType),
			LastModified: o.Mtime.UTC().Format(time.RFC3339),
			ETag:         `"` + o.ETag + `"`,
			Size:         o.Size,
			StorageClass: o.StorageClass,
		}
		fetchOwner := !v2 || strings.EqualFold(q.Get("fetch-owner"), "true")
		if fetchOwner {
			entry.Owner = &owner{ID: bucketOwner, DisplayName: bucketOwner}
		}
		contents = append(contents, entry)
	}
	commonPrefixes := make([]commonPrefixEl, 0, len(res.CommonPrefixes))
	for _, p := range res.CommonPrefixes {
		commonPrefixes = append(commonPrefixes, commonPrefixEl{Prefix: maybeURLEncode(p, encodingType)})
	}

	if v2 {
		resp := listBucketResultV2{
			Name:                  bucket,
			Prefix:                maybeURLEncode(prefix, encodingType),
			MaxKeys:               maxKeys,
			Delimiter:             delim,
			EncodingType:          encodingType,
			IsTruncated:           res.Truncated,
			ContinuationToken:     contToken,
			NextContinuationToken: res.NextMarker,
			StartAfter:            startAfter,
			Contents:              contents,
			CommonPrefixes:        commonPrefixes,
		}
		resp.KeyCount = len(contents) + len(commonPrefixes)
		writeXML(w, http.StatusOK, resp)
		return
	}

	resp := listBucketResultV1{
		Name:           bucket,
		Prefix:         maybeURLEncode(prefix, encodingType),
		Marker:         marker,
		MaxKeys:        maxKeys,
		Delimiter:      delim,
		EncodingType:   encodingType,
		IsTruncated:    res.Truncated,
		Contents:       contents,
		CommonPrefixes: commonPrefixes,
	}
	if res.Truncated {
		resp.NextMarker = res.NextMarker
	}
	writeXML(w, http.StatusOK, resp)
}

func maybeURLEncode(s, encodingType string) string {
	if encodingType != "url" {
		return s
	}
	return urlPathEncode(s)
}

// urlPathEncode percent-encodes the unreserved-but-special characters that
// appear in object keys, matching what AWS does for encoding-type=url.
func urlPathEncode(s string) string {
	const upperhex = "0123456789ABCDEF"
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z',
			'a' <= c && c <= 'z',
			'0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '/':
			out = append(out, c)
		default:
			out = append(out, '%', upperhex[c>>4], upperhex[c&0x0F])
		}
	}
	return string(out)
}

func (s *Server) handleObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if r.Method == http.MethodOptions {
		s.corsPreflight(w, r, bucket)
		return
	}
	if !validObjectKey(key) {
		writeError(w, r, ErrInvalidURI)
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
	if q.Has("attributes") && r.Method == http.MethodGet {
		if !s.requireObjectAccess(w, r, b, key, "s3:GetObject") {
			return
		}
		s.getObjectAttributes(w, r, b, key)
		return
	}
	if q.Has("restore") && r.Method == http.MethodPost {
		if !s.requireObjectAccess(w, r, b, key, "s3:RestoreObject") {
			return
		}
		s.postObjectRestore(w, r, b, key)
		return
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
		if s.tryWebsiteRedirectAll(w, r, b, key) {
			return
		}
		if s.tryWebsiteRouting(w, r, b, key) {
			return
		}
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
	checksumEntries, cerr := parseRequestChecksums(r)
	if cerr != nil {
		writeError(w, r, ErrInvalidArgument)
		return
	}
	ssec, ssecErr, ssecOK := parseSSECHeaders(r)
	if !ssecOK {
		writeError(w, r, ssecErr)
		return
	}
	var (
		sse      string
		sseKMSID string
	)
	if !ssec.Present {
		var sseErr APIError
		var sseOK bool
		sse, sseKMSID, sseErr, sseOK = s.resolveSSEWithKey(r, b)
		if !sseOK {
			writeError(w, r, sseErr)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	body := io.Reader(r.Body)
	if len(checksumEntries) > 0 {
		body = io.TeeReader(body, checksumWriter(checksumEntries))
	}
	var (
		encReader  *sseEncryptingReader
		wrappedDEK []byte
		sseKeyID   string
	)
	switch sse {
	case sseAlgorithmAES256:
		if s.Master == nil {
			writeError(w, r, ErrInternal)
			return
		}
		mk, mid, merr := s.Master.Resolve(ctx)
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
		wrappedDEK = wrapped
		sseKeyID = mid
		encReader = newSSEEncryptingReader(body, dek, key)
		body = encReader
	case sseAlgorithmAWSKMS:
		if s.KMS == nil {
			writeError(w, r, ErrInternal)
			return
		}
		dek, wrapped, kerr := s.KMS.GenerateDataKey(ctx, sseKMSID)
		if kerr != nil {
			writeError(w, r, ErrInternal)
			return
		}
		if len(dek) != ssecrypto.KeySize {
			writeError(w, r, ErrInternal)
			return
		}
		wrappedDEK = wrapped
		sseKeyID = sseKMSID
		encReader = newSSEEncryptingReader(body, dek, key)
		body = encReader
	}
	m, err := s.Data.PutChunks(ctx, body, class)
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
		_ = s.Data.Delete(r.Context(), m)
		writeError(w, r, ErrBadDigest)
		return
	}
	objSize := m.Size
	objETag := m.ETag
	if encReader != nil {
		objSize = encReader.PlaintextSize()
		objETag = encReader.PlaintextETag()
	}
	obj := &meta.Object{
		BucketID:     b.ID,
		Key:          key,
		Size:         objSize,
		ETag:         objETag,
		ContentType:  r.Header.Get("Content-Type"),
		StorageClass: m.Class,
		Mtime:        time.Now().UTC(),
		Manifest:     m,
		Checksums:    sums,
		SSE:          sse,
		SSEKey:       wrappedDEK,
		SSEKeyID:     sseKeyID,
		UserMeta:     extractUserMeta(r.Header),
		CacheControl: r.Header.Get("Cache-Control"),
		Expires:      r.Header.Get("Expires"),
	}
	if ssec.Present {
		obj.SSECKeyMD5 = ssec.KeyMD5
	}
	if rm := r.Header.Get("x-amz-object-lock-mode"); rm != "" {
		obj.RetainMode = rm
	}
	if ru := r.Header.Get("x-amz-object-lock-retain-until-date"); ru != "" {
		if t, err := time.Parse(time.RFC3339, ru); err == nil {
			obj.RetainUntil = t
		}
	}
	if obj.RetainMode == "" && obj.RetainUntil.IsZero() {
		if mode, until := s.resolveDefaultRetention(r, b); mode != "" {
			obj.RetainMode = mode
			obj.RetainUntil = until
		}
	}
	if r.Header.Get("x-amz-object-lock-legal-hold") == "ON" {
		obj.LegalHold = true
	}
	if tagHdr := r.Header.Get("x-amz-tagging"); tagHdr != "" {
		obj.Tags = parseTagHeader(tagHdr)
	}
	if b.Versioning == meta.VersioningSuspended {
		obj.IsNull = true
	}
	if err := s.Meta.PutObject(r.Context(), obj, meta.IsVersioningActive(b.Versioning)); err != nil {
		_ = s.Data.Delete(r.Context(), m)
		mapMetaErr(w, r, err)
		return
	}
	w.Header().Set("ETag", `"`+obj.ETag+`"`)
	if obj.SSE != "" {
		w.Header().Set("x-amz-server-side-encryption", obj.SSE)
	}
	if obj.SSE == sseAlgorithmAWSKMS && obj.SSEKeyID != "" {
		w.Header().Set(hdrSSEKMSKeyID, obj.SSEKeyID)
	}
	if meta.IsVersioningActive(b.Versioning) && obj.VersionID != "" {
		w.Header().Set("x-amz-version-id", wireVersionID(obj))
	}
	if status := s.emitReplicationEvent(r, b, replicationEventDetails{
		EventName: "s3:ObjectCreated:Put",
		Key:       key,
		VersionID: obj.VersionID,
		Tags:      obj.Tags,
	}); status != "" {
		w.Header().Set("x-amz-replication-status", status)
	}
	s.emitNotificationEvent(r, b, notificationEventDetails{
		EventName: "s3:ObjectCreated:Put",
		Key:       key,
		Size:      obj.Size,
		ETag:      obj.ETag,
		VersionID: obj.VersionID,
		SourceIP:  clientSourceIP(r),
		Principal: principalFromContext(r),
	})
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, b *meta.Bucket, key string, body bool) {
	q := r.URL.Query()
	versionID := q.Get("versionId")
	o, err := s.Meta.GetObject(r.Context(), b.ID, key, versionID)
	if err != nil {
		mapMetaErr(w, r, err)
		return
	}
	partNumberStr := q.Get("partNumber")
	var (
		partNumber int
		partRange  *data.PartRange
		partsTotal int
	)
	if o.Manifest != nil && len(o.Manifest.PartChunks) > 0 {
		partsTotal = len(o.Manifest.PartChunks)
	} else {
		partsTotal = len(o.PartSizes)
	}
	if partNumberStr != "" {
		n, perr := strconv.Atoi(partNumberStr)
		if perr != nil || n < 1 || partsTotal == 0 || n > partsTotal {
			writeError(w, r, ErrInvalidRange)
			return
		}
		partNumber = n
		if o.Manifest != nil && len(o.Manifest.PartChunks) >= n {
			pr := o.Manifest.PartChunks[n-1]
			partRange = &pr
		}
	}
	if o.SSECKeyMD5 != "" {
		if apiErr, ok := requireSSECMatch(r, o.SSECKeyMD5); !ok {
			writeError(w, r, apiErr)
			return
		}
	}
	if status, ok := checkConditional(r.Header, `"`+o.ETag+`"`, o.Mtime); !ok {
		w.Header().Set("ETag", `"`+o.ETag+`"`)
		w.Header().Set("Last-Modified", o.Mtime.UTC().Format(http.TimeFormat))
		w.WriteHeader(status)
		return
	}
	// Derive the DEK before any body-shaping headers (Content-Length,
	// Content-Range) so a wrap/unwrap failure surfaces as a clean error
	// response — Content-Length set to o.Size would otherwise truncate the
	// XML body the client receives.
	var dek []byte
	if body && (o.SSE == sseAlgorithmAES256 || o.SSE == sseAlgorithmAWSKMS) && len(o.SSEKey) > 0 {
		var derr error
		switch o.SSE {
		case sseAlgorithmAES256:
			if s.Master == nil {
				writeError(w, r, ErrInternal)
				return
			}
			mk, merr := master.ResolveByID(r.Context(), s.Master, o.SSEKeyID)
			if merr != nil {
				writeError(w, r, ErrInternal)
				return
			}
			dek, derr = ssecrypto.UnwrapDEK(mk, o.SSEKey)
		case sseAlgorithmAWSKMS:
			if s.KMS == nil {
				writeError(w, r, ErrInternal)
				return
			}
			if o.SSEKeyID == "" {
				writeError(w, r, ErrKMSKeyIDMissing)
				return
			}
			dek, derr = s.KMS.UnwrapDEK(r.Context(), o.SSEKeyID, o.SSEKey)
			if errors.Is(derr, kms.ErrKeyIDMismatch) {
				writeError(w, r, ErrKMSAccessDenied)
				return
			}
			if errors.Is(derr, kms.ErrMissingKeyID) {
				writeError(w, r, ErrKMSKeyIDMissing)
				return
			}
		}
		if derr != nil {
			writeError(w, r, ErrInternal)
			return
		}
	}
	w.Header().Set("Content-Type", firstNonEmpty(o.ContentType, "application/octet-stream"))
	etag := o.ETag
	if partRange != nil && partRange.ETag != "" {
		etag = partRange.ETag
	}
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Last-Modified", o.Mtime.UTC().Format(http.TimeFormat))
	w.Header().Set("x-amz-storage-class", o.StorageClass)
	w.Header().Set("Accept-Ranges", "bytes")
	if o.CacheControl != "" {
		w.Header().Set("Cache-Control", o.CacheControl)
	}
	if o.Expires != "" {
		w.Header().Set("Expires", o.Expires)
	}
	for k, v := range o.UserMeta {
		w.Header()["x-amz-meta-"+k] = []string{v}
	}
	switch {
	case partNumber > 0:
		w.Header().Set("x-amz-mp-parts-count", strconv.Itoa(partsTotal))
	case o.PartsCount > 0:
		w.Header().Set("x-amz-mp-parts-count", strconv.Itoa(o.PartsCount))
	}
	if o.SSE != "" {
		w.Header().Set("x-amz-server-side-encryption", o.SSE)
	}
	if o.SSE == sseAlgorithmAWSKMS && o.SSEKeyID != "" {
		w.Header().Set(hdrSSEKMSKeyID, o.SSEKeyID)
	}
	if o.SSECKeyMD5 != "" {
		w.Header().Set(hdrSSECAlgorithm, sseAlgorithmAES256)
		w.Header().Set(hdrSSECKeyMD5, o.SSECKeyMD5)
	}
	if checksumModeEnabled(r.Header.Get("x-amz-checksum-mode")) {
		if partNumber > 0 {
			sums := partChecksumsAt(o, partNumber-1)
			if partRange != nil && partRange.ChecksumValue != "" && partRange.ChecksumAlgorithm != "" {
				if sums == nil {
					sums = map[string]string{}
				}
				sums[partRange.ChecksumAlgorithm] = partRange.ChecksumValue
			}
			writeChecksumHeaders(w.Header(), sums)
			// AWS-parity: ?partNumber= GET/HEAD echoes the object-level
			// ChecksumType alongside the per-part digest so SDKs (boto3
			// `response['ChecksumType']`) round-trip the COMPOSITE/FULL_OBJECT
			// label. s3-tests `test_multipart_use_cksum_helper_*` asserts this.
			if len(sums) > 0 && o.ChecksumType != "" {
				w.Header().Set("x-amz-checksum-type", o.ChecksumType)
			}
		} else {
			writeChecksumHeaders(w.Header(), o.Checksums)
			if o.ChecksumType != "" {
				w.Header().Set("x-amz-checksum-type", o.ChecksumType)
			}
		}
	}
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
	if o.RestoreStatus != "" {
		w.Header().Set("x-amz-restore", o.RestoreStatus)
	}
	if o.ReplicationStatus != "" {
		w.Header().Set("x-amz-replication-status", o.ReplicationStatus)
	}
	if o.VersionID != "" && meta.IsVersioningActive(b.Versioning) {
		w.Header().Set("x-amz-version-id", o.VersionID)
	}

	var (
		offset, length int64
		status         int
		ok             bool
	)
	if partNumber > 0 {
		var partOff, partLen int64
		if partRange != nil {
			partOff = partRange.Offset
			partLen = partRange.Size
		} else {
			partOff, partLen = partOffsetLength(o.PartSizes, partNumber)
		}
		offset, length = partOff, partLen
		status = http.StatusPartialContent
		ok = true
		if rh := r.Header.Get("Range"); rh != "" {
			subOff, subLen, _, subOK := parseRange(rh, partLen)
			if !subOK {
				w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(o.Size, 10))
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			offset = partOff + subOff
			length = subLen
		}
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(offset, 10)+"-"+strconv.FormatInt(offset+length-1, 10)+"/"+strconv.FormatInt(o.Size, 10))
	} else {
		offset, length, status, ok = parseRange(r.Header.Get("Range"), o.Size)
	}
	if !ok {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(o.Size, 10))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if status == http.StatusPartialContent && partNumber == 0 {
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(offset, 10)+"-"+strconv.FormatInt(offset+length-1, 10)+"/"+strconv.FormatInt(o.Size, 10))
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))

	if !body {
		w.WriteHeader(status)
		return
	}
	if dek != nil {
		var dec *sseDecryptingReader
		if o.Manifest != nil && len(o.Manifest.PartChunkCounts) > 0 {
			dec = newSSEDecryptingReaderWithLocator(r.Context(), s.Data, o.Manifest, dek, multipartChunkLocator(key, o.Manifest.PartChunkCounts), offset, length)
		} else {
			dec = newSSEDecryptingReader(r.Context(), s.Data, o.Manifest, dek, key, offset, length)
		}
		if err := dec.Preload(); err != nil {
			writeError(w, r, ErrInternal)
			return
		}
		w.WriteHeader(status)
		if _, copyErr := io.Copy(w, dec); copyErr != nil {
			return
		}
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

	if versionID != "" && b.MfaDelete == meta.MfaDeleteEnabled {
		if !s.validateMFAHeader(r.Header.Get("x-amz-mfa")) {
			writeError(w, r, ErrMFARequired)
			return
		}
	}

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

	var (
		o   *meta.Object
		err error
	)
	if versionID == "" && b.Versioning == meta.VersioningSuspended {
		// Suspended-mode unversioned DELETE: replace any prior null-versioned
		// row with a fresh null-versioned delete marker. The replaced row's
		// chunks (if any) are queued for GC.
		prior, perr := s.Meta.GetObject(r.Context(), b.ID, key, meta.NullVersionLiteral)
		if perr == nil && prior != nil && prior.Manifest != nil {
			s.enqueueChunks(r.Context(), prior.Manifest.Chunks)
		}
		o, err = s.Meta.DeleteObjectNullReplacement(r.Context(), b.ID, key)
	} else {
		o, err = s.Meta.DeleteObject(r.Context(), b.ID, key, versionID, versioned)
	}
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
		w.Header().Set("x-amz-version-id", wireVersionID(o))
		if o.IsDeleteMarker {
			w.Header().Set("x-amz-delete-marker", "true")
		}
	}
	if o != nil {
		eventName := "s3:ObjectRemoved:Delete"
		if o.IsDeleteMarker {
			eventName = "s3:ObjectRemoved:DeleteMarkerCreated"
		}
		s.emitNotificationEvent(r, b, notificationEventDetails{
			EventName: eventName,
			Key:       key,
			VersionID: o.VersionID,
			SourceIP:  clientSourceIP(r),
			Principal: principalFromContext(r),
		})
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
