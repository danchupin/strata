package s3api_test

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/danchupin/strata/internal/data"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

// TestMultipartGatewayRoutesToMultipartBackend pins the US-010 wiring in
// s3api: when the data backend implements data.MultipartBackend, the
// gateway's initiate / upload-part / complete / abort handlers must route
// through CreateBackendMultipart / UploadBackendPart /
// CompleteBackendMultipart / AbortBackendMultipart instead of the chunk-
// based PutChunks code path.
//
// Uses a fake MultipartBackend that records each call so the test can
// assert call topology without spinning up MinIO.
func TestMultipartGatewayRoutesToMultipartBackend(t *testing.T) {
	fake := newFakeMultipartBackend()
	api := s3api.New(fake, metamem.New())
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)

	// Seed bucket.
	mustStatus(t, http.MethodPut, ts.URL+"/bkt", nil, http.StatusOK)

	// Initiate.
	resp := mustDo(t, http.MethodPost, ts.URL+"/bkt/key?uploads", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initiate status: %d", resp.StatusCode)
	}
	body := mustReadBody(t, resp)
	m := regexp.MustCompile(`<UploadId>([^<]+)</UploadId>`).FindStringSubmatch(body)
	if len(m) != 2 {
		t.Fatalf("no UploadId in initiate body: %s", body)
	}
	uploadID := m[1]
	if got := fake.callCount("Create"); got != 1 {
		t.Fatalf("CreateBackendMultipart count: got %d, want 1", got)
	}

	// Two parts. Non-last part ≥ 5 MiB per S3 size-too-small (US-009);
	// last part can be smaller.
	largePart := make([]byte, 5<<20)
	for i := range largePart {
		largePart[i] = 'A'
	}
	parts := [][]byte{largePart, []byte("BBBBBB")}
	etags := make([]string, len(parts))
	for i, p := range parts {
		url := fmt.Sprintf("%s/bkt/key?uploadId=%s&partNumber=%d", ts.URL, uploadID, i+1)
		req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(string(p)))
		if err != nil {
			t.Fatalf("new req: %v", err)
		}
		req.ContentLength = int64(len(p))
		presp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("upload part %d: %v", i+1, err)
		}
		if presp.StatusCode != http.StatusOK {
			t.Fatalf("upload part %d status: %d", i+1, presp.StatusCode)
		}
		etags[i] = strings.Trim(presp.Header.Get("Etag"), `"`)
		if etags[i] == "" {
			t.Fatalf("part %d empty etag", i+1)
		}
		_ = presp.Body.Close()
	}
	if got := fake.callCount("UploadPart"); got != 2 {
		t.Fatalf("UploadBackendPart count: got %d, want 2", got)
	}

	// Complete.
	completeBody := "<CompleteMultipartUpload>" +
		fmt.Sprintf(`<Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part>`, etags[0]) +
		fmt.Sprintf(`<Part><PartNumber>2</PartNumber><ETag>"%s"</ETag></Part>`, etags[1]) +
		"</CompleteMultipartUpload>"
	cresp := mustDo(t, http.MethodPost, fmt.Sprintf("%s/bkt/key?uploadId=%s", ts.URL, uploadID), strings.NewReader(completeBody))
	if cresp.StatusCode != http.StatusOK {
		t.Fatalf("complete status: %d (body=%s)", cresp.StatusCode, mustReadBody(t, cresp))
	}
	_ = cresp.Body.Close()
	if got := fake.callCount("Complete"); got != 1 {
		t.Fatalf("CompleteBackendMultipart count: got %d, want 1", got)
	}

	// HEAD/GET to verify the manifest persisted with BackendRef shape.
	gresp := mustDo(t, http.MethodGet, ts.URL+"/bkt/key", nil)
	if gresp.StatusCode != http.StatusOK {
		t.Fatalf("GET assembled status: %d", gresp.StatusCode)
	}
	got := mustReadBody(t, gresp)
	want := string(parts[0]) + string(parts[1])
	if got != want {
		t.Fatalf("GET body: got %q, want %q", got, want)
	}

	// Initiate + abort path.
	resp2 := mustDo(t, http.MethodPost, ts.URL+"/bkt/abortme?uploads", nil)
	body2 := mustReadBody(t, resp2)
	m2 := regexp.MustCompile(`<UploadId>([^<]+)</UploadId>`).FindStringSubmatch(body2)
	if len(m2) != 2 {
		t.Fatalf("no UploadId for abort case: %s", body2)
	}
	uploadID2 := m2[1]
	mustStatus(t, http.MethodDelete, fmt.Sprintf("%s/bkt/abortme?uploadId=%s", ts.URL, uploadID2), nil, http.StatusNoContent)
	if got := fake.callCount("Abort"); got != 1 {
		t.Fatalf("AbortBackendMultipart count: got %d, want 1", got)
	}
}

// fakeMultipartBackend is a fixed-state MultipartBackend that satisfies
// data.Backend (chunk-based methods always error — never used in
// multipart pass-through paths) and data.MultipartBackend with stable
// fake values so tests can assert wiring without MinIO.
type fakeMultipartBackend struct {
	mu       sync.Mutex
	calls    map[string]int
	objects  map[string][]byte
	uploads  map[string]*fakeUpload
	versions map[string]string
}

type fakeUpload struct {
	key   string
	parts map[int32][]byte
}

func newFakeMultipartBackend() *fakeMultipartBackend {
	return &fakeMultipartBackend{
		calls:    map[string]int{},
		objects:  map[string][]byte{},
		uploads:  map[string]*fakeUpload{},
		versions: map[string]string{},
	}
}

func (f *fakeMultipartBackend) callCount(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[op]
}

// data.Backend surface — chunk-based ops are not used in pass-through
// paths; the gateway must never invoke them when MultipartBackend is
// present. PutChunks/Delete return errors so a regression that bypasses
// pass-through is loud.
func (f *fakeMultipartBackend) PutChunks(ctx context.Context, r io.Reader, class string) (*data.Manifest, error) {
	return nil, errors.New("fake: PutChunks must not be called when MultipartBackend handles the multipart")
}

func (f *fakeMultipartBackend) GetChunks(ctx context.Context, m *data.Manifest, off, length int64) (io.ReadCloser, error) {
	if m == nil || m.BackendRef == nil {
		return nil, errors.New("fake: GetChunks requires BackendRef-shape manifest")
	}
	f.mu.Lock()
	body, ok := f.objects[m.BackendRef.Key]
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("fake: no object for key %q", m.BackendRef.Key)
	}
	end := off + length
	if length <= 0 || end > int64(len(body)) {
		end = int64(len(body))
	}
	if off < 0 || off > int64(len(body)) {
		off = int64(len(body))
	}
	return io.NopCloser(strings.NewReader(string(body[off:end]))), nil
}

func (f *fakeMultipartBackend) Delete(ctx context.Context, m *data.Manifest) error {
	if m == nil || m.BackendRef == nil {
		return nil
	}
	f.mu.Lock()
	delete(f.objects, m.BackendRef.Key)
	f.mu.Unlock()
	return nil
}

func (f *fakeMultipartBackend) Close() error { return nil }

// data.MultipartBackend surface.
func (f *fakeMultipartBackend) CreateBackendMultipart(ctx context.Context, class string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["Create"]++
	uploadID := fmt.Sprintf("upload-%d", f.calls["Create"])
	key := fmt.Sprintf("bucket-uuid/object-%d", f.calls["Create"])
	f.uploads[uploadID] = &fakeUpload{key: key, parts: map[int32][]byte{}}
	return key + "\x00" + uploadID, nil
}

func (f *fakeMultipartBackend) UploadBackendPart(ctx context.Context, handle string, partNumber int32, r io.Reader, size int64) (string, error) {
	f.mu.Lock()
	f.calls["UploadPart"]++
	f.mu.Unlock()
	parts := strings.SplitN(handle, "\x00", 2)
	if len(parts) != 2 {
		return "", errors.New("fake: bad handle")
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	up, ok := f.uploads[parts[1]]
	if !ok {
		return "", errors.New("fake: no such upload")
	}
	up.parts[partNumber] = body
	sum := md5.Sum(body)
	return hex.EncodeToString(sum[:]), nil
}

func (f *fakeMultipartBackend) CompleteBackendMultipart(ctx context.Context, handle string, parts []data.BackendCompletedPart, class string) (*data.Manifest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["Complete"]++
	split := strings.SplitN(handle, "\x00", 2)
	if len(split) != 2 {
		return nil, errors.New("fake: bad handle")
	}
	up, ok := f.uploads[split[1]]
	if !ok {
		return nil, errors.New("fake: no such upload")
	}
	var assembled []byte
	for _, p := range parts {
		body, ok := up.parts[p.PartNumber]
		if !ok {
			return nil, fmt.Errorf("fake: missing part %d", p.PartNumber)
		}
		assembled = append(assembled, body...)
	}
	f.objects[split[0]] = assembled
	delete(f.uploads, split[1])
	versionID := fmt.Sprintf("v-%d", f.calls["Complete"])
	f.versions[split[0]] = versionID
	sum := md5.Sum(assembled)
	etag := hex.EncodeToString(sum[:]) + fmt.Sprintf("-%d", len(parts))
	return &data.Manifest{
		Class:     class,
		ETag:      etag,
		ChunkSize: data.DefaultChunkSize,
		BackendRef: &data.BackendRef{
			Backend:   "s3-fake",
			Key:       split[0],
			ETag:      etag,
			VersionID: versionID,
		},
	}, nil
}

func (f *fakeMultipartBackend) AbortBackendMultipart(ctx context.Context, handle string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls["Abort"]++
	split := strings.SplitN(handle, "\x00", 2)
	if len(split) == 2 {
		delete(f.uploads, split[1])
	}
	return nil
}

// Compile-time assertion.
var (
	_ data.Backend          = (*fakeMultipartBackend)(nil)
	_ data.MultipartBackend = (*fakeMultipartBackend)(nil)
)

func mustStatus(t *testing.T, method, url string, body io.Reader, want int) {
	t.Helper()
	resp := mustDo(t, method, url, body)
	defer resp.Body.Close()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s status: got %d want %d, body=%s", method, url, resp.StatusCode, want, string(b))
	}
}

func mustDo(t *testing.T, method, url string, body io.Reader) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

func mustReadBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
