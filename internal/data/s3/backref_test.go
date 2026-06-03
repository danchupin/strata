package s3

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/danchupin/strata/internal/data"
)

// captureTransport routes by method: it answers a HEAD with a fixed metadata
// set and a CopyObject (PUT carrying x-amz-copy-source) with 200, recording the
// CopyObject request's headers so the test can assert the re-stamped
// back-reference rode the metadata-replace copy.
type captureTransport struct {
	mu       sync.Mutex
	copyReq  http.Header
	headHits int
	copyHits int
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case req.Method == http.MethodHead:
		c.headHits++
		h := http.Header{}
		h.Set("X-Amz-Meta-Foo", "bar") // pre-existing user metadata to preserve
		h.Set("Content-Type", "application/octet-stream")
		h.Set("ETag", `"deadbeef"`)
		return &http.Response{
			StatusCode: http.StatusOK, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header: h, Body: io.NopCloser(strings.NewReader("")), Request: req,
		}, nil
	case req.Header.Get("X-Amz-Copy-Source") != "":
		c.copyHits++
		c.copyReq = req.Header.Clone()
		body := `<?xml version="1.0" encoding="UTF-8"?><CopyObjectResult><ETag>"deadbeef"</ETag></CopyObjectResult>`
		return &http.Response{
			StatusCode: http.StatusOK, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header: http.Header{"Content-Type": []string{"application/xml"}},
			Body:   io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: req,
		}, nil
	default:
		return nil, io.EOF
	}
}

// TestStampBackrefRewritesBackendObjectMetadata proves the s3 backend's
// data.BackrefStamper leg (US-001b): StampBackref on a BackendRef-shape
// manifest HEADs the backing object to preserve its metadata, then issues a
// self-CopyObject with MetadataDirective=REPLACE carrying the new
// x-amz-meta-strata-backref AND the preserved x-amz-meta-foo. The decoded
// back-reference must carry the final object identity passed in attrs.
func TestStampBackrefRewritesBackendObjectMetadata(t *testing.T) {
	ct := &captureTransport{}
	b := openTestBackend(t, ct)

	bucketID := uuid.New()
	mtime := time.Unix(1700001234, 0).UTC()
	m := &data.Manifest{
		Class: "STANDARD",
		BackendRef: &data.BackendRef{
			Backend: BackendName,
			Key:     bucketID.String() + "/" + uuid.New().String(),
		},
	}
	attrs := data.BackrefAttrs{
		BucketID:  bucketID,
		Key:       "my/object/key",
		VersionID: "v-final-123",
		Mtime:     mtime,
		SSEAlgo:   "AES256",
	}

	require.NoError(t, b.StampBackref(context.Background(), m, attrs))
	require.Equal(t, 1, ct.headHits, "must HEAD the backing object once")
	require.Equal(t, 1, ct.copyHits, "must self-copy once to replace metadata")

	require.Equal(t, "REPLACE", ct.copyReq.Get("X-Amz-Metadata-Directive"))
	require.Equal(t, "bar", ct.copyReq.Get("X-Amz-Meta-Foo"), "pre-existing metadata must be preserved")

	encoded := ct.copyReq.Get("X-Amz-Meta-Strata-Backref")
	require.NotEmpty(t, encoded, "re-stamped back-reference header must be present")
	raw, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	br, err := data.DecodeBackref(raw)
	require.NoError(t, err)

	require.Equal(t, bucketID, br.BucketID)
	require.Equal(t, "my/object/key", br.Key)
	require.Equal(t, "v-final-123", br.VersionID, "stamped version_id must be the final object's")
	require.Equal(t, 0, br.ChunkIdx, "single backing object is chunk index 0")
	require.Equal(t, "AES256", br.SSEAlgo)
	require.Equal(t, mtime, br.Mtime)
}

// TestStampBackrefDisabledIsNoOp proves the STRATA_CHUNK_BACKREF=false opt-out
// is honoured: StampBackref issues no backend calls when back-references are
// disabled at backend New time.
func TestStampBackrefDisabledIsNoOp(t *testing.T) {
	ct := &captureTransport{}
	b := openTestBackend(t, ct)
	b.backref = false // mirror BackrefEnabledFromEnv()==false

	m := &data.Manifest{Class: "STANDARD", BackendRef: &data.BackendRef{Backend: BackendName, Key: "k"}}
	require.NoError(t, b.StampBackref(context.Background(), m, data.BackrefAttrs{Key: "k"}))
	require.Equal(t, 0, ct.headHits)
	require.Equal(t, 0, ct.copyHits)
}
