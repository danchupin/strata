package s3

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/data"
)

// TestMultipartSurvivesDrainingCluster pins the US-009 drain invariant on
// the s3 pass-through backend: an in-flight multipart session bound to a
// cluster that is subsequently marked draining MUST finish — UploadPart +
// Complete recover routing from the opaque BackendUploadID handle (connFor)
// and never re-consult the placement picker, while a FRESH PutChunks to the
// same draining cluster is refused with data.ErrDrainRefused.
//
// This is the "PUT-only stop-write" half of the always-strict drain
// invariant proven at the backend boundary: the picker stops landing new
// objects, but a session already routed to the cluster runs to completion.
func TestMultipartSurvivesDrainingCluster(t *testing.T) {
	server := newSyntheticMultipartServer()
	transport := &httpHandlerTransport{handler: server}

	ctx := context.Background()
	b := openTestBackend(t, transport)

	// Initiate while the cluster is live — handle carries cluster "default".
	handle, err := b.CreateBackendMultipart(ctx, "STANDARD")
	if err != nil {
		t.Fatalf("CreateBackendMultipart: %v", err)
	}
	cluster, _, key, _, err := decodeHandle(handle)
	if err != nil {
		t.Fatalf("decode handle: %v", err)
	}
	if cluster != "default" {
		t.Fatalf("handle cluster %q, want default", cluster)
	}

	// Now the cluster is draining. Every subsequent backend call carries the
	// draining set in context (mirrors the gateway's
	// data.WithDrainingClusters injection on the data-plane hot path).
	drainCtx := data.WithDrainingClusters(ctx, map[string]bool{"default": true})

	// FRESH write to the draining cluster → refused (picker stop-write).
	if _, err := b.PutChunks(drainCtx, strings.NewReader("fresh"), "STANDARD"); !errors.Is(err, data.ErrDrainRefused) {
		t.Fatalf("PutChunks on draining cluster: want ErrDrainRefused, got %v", err)
	}
	var dre *data.DrainRefusedError
	if _, err := b.PutChunks(drainCtx, strings.NewReader("fresh"), "STANDARD"); !errors.As(err, &dre) || dre.Cluster != "default" {
		t.Fatalf("PutChunks DrainRefusedError cluster: got %v, want default", err)
	}

	// In-flight session keeps running against the draining cluster: the part
	// upload routes via the handle, not the picker.
	etag1, err := b.UploadBackendPart(drainCtx, handle, 1, strings.NewReader("part-1"), 6)
	if err != nil {
		t.Fatalf("UploadBackendPart on draining cluster: want success (handle-routed), got %v", err)
	}
	if etag1 != server.partETag(1) {
		t.Fatalf("part 1 etag: got %q, want %q", etag1, server.partETag(1))
	}

	// Complete finalises on the same (cluster, bucket, key) pair Create
	// initiated — never re-picks.
	m, err := b.CompleteBackendMultipart(drainCtx, handle, []data.BackendCompletedPart{
		{PartNumber: 1, ETag: etag1},
	}, "STANDARD")
	if err != nil {
		t.Fatalf("CompleteBackendMultipart on draining cluster: want success, got %v", err)
	}
	if m.BackendRef == nil || m.BackendRef.Key != key {
		t.Fatalf("BackendRef must point at the initiated key %q, got %+v", key, m.BackendRef)
	}

	// The SDK saw exactly one Create + one UploadPart + one Complete — no
	// re-routing, no second Create against a live cluster.
	if got := server.requestCount("CreateMultipartUpload"); got != 1 {
		t.Fatalf("CreateMultipartUpload count: got %d, want 1", got)
	}
	if got := server.requestCount("UploadPart"); got != 1 {
		t.Fatalf("UploadPart count: got %d, want 1", got)
	}
	if got := server.requestCount("CompleteMultipartUpload"); got != 1 {
		t.Fatalf("CompleteMultipartUpload count: got %d, want 1", got)
	}

	// Abort is also handle-routed and idempotent against the draining
	// cluster (NoSuchUpload after Complete → nil).
	if err := b.AbortBackendMultipart(drainCtx, handle); err != nil {
		t.Fatalf("AbortBackendMultipart on draining cluster: %v", err)
	}
}
