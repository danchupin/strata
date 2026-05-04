package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

func TestMultipartActive_HappyAndFiltering(t *testing.T) {
	s, b := newUploadTestServer(t)
	now := time.Now().UTC()
	stale := &meta.MultipartUpload{
		BucketID:     b.ID,
		UploadID:     "upl-stale",
		Key:          "stalled/big.bin",
		StorageClass: "STANDARD",
		Status:       "uploading",
		InitiatedAt:  now.Add(-48 * time.Hour),
	}
	fresh := &meta.MultipartUpload{
		BucketID:     b.ID,
		UploadID:     "upl-fresh",
		Key:          "fresh/file.bin",
		StorageClass: "STANDARD",
		Status:       "uploading",
		InitiatedAt:  now.Add(-30 * time.Minute),
	}
	for _, mu := range []*meta.MultipartUpload{stale, fresh} {
		if err := s.Meta.CreateMultipartUpload(context.Background(), mu); err != nil {
			t.Fatalf("seed %s: %v", mu.UploadID, err)
		}
	}
	if err := s.Meta.SavePart(context.Background(), b.ID, "upl-stale", &meta.MultipartPart{
		PartNumber: 1, ETag: "etag1", Size: 2_000_000,
	}); err != nil {
		t.Fatalf("save part 1: %v", err)
	}
	if err := s.Meta.SavePart(context.Background(), b.ID, "upl-stale", &meta.MultipartPart{
		PartNumber: 2, ETag: "etag2", Size: 3_500_000,
	}); err != nil {
		t.Fatalf("save part 2: %v", err)
	}

	// All-uploads view (no filters).
	rr := uploadAdminRequest(t, s, http.MethodGet, "/admin/v1/multipart/active", "AKIAOPS", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp MultipartActiveResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 {
		t.Fatalf("total=%d want 2", resp.Total)
	}
	if len(resp.Uploads) != 2 {
		t.Fatalf("uploads len=%d want 2", len(resp.Uploads))
	}
	// Oldest first.
	if resp.Uploads[0].UploadID != "upl-stale" {
		t.Errorf("uploads[0]=%s want upl-stale (oldest first)", resp.Uploads[0].UploadID)
	}
	if resp.Uploads[0].BytesUploaded != 5_500_000 {
		t.Errorf("bytes_uploaded=%d want 5500000", resp.Uploads[0].BytesUploaded)
	}
	if resp.Uploads[0].Bucket != "uploadbkt" || resp.Uploads[0].Key != "stalled/big.bin" {
		t.Errorf("row 0 = %+v", resp.Uploads[0])
	}
	if resp.Uploads[0].Initiator != "AKIAOPS" {
		t.Errorf("initiator=%q want AKIAOPS (bucket owner fallback)", resp.Uploads[0].Initiator)
	}
	if resp.Uploads[0].AgeSeconds < int64(47*time.Hour/time.Second) {
		t.Errorf("age_seconds=%d too low", resp.Uploads[0].AgeSeconds)
	}

	// min_age_hours=24 hides the fresh row.
	rr = uploadAdminRequest(t, s, http.MethodGet,
		"/admin/v1/multipart/active?min_age_hours=24", "AKIAOPS", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	resp = MultipartActiveResponse{}
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Total != 1 || len(resp.Uploads) != 1 || resp.Uploads[0].UploadID != "upl-stale" {
		t.Errorf("min_age_hours=24 resp = %+v", resp)
	}

	// bucket=missing returns no rows.
	rr = uploadAdminRequest(t, s, http.MethodGet,
		"/admin/v1/multipart/active?bucket=missing", "AKIAOPS", nil)
	resp = MultipartActiveResponse{}
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Total != 0 {
		t.Errorf("bucket=missing total=%d want 0", resp.Total)
	}

	// initiator=other filters everything out.
	rr = uploadAdminRequest(t, s, http.MethodGet,
		"/admin/v1/multipart/active?initiator=other", "AKIAOPS", nil)
	resp = MultipartActiveResponse{}
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Total != 0 {
		t.Errorf("initiator=other total=%d want 0", resp.Total)
	}
}

func TestMultipartActive_BadMinAge(t *testing.T) {
	s, _ := newUploadTestServer(t)
	rr := uploadAdminRequest(t, s, http.MethodGet,
		"/admin/v1/multipart/active?min_age_hours=-1", "AKIAOPS", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%s", rr.Code, rr.Body.String())
	}
}

func TestMultipartActive_Pagination(t *testing.T) {
	s, b := newUploadTestServer(t)
	now := time.Now().UTC()
	for i := range 5 {
		mu := &meta.MultipartUpload{
			BucketID:     b.ID,
			UploadID:     "upl-" + string(rune('a'+i)),
			Key:          "k" + string(rune('0'+i)),
			StorageClass: "STANDARD",
			Status:       "uploading",
			InitiatedAt:  now.Add(time.Duration(-i-1) * time.Hour),
		}
		if err := s.Meta.CreateMultipartUpload(context.Background(), mu); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	rr := uploadAdminRequest(t, s, http.MethodGet,
		"/admin/v1/multipart/active?page=2&page_size=2", "AKIAOPS", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp MultipartActiveResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if resp.Total != 5 {
		t.Errorf("total=%d want 5", resp.Total)
	}
	if len(resp.Uploads) != 2 {
		t.Errorf("page 2 size=%d want 2", len(resp.Uploads))
	}
}

func TestMultipartAbort_Batch(t *testing.T) {
	s, b := newUploadTestServer(t)
	for _, id := range []string{"upl-x", "upl-y"} {
		mu := &meta.MultipartUpload{
			BucketID:     b.ID,
			UploadID:     id,
			Key:          id + ".bin",
			StorageClass: "STANDARD",
			Status:       "uploading",
			InitiatedAt:  time.Now().UTC().Add(-time.Hour),
		}
		if err := s.Meta.CreateMultipartUpload(context.Background(), mu); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	body := MultipartAbortRequest{Uploads: []MultipartAbortTarget{
		{Bucket: "uploadbkt", UploadID: "upl-x"},
		{Bucket: "uploadbkt", UploadID: "upl-y"},
		{Bucket: "uploadbkt", UploadID: "no-such"},
		{Bucket: "missing", UploadID: "anything"},
	}}
	rr := uploadAdminRequest(t, s, http.MethodPost, "/admin/v1/multipart/abort", "AKIAOPS", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp MultipartAbortResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Results) != 4 {
		t.Fatalf("results len=%d want 4", len(resp.Results))
	}
	wantStatus := []struct {
		uploadID string
		status   string
		code     string
	}{
		{"upl-x", "aborted", ""},
		{"upl-y", "aborted", ""},
		{"no-such", "error", "NoSuchUpload"},
		{"anything", "error", "NoSuchBucket"},
	}
	for i, want := range wantStatus {
		got := resp.Results[i]
		if got.UploadID != want.uploadID || got.Status != want.status || got.Code != want.code {
			t.Errorf("results[%d] = %+v want uploadID=%s status=%s code=%s",
				i, got, want.uploadID, want.status, want.code)
		}
	}
	// Verify the aborted uploads are actually gone from meta.
	for _, id := range []string{"upl-x", "upl-y"} {
		if _, err := s.Meta.GetMultipartUpload(context.Background(), b.ID, id); err == nil {
			t.Errorf("upload %s still present after abort", id)
		}
	}
}

func TestMultipartAbort_EmptyBody(t *testing.T) {
	s, _ := newUploadTestServer(t)
	rr := uploadAdminRequest(t, s, http.MethodPost, "/admin/v1/multipart/abort", "AKIAOPS",
		MultipartAbortRequest{})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%s", rr.Code, rr.Body.String())
	}
}

func TestMultipartAbort_NestedKey(t *testing.T) {
	// Smoke test that uploads with nested keys still route through the
	// inner s3api handler and abort cleanly.
	s, b := newUploadTestServer(t)
	mu := &meta.MultipartUpload{
		BucketID:     b.ID,
		UploadID:     "upl-nested",
		Key:          "logs/2026/05/abort.bin",
		StorageClass: "STANDARD",
		Status:       "uploading",
		InitiatedAt:  time.Now().UTC().Add(-time.Hour),
	}
	if err := s.Meta.CreateMultipartUpload(context.Background(), mu); err != nil {
		t.Fatalf("seed: %v", err)
	}
	body := MultipartAbortRequest{Uploads: []MultipartAbortTarget{
		{Bucket: "uploadbkt", UploadID: "upl-nested"},
	}}
	rr := uploadAdminRequest(t, s, http.MethodPost, "/admin/v1/multipart/abort", "AKIAOPS", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var resp MultipartAbortResponse
	_ = json.NewDecoder(rr.Body).Decode(&resp)
	if len(resp.Results) != 1 || resp.Results[0].Status != "aborted" {
		t.Errorf("nested-key abort results = %+v", resp.Results)
	}
	if _, err := s.Meta.GetMultipartUpload(context.Background(), b.ID, "upl-nested"); err == nil {
		t.Errorf("nested upload still present after abort")
	}
}
