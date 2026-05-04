package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/auth"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

// newUploadTestServer wires the admin Server with a real s3api handler so
// the Complete + Abort forwards land on a working multipart pipeline.
func newUploadTestServer(t *testing.T) (*Server, *meta.Bucket) {
	t.Helper()
	store := metamem.New()
	dataBackend := datamem.New()
	cred := &auth.Credential{
		AccessKey: "AKIAOPS",
		Secret:    "secret-ops",
		Owner:     "ops",
	}
	creds := auth.NewStaticStore(map[string]*auth.Credential{
		cred.AccessKey: cred,
	})
	s := New(Config{
		Meta:        store,
		Creds:       creds,
		Region:      "us-east-1",
		MetaBackend: "memory",
		DataBackend: "memory",
		JWTSecret:   []byte("0123456789abcdef0123456789abcdef"),
	})
	s.S3Handler = s3api.New(dataBackend, store)
	if h, ok := s.S3Handler.(*s3api.Server); ok {
		h.Region = "us-east-1"
	}
	b, err := store.CreateBucket(context.Background(), "uploadbkt", "AKIAOPS", "STANDARD")
	if err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
	return s, b
}

// uploadAdminRequest stamps an operator-authenticated context onto the
// request and dispatches through the route mux.
func uploadAdminRequest(t *testing.T, s *Server, method, path, accessKey string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if accessKey != "" {
		req = req.WithContext(auth.WithAuth(req.Context(), &auth.AuthInfo{AccessKey: accessKey, Owner: accessKey}))
	}
	req.Host = "strata.local:9000"
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, req)
	return rr
}

func TestUploadInit_HappyAndPartSize(t *testing.T) {
	s, _ := newUploadTestServer(t)
	rr := uploadAdminRequest(t, s, http.MethodPost,
		"/admin/v1/buckets/uploadbkt/uploads", "AKIAOPS",
		UploadInitRequest{Key: "hello/big.bin", StorageClass: "STANDARD"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got UploadInitResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.UploadID == "" || got.Key != "hello/big.bin" || got.Bucket != "uploadbkt" {
		t.Fatalf("unexpected response %+v", got)
	}
	if got.PartSize != uploadDefaultPartSize {
		t.Errorf("part_size=%d want %d", got.PartSize, uploadDefaultPartSize)
	}
}

func TestUploadInit_BucketMissing(t *testing.T) {
	s, _ := newUploadTestServer(t)
	rr := uploadAdminRequest(t, s, http.MethodPost,
		"/admin/v1/buckets/missing/uploads", "AKIAOPS",
		UploadInitRequest{Key: "x.bin"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestUploadInit_KeyRequired(t *testing.T) {
	s, _ := newUploadTestServer(t)
	rr := uploadAdminRequest(t, s, http.MethodPost,
		"/admin/v1/buckets/uploadbkt/uploads", "AKIAOPS",
		UploadInitRequest{})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestUploadPartPresign_HappyURLShape(t *testing.T) {
	s, _ := newUploadTestServer(t)
	initRR := uploadAdminRequest(t, s, http.MethodPost,
		"/admin/v1/buckets/uploadbkt/uploads", "AKIAOPS",
		UploadInitRequest{Key: "subdir/big.bin"})
	if initRR.Code != http.StatusCreated {
		t.Fatalf("init failed: %d %s", initRR.Code, initRR.Body.String())
	}
	var init UploadInitResponse
	_ = json.NewDecoder(initRR.Body).Decode(&init)

	rr := uploadAdminRequest(t, s, http.MethodPost,
		"/admin/v1/buckets/uploadbkt/uploads/"+init.UploadID+"/parts/3/presign", "AKIAOPS", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got UploadPartPresignResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.PartNumber != 3 {
		t.Errorf("part_number=%d want 3", got.PartNumber)
	}
	u, perr := url.Parse(got.URL)
	if perr != nil {
		t.Fatalf("url parse: %v", perr)
	}
	if !strings.HasSuffix(u.Path, "/uploadbkt/subdir/big.bin") {
		t.Errorf("path=%q does not end with /uploadbkt/subdir/big.bin", u.Path)
	}
	q := u.Query()
	if q.Get("partNumber") != "3" || q.Get("uploadId") != init.UploadID {
		t.Errorf("query partNumber=%q uploadId=%q", q.Get("partNumber"), q.Get("uploadId"))
	}
	if q.Get("X-Amz-Signature") == "" {
		t.Errorf("missing X-Amz-Signature on presigned URL")
	}
	if q.Get("X-Amz-Expires") != "300" {
		t.Errorf("X-Amz-Expires=%q want 300", q.Get("X-Amz-Expires"))
	}
}

func TestUploadPartPresign_PartNumberRange(t *testing.T) {
	s, _ := newUploadTestServer(t)
	initRR := uploadAdminRequest(t, s, http.MethodPost,
		"/admin/v1/buckets/uploadbkt/uploads", "AKIAOPS",
		UploadInitRequest{Key: "x.bin"})
	var init UploadInitResponse
	_ = json.NewDecoder(initRR.Body).Decode(&init)

	for _, n := range []string{"0", "10001", "abc"} {
		rr := uploadAdminRequest(t, s, http.MethodPost,
			"/admin/v1/buckets/uploadbkt/uploads/"+init.UploadID+"/parts/"+n+"/presign", "AKIAOPS", nil)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("partNumber=%q status=%d want 400", n, rr.Code)
		}
	}
}

func TestUploadPartPresign_UploadMissing(t *testing.T) {
	s, _ := newUploadTestServer(t)
	rr := uploadAdminRequest(t, s, http.MethodPost,
		"/admin/v1/buckets/uploadbkt/uploads/missing-upload-id/parts/1/presign", "AKIAOPS", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestUploadAbort_ForwardsToS3(t *testing.T) {
	s, b := newUploadTestServer(t)
	mu := &meta.MultipartUpload{
		BucketID:     b.ID,
		UploadID:     "upl-test-abort",
		Key:          "doomed.bin",
		StorageClass: "STANDARD",
		Status:       "uploading",
	}
	if err := s.Meta.CreateMultipartUpload(context.Background(), mu); err != nil {
		t.Fatalf("seed mu: %v", err)
	}
	rr := uploadAdminRequest(t, s, http.MethodDelete,
		"/admin/v1/buckets/uploadbkt/uploads/upl-test-abort", "AKIAOPS", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if _, err := s.Meta.GetMultipartUpload(context.Background(), b.ID, "upl-test-abort"); err == nil {
		t.Errorf("upload still present after abort")
	}
}

func TestUploadAbort_UploadMissing(t *testing.T) {
	s, _ := newUploadTestServer(t)
	rr := uploadAdminRequest(t, s, http.MethodDelete,
		"/admin/v1/buckets/uploadbkt/uploads/no-such-upload", "AKIAOPS", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestUploadComplete_PartsRequired(t *testing.T) {
	s, _ := newUploadTestServer(t)
	rr := uploadAdminRequest(t, s, http.MethodPost,
		"/admin/v1/buckets/uploadbkt/uploads/whatever/complete", "AKIAOPS",
		UploadCompleteRequest{})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSinglePresign_HappyURLShape(t *testing.T) {
	s, _ := newUploadTestServer(t)
	rr := uploadAdminRequest(t, s, http.MethodPost,
		"/admin/v1/buckets/uploadbkt/single-presign",
		"AKIAOPS", SinglePresignRequest{Key: "path/to/file.txt"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got SinglePresignResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	u, perr := url.Parse(got.URL)
	if perr != nil {
		t.Fatalf("url parse: %v", perr)
	}
	if !strings.HasSuffix(u.Path, "/uploadbkt/path/to/file.txt") {
		t.Errorf("path=%q does not end with /uploadbkt/path/to/file.txt", u.Path)
	}
	q := u.Query()
	if q.Get("X-Amz-Signature") == "" {
		t.Errorf("missing X-Amz-Signature on single-PUT URL")
	}
	if q.Get("X-Amz-Expires") != "300" {
		t.Errorf("X-Amz-Expires=%q want 300", q.Get("X-Amz-Expires"))
	}
}

func TestSinglePresign_BucketMissing(t *testing.T) {
	s, _ := newUploadTestServer(t)
	rr := uploadAdminRequest(t, s, http.MethodPost,
		"/admin/v1/buckets/missing/single-presign",
		"AKIAOPS", SinglePresignRequest{Key: "file.txt"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestSinglePresign_KeyRequired(t *testing.T) {
	s, _ := newUploadTestServer(t)
	rr := uploadAdminRequest(t, s, http.MethodPost,
		"/admin/v1/buckets/uploadbkt/single-presign",
		"AKIAOPS", SinglePresignRequest{})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestUploadInit_AnonymousIsBlockedByPresign(t *testing.T) {
	// Sanity check: presign requires an operator identity in context. The
	// admin auth middleware enforces this in production; we exercise the
	// fail-closed path here by hitting the presign route without stamping
	// an AuthInfo.
	s, _ := newUploadTestServer(t)
	initRR := uploadAdminRequest(t, s, http.MethodPost,
		"/admin/v1/buckets/uploadbkt/uploads", "AKIAOPS",
		UploadInitRequest{Key: "x.bin"})
	if initRR.Code != http.StatusCreated {
		t.Fatalf("init: %d %s", initRR.Code, initRR.Body.String())
	}
	var init UploadInitResponse
	_ = json.NewDecoder(initRR.Body).Decode(&init)
	rr := uploadAdminRequest(t, s, http.MethodPost,
		"/admin/v1/buckets/uploadbkt/uploads/"+init.UploadID+"/parts/1/presign", "", nil)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 (no operator identity) got %d body=%s", rr.Code, rr.Body.String())
	}
}
