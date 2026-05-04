package adminapi

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gocql/gocql"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// uploadPresignTTL bounds the lifetime of every presigned URL minted by
// the admin upload flow (US-015 AC: "URL expires in 5 minutes").
const uploadPresignTTL = 5 * time.Minute

// uploadDefaultPartSize is the recommended multipart part size returned to
// the browser. 8 MiB is a sensible default — large enough that the per-part
// signing overhead amortises, small enough to keep a single failed PUT
// retry under a second on a 1 Gbps link.
const uploadDefaultPartSize = 8 * 1024 * 1024

// UploadInitRequest is the JSON body accepted by POST /admin/v1/buckets/
// {bucket}/uploads. Key is mandatory; storage_class / content_type /
// cache_control / content_disposition default to bucket / empty when
// omitted. UserMeta carries arbitrary x-amz-meta-* pairs persisted with
// the upload.
type UploadInitRequest struct {
	Key                string            `json:"key"`
	StorageClass       string            `json:"storage_class,omitempty"`
	ContentType        string            `json:"content_type,omitempty"`
	CacheControl       string            `json:"cache_control,omitempty"`
	ContentDisposition string            `json:"content_disposition,omitempty"`
	UserMeta           map[string]string `json:"user_meta,omitempty"`
	Tags               map[string]string `json:"tags,omitempty"`
}

// UploadInitResponse is the JSON returned from a successful Initiate. The
// browser should slice the file into PartSize-sized chunks and request a
// presigned URL per chunk.
type UploadInitResponse struct {
	UploadID string `json:"upload_id"`
	Key      string `json:"key"`
	Bucket   string `json:"bucket"`
	PartSize int64  `json:"part_size"`
}

// UploadPartPresignResponse carries a single per-part presigned URL plus
// the absolute expiry time so the browser can avoid using a URL that the
// gateway will reject.
type UploadPartPresignResponse struct {
	URL        string `json:"url"`
	ExpiresAt  int64  `json:"expires_at"`
	PartNumber int    `json:"part_number"`
}

// UploadCompleteRequest is the JSON body of /uploads/{uploadID}/complete.
// Parts must be sorted by PartNumber ASC; ETags carry the server-issued
// per-part ETag returned by UploadPart (without surrounding quotes).
type UploadCompleteRequest struct {
	Parts []UploadCompletePart `json:"parts"`
}

type UploadCompletePart struct {
	PartNumber int    `json:"part_number"`
	ETag       string `json:"etag"`
}

// SinglePresignResponse mirrors UploadPartPresignResponse but for the
// single-PUT (small-file) path — no PartNumber.
type SinglePresignResponse struct {
	URL       string `json:"url"`
	ExpiresAt int64  `json:"expires_at"`
}

// handleUploadInit serves POST /admin/v1/buckets/{bucket}/uploads. Wraps
// meta.Store.CreateMultipartUpload with operator-friendly JSON shape and
// returns the upload_id + recommended part_size.
//
// Audit row deferred to admin:UploadObject (stamped on Complete) so partial
// uploads never inflate the audit log. AbortMultipart writes its own row.
func (s *Server) handleUploadInit(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	bucket := r.PathValue("bucket")
	if bucket == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	var req UploadInitRequest
	if jerr := json.Unmarshal(body, &req); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
		return
	}
	key := strings.TrimSpace(req.Key)
	if key == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "key is required")
		return
	}

	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}

	class := strings.TrimSpace(req.StorageClass)
	if class == "" {
		class = b.DefaultClass
	}
	mu := &meta.MultipartUpload{
		BucketID:     b.ID,
		UploadID:     gocql.TimeUUID().String(),
		Key:          key,
		StorageClass: class,
		ContentType:  strings.TrimSpace(req.ContentType),
		InitiatedAt:  time.Now().UTC(),
		Status:       "uploading",
		UserMeta:     req.UserMeta,
		CacheControl: strings.TrimSpace(req.CacheControl),
	}
	if cerr := s.Meta.CreateMultipartUpload(r.Context(), mu); cerr != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", cerr.Error())
		return
	}
	writeJSON(w, http.StatusCreated, UploadInitResponse{
		UploadID: mu.UploadID,
		Key:      key,
		Bucket:   bucket,
		PartSize: uploadDefaultPartSize,
	})
}

// handleUploadPartPresign serves POST /admin/v1/buckets/{bucket}/uploads/
// {uploadID}/parts/{partNumber}/presign. Looks up the multipart upload to
// pick up the object key (the AC's URL doesn't carry it), looks up the
// operator's session credential to sign with, mints a 5-minute PUT URL
// pointing at /<bucket>/<key>?partNumber=N&uploadId=ID.
func (s *Server) handleUploadPartPresign(w http.ResponseWriter, r *http.Request) {
	bucket := r.PathValue("bucket")
	uploadID := r.PathValue("uploadID")
	partNumberStr := r.PathValue("partNumber")
	if bucket == "" || uploadID == "" || partNumberStr == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket, uploadID, partNumber are required")
		return
	}
	partNumber, err := strconv.Atoi(partNumberStr)
	if err != nil || partNumber < 1 || partNumber > 10000 {
		writeJSONError(w, http.StatusBadRequest, "InvalidArgument", "partNumber must be in [1,10000]")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	mu, err := s.Meta.GetMultipartUpload(r.Context(), b.ID, uploadID)
	if err != nil {
		if errors.Is(err, meta.ErrMultipartNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchUpload", "multipart upload not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}

	q := url.Values{}
	q.Set("partNumber", strconv.Itoa(partNumber))
	q.Set("uploadId", uploadID)
	urlStr, perr := s.mintPresignedURL(r, http.MethodPut, presignPath(bucket, mu.Key), q)
	if perr != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", perr.Error())
		return
	}
	writeJSON(w, http.StatusOK, UploadPartPresignResponse{
		URL:        urlStr,
		ExpiresAt:  time.Now().Add(uploadPresignTTL).Unix(),
		PartNumber: partNumber,
	})
}

// handleUploadComplete serves POST /admin/v1/buckets/{bucket}/uploads/
// {uploadID}/complete. Body carries the operator-supplied parts list; the
// handler synthesises the AWS CompleteMultipartUpload XML and forwards
// through the s3api handler so all the existing checksum / etag /
// versioning logic kicks in unchanged. Audit row admin:UploadObject is
// stamped on success.
func (s *Server) handleUploadComplete(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	bucket := r.PathValue("bucket")
	uploadID := r.PathValue("uploadID")
	if bucket == "" || uploadID == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket and uploadID are required")
		return
	}
	if s.S3Handler == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "s3 handler not wired")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	var req UploadCompleteRequest
	if jerr := json.Unmarshal(body, &req); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
		return
	}
	if len(req.Parts) == 0 {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "parts list is empty")
		return
	}

	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	mu, err := s.Meta.GetMultipartUpload(r.Context(), b.ID, uploadID)
	if err != nil {
		if errors.Is(err, meta.ErrMultipartNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchUpload", "multipart upload not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}

	xmlBody, xerr := encodeCompleteMultipartXML(req.Parts)
	if xerr != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", xerr.Error())
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:UploadObject", "object:"+bucket+"/"+mu.Key, bucket, owner)

	innerPath := "/" + bucket + "/" + mu.Key
	innerURL := innerPath + "?uploadId=" + url.QueryEscape(uploadID)
	inner := httptest.NewRequest(http.MethodPost, innerURL, bytes.NewReader(xmlBody))
	inner = inner.WithContext(ctx)
	inner.Header.Set("Content-Type", "application/xml")
	rec := httptest.NewRecorder()
	s.S3Handler.ServeHTTP(rec, inner)

	if rec.Code >= 200 && rec.Code < 300 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		writeJSON(w, http.StatusOK, map[string]any{
			"bucket":    bucket,
			"key":       mu.Key,
			"upload_id": uploadID,
		})
		return
	}
	writeJSONError(w, rec.Code, "CompleteFailed", strings.TrimSpace(rec.Body.String()))
}

// handleUploadAbort serves DELETE /admin/v1/buckets/{bucket}/uploads/
// {uploadID}. Forwards through s3api so chunk cleanup / metrics / etc
// stay consistent with the gateway-side abort path.
func (s *Server) handleUploadAbort(w http.ResponseWriter, r *http.Request) {
	bucket := r.PathValue("bucket")
	uploadID := r.PathValue("uploadID")
	if bucket == "" || uploadID == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket and uploadID are required")
		return
	}
	if s.S3Handler == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "s3 handler not wired")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	b, err := s.Meta.GetBucket(r.Context(), bucket)
	if err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	mu, err := s.Meta.GetMultipartUpload(r.Context(), b.ID, uploadID)
	if err != nil {
		if errors.Is(err, meta.ErrMultipartNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchUpload", "multipart upload not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}

	ctx := r.Context()
	owner := auth.FromContext(ctx).Owner
	s3api.SetAuditOverride(ctx, "admin:AbortMultipartUpload", "object:"+bucket+"/"+mu.Key, bucket, owner)

	innerPath := "/" + bucket + "/" + mu.Key
	innerURL := innerPath + "?uploadId=" + url.QueryEscape(uploadID)
	inner := httptest.NewRequest(http.MethodDelete, innerURL, nil)
	inner = inner.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.S3Handler.ServeHTTP(rec, inner)

	if rec.Code >= 200 && rec.Code < 300 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSONError(w, rec.Code, "AbortFailed", strings.TrimSpace(rec.Body.String()))
}

// SinglePresignRequest is the JSON body for POST /admin/v1/buckets/
// {bucket}/single-presign. The key cannot live in the URL because Go
// 1.22 mux trailing wildcards must be the last segment of the pattern,
// so `/objects/{key...}/single-presign` is unrepresentable — see the
// route registration block in server.go for the deviation note.
type SinglePresignRequest struct {
	Key string `json:"key"`
}

// handleSinglePutPresign serves POST /admin/v1/buckets/{bucket}/single-presign.
// Mints a 5-minute presigned PUT URL pointing directly at /<bucket>/<key>;
// the browser uploads <=5 MiB files in a single PUT (no multipart
// bookkeeping).
func (s *Server) handleSinglePutPresign(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	bucket := r.PathValue("bucket")
	if bucket == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "bucket is required")
		return
	}
	body, berr := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if berr != nil {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "failed to read body")
		return
	}
	var req SinglePresignRequest
	if jerr := json.Unmarshal(body, &req); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "MalformedRequest", "invalid JSON: "+jerr.Error())
		return
	}
	key := strings.TrimSpace(req.Key)
	if key == "" {
		writeJSONError(w, http.StatusBadRequest, "BadRequest", "key is required")
		return
	}
	if s.Meta == nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", "meta store not configured")
		return
	}
	if _, err := s.Meta.GetBucket(r.Context(), bucket); err != nil {
		if errors.Is(err, meta.ErrBucketNotFound) {
			writeJSONError(w, http.StatusNotFound, "NoSuchBucket", "bucket not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "Internal", err.Error())
		return
	}
	urlStr, perr := s.mintPresignedURL(r, http.MethodPut, presignPath(bucket, key), nil)
	if perr != nil {
		writeJSONError(w, http.StatusInternalServerError, "Internal", perr.Error())
		return
	}
	writeJSON(w, http.StatusOK, SinglePresignResponse{
		URL:       urlStr,
		ExpiresAt: time.Now().Add(uploadPresignTTL).Unix(),
	})
}

// mintPresignedURL looks up the operator's credential (the session-cookie
// AccessKey) and signs a SigV4 URL with the gateway's region. The browser
// never sees the secret — it only gets the resulting URL string. Host is
// lifted from the inbound request so the URL points back at the same
// gateway origin the operator is talking to.
func (s *Server) mintPresignedURL(r *http.Request, method, path string, query url.Values) (string, error) {
	if s.Creds == nil {
		return "", errors.New("credentials store not configured")
	}
	info := auth.FromContext(r.Context())
	if info == nil || info.IsAnonymous || info.AccessKey == "" {
		return "", errors.New("no operator identity in request context")
	}
	cred, err := s.Creds.Lookup(r.Context(), info.AccessKey)
	if err != nil || cred == nil {
		return "", fmt.Errorf("lookup operator credential: %w", err)
	}
	region := s.Region
	if region == "" {
		region = "default"
	}
	scheme := "https"
	if !isHTTPS(r) {
		scheme = "http"
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	return auth.GeneratePresignedURL(auth.PresignOptions{
		Method:    method,
		Scheme:    scheme,
		Host:      host,
		Path:      path,
		Query:     query,
		Region:    region,
		AccessKey: cred.AccessKey,
		Secret:    cred.Secret,
		Expires:   uploadPresignTTL,
	})
}

// presignPath assembles the /<bucket>/<key> path used as the request
// target for both per-part PUT URLs and the single-PUT URL.
func presignPath(bucket, key string) string {
	return "/" + bucket + "/" + key
}

// completeMultipartXMLPart matches the wire shape s3api.completeMultipart
// expects — see internal/s3api/multipart.go::completeMultipartBody.
type completeMultipartXMLPart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type completeMultipartXMLBody struct {
	XMLName xml.Name                   `xml:"CompleteMultipartUpload"`
	Parts   []completeMultipartXMLPart `xml:"Part"`
}

func encodeCompleteMultipartXML(parts []UploadCompletePart) ([]byte, error) {
	out := completeMultipartXMLBody{Parts: make([]completeMultipartXMLPart, 0, len(parts))}
	for _, p := range parts {
		if p.PartNumber < 1 {
			return nil, fmt.Errorf("invalid part_number %d", p.PartNumber)
		}
		if strings.TrimSpace(p.ETag) == "" {
			return nil, fmt.Errorf("missing etag for part %d", p.PartNumber)
		}
		etag := strings.Trim(strings.TrimSpace(p.ETag), `"`)
		out.Parts = append(out.Parts, completeMultipartXMLPart{
			PartNumber: p.PartNumber,
			ETag:       `"` + etag + `"`,
		})
	}
	return xml.Marshal(out)
}

