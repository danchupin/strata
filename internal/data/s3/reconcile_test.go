package s3

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/danchupin/strata/internal/data"
)

// headProbeTransport answers HeadObject by key: a key in present returns 200,
// any other key returns a 404 (the SDK surfaces *types.NotFound) so the S3
// chunk prober (US-003b) maps it to absent. Records the HEADed keys so the
// test can assert exactly one probe per ChunkExists.
type headProbeTransport struct {
	mu      sync.Mutex
	present map[string]bool
	heads   []string
}

func (h *headProbeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	if req.Method != http.MethodHead {
		return &http.Response{
			StatusCode: http.StatusBadRequest, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req,
		}, nil
	}
	// Path-style URL: /<bucket>/<key>. The key is everything after the bucket.
	parts := strings.SplitN(strings.TrimPrefix(req.URL.Path, "/"), "/", 2)
	key := ""
	if len(parts) == 2 {
		key = parts[1]
	}
	h.mu.Lock()
	h.heads = append(h.heads, key)
	ok := h.present[key]
	h.mu.Unlock()
	status := http.StatusNotFound
	if ok {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req,
	}, nil
}

// TestS3ChunkExists is the US-003b S3-passthrough chunk prober: a backing
// object that HEADs 200 is present; one that HEADs 404 is absent (never an
// error). Drives data.ChunkStater the way the reconcile dangling pass does.
func TestS3ChunkExists(t *testing.T) {
	rt := &headProbeTransport{present: map[string]bool{"live-object": true}}
	b := openTestBackend(t, rt)
	ctx := context.Background()

	ok, err := b.ChunkExists(ctx, data.ChunkRef{OID: "live-object"})
	if err != nil {
		t.Fatalf("ChunkExists(live): %v", err)
	}
	if !ok {
		t.Errorf("live backing object reported absent")
	}

	ok, err = b.ChunkExists(ctx, data.ChunkRef{OID: "gone-object"})
	if err != nil {
		t.Fatalf("ChunkExists(gone): unexpected error %v", err)
	}
	if ok {
		t.Errorf("missing backing object reported present")
	}

	rt.mu.Lock()
	heads := append([]string(nil), rt.heads...)
	rt.mu.Unlock()
	if len(heads) != 2 || heads[0] != "live-object" || heads[1] != "gone-object" {
		t.Errorf("HEAD probes: got %v want [live-object gone-object]", heads)
	}
}

// TestS3ChunkExistsRequiresOID proves a probe with no backing key is a hard
// error (a misuse), never a silent false.
func TestS3ChunkExistsRequiresOID(t *testing.T) {
	rt := &headProbeTransport{present: map[string]bool{}}
	b := openTestBackend(t, rt)
	if _, err := b.ChunkExists(context.Background(), data.ChunkRef{}); err == nil {
		t.Errorf("ChunkExists with empty OID: want error, got nil")
	}
}

var _ data.ChunkStater = (*Backend)(nil)
