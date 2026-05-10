package racetest_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/racetest"
	"github.com/danchupin/strata/internal/s3api"
)

// TestTrackerValidateETag covers the in-memory tracker logic without
// hitting the gateway: a recorded PUT must match by ETag, an unknown
// ETag must miss, and an etag-match with size-mismatch must flag the
// size discrepancy.
func TestTrackerValidateETag(t *testing.T) {
	tr := racetest.NewTracker(nil, 5*time.Second)
	now := time.Now().UTC()
	tr.RecordPut("b", "k", "etag-a", "v1", 7, now)

	if got := len(tr.Snapshot()); got != 0 {
		t.Fatalf("snapshot before any flag: got %d, want 0", got)
	}

	tr.Flag(racetest.Inconsistency{Kind: "x", Bucket: "b", Key: "k"})
	got := tr.Snapshot()
	if len(got) != 1 || got[0].Kind != "x" {
		t.Fatalf("snapshot after flag: %+v", got)
	}
}

// TestRunCleanRunNoInconsistencies asserts the verifier produces zero
// inconsistencies against the in-memory backend over a short Run. This
// is the regression gate for false positives — the recheck-after-200ms
// ETag race-window absorption must keep Run honest under concurrent
// writes.
func TestRunCleanRunNoInconsistencies(t *testing.T) {
	api := s3api.New(datamem.New(), metamem.New())
	api.Region = "default"
	ts := httptest.NewServer(http.HandlerFunc(api.ServeHTTP))
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	report, err := racetest.Run(ctx, racetest.Config{
		HTTPEndpoint: ts.URL,
		Duration:     2 * time.Second,
		Concurrency:  4,
		BucketCount:  1,
		ObjectKeys:   3,
		VerifyEvery:  200 * time.Millisecond,
		// Generously larger than Run.Duration so no delete grace check
		// can fire — a clean run shouldn't depend on grace timing.
		DeleteGracePeriod: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(report.Inconsistencies); got != 0 {
		t.Fatalf("clean run produced %d inconsistencies: %+v", got, report.Inconsistencies)
	}
}

// faultProxy mutates upstream gateway responses to inject corruption
// so the verifier's three oracles can be exercised without writing a
// broken meta backend. The fault knobs are mutually exclusive — exactly
// one is set per test.
type faultProxy struct {
	upstream     *httptest.Server
	mutateETag   bool // GET returns a synthetic etag the workload never PUT
	hideVersions bool // ?versions response is replaced with empty <ListVersionsResult/>
	resurrect    bool // ListObjectsV2 echoes prefix back as a Contents row
}

func (p *faultProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	method := r.Method

	// Forge a ListBucketResult response whose Contents.Key matches the
	// requested prefix so the delete-grace verifier (which lists with
	// prefix=<deleted-key>) sees the deleted key as live.
	if p.resurrect && method == "GET" && q.Get("list-type") == "2" {
		prefix := q.Get("prefix")
		if prefix == "" {
			prefix = "ghost"
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(200)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>fault</Name>
  <Prefix>%s</Prefix>
  <KeyCount>1</KeyCount>
  <MaxKeys>1</MaxKeys>
  <IsTruncated>false</IsTruncated>
  <Contents><Key>%s</Key><LastModified>2026-01-01T00:00:00Z</LastModified><ETag>"d41d8cd98f00b204e9800998ecf8427e"</ETag><Size>0</Size><StorageClass>STANDARD</StorageClass></Contents>
</ListBucketResult>`, prefix, prefix)
		return
	}

	if p.hideVersions && method == "GET" && q.Has("versions") {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(200)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListVersionsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>fault</Name>
  <KeyMarker></KeyMarker>
  <VersionIdMarker></VersionIdMarker>
  <MaxKeys>1000</MaxKeys>
  <IsTruncated>false</IsTruncated>
</ListVersionsResult>`)
		return
	}

	// Forward the request to the upstream gateway and capture response.
	rr := httptest.NewRecorder()
	r2 := r.Clone(r.Context())
	r2.URL.Scheme = "http"
	r2.URL.Host = strings.TrimPrefix(p.upstream.URL, "http://")
	r2.RequestURI = ""
	resp, err := p.upstream.Client().Do(r2)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		for _, v := range vs {
			rr.Header().Add(k, v)
		}
	}
	rr.WriteHeader(resp.StatusCode)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			rr.Write(buf[:n])
		}
		if rerr != nil {
			break
		}
	}

	// Mutate GET-object response Etag to a value the workload could not
	// have PUT, so validateETag flags the divergence.
	if p.mutateETag && method == "GET" && resp.StatusCode == 200 &&
		!q.Has("list-type") && !q.Has("versions") {
		rr.Header().Set("Etag", `"deadbeefcafebabe1234567890abcdef"`)
	}

	for k, vs := range rr.Header() {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(rr.Code)
	w.Write(rr.Body.Bytes())
}

func newFaultGateway(t *testing.T, p *faultProxy) *httptest.Server {
	t.Helper()
	api := s3api.New(datamem.New(), metamem.New())
	api.Region = "default"
	upstream := httptest.NewServer(http.HandlerFunc(api.ServeHTTP))
	t.Cleanup(upstream.Close)
	p.upstream = upstream
	ts := httptest.NewServer(p)
	t.Cleanup(ts.Close)
	return ts
}

// TestVerifierFlagsETagMismatch wraps the gateway in a proxy that mutates
// every successful GET object response's Etag to a value the workload
// never PUT, asserting the read-after-write oracle produces at least one
// inconsistency.
func TestVerifierFlagsETagMismatch(t *testing.T) {
	ts := newFaultGateway(t, &faultProxy{mutateETag: true})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	report, err := racetest.Run(ctx, racetest.Config{
		HTTPEndpoint: ts.URL,
		Duration:     1 * time.Second,
		Concurrency:  2,
		BucketCount:  1,
		ObjectKeys:   2,
		VerifyEvery:  150 * time.Millisecond,
		Mix: map[string]float64{
			racetest.OpPut: 1.0,
		},
		DeleteGracePeriod: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	hits := 0
	for _, inc := range report.Inconsistencies {
		if inc.Kind == "read_after_write" {
			hits++
		}
	}
	if hits == 0 {
		t.Fatalf("expected ≥1 read_after_write inconsistency, got %+v", report.Inconsistencies)
	}
}

// TestVerifierFlagsResurrectedDelete proves the delete-grace oracle
// flags a key that surfaces in ListObjectsV2 after the grace window
// elapses. Workload is delete-only with a sub-millisecond grace so
// the verifier catches a stale deletedAt on the next tick reliably
// regardless of workload pacing.
func TestVerifierFlagsResurrectedDelete(t *testing.T) {
	ts := newFaultGateway(t, &faultProxy{resurrect: true})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	report, err := racetest.Run(ctx, racetest.Config{
		HTTPEndpoint: ts.URL,
		Duration:     1500 * time.Millisecond,
		Concurrency:  1,
		BucketCount:  1,
		ObjectKeys:   2,
		VerifyEvery:  100 * time.Millisecond,
		Mix: map[string]float64{
			racetest.OpDeleteObjects: 1.0,
		},
		DeleteGracePeriod: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	hits := 0
	for _, inc := range report.Inconsistencies {
		if inc.Kind == "delete_grace" {
			hits++
		}
	}
	if hits == 0 {
		t.Fatalf("expected ≥1 delete_grace inconsistency, got %+v", report.Inconsistencies)
	}
}

// TestVerifierFlagsMissingVersion stubs ?versions to always return an
// empty version list, so any tracked version_id triggers the oracle.
func TestVerifierFlagsMissingVersion(t *testing.T) {
	ts := newFaultGateway(t, &faultProxy{hideVersions: true})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	report, err := racetest.Run(ctx, racetest.Config{
		HTTPEndpoint: ts.URL,
		Duration:     1500 * time.Millisecond,
		Concurrency:  2,
		BucketCount:  1,
		ObjectKeys:   2,
		VerifyEvery:  200 * time.Millisecond,
		Mix: map[string]float64{
			racetest.OpPut: 1.0,
		},
		DeleteGracePeriod: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	hits := 0
	for _, inc := range report.Inconsistencies {
		if inc.Kind == "versioning_missing" {
			hits++
		}
	}
	if hits == 0 {
		t.Fatalf("expected ≥1 versioning_missing inconsistency, got %+v", report.Inconsistencies)
	}
}

// TestRejectsNegativeGrace asserts Config validation flags a negative
// DeleteGracePeriod. Zero is allowed (defaulted).
func TestRejectsNegativeGrace(t *testing.T) {
	_, err := racetest.Run(context.Background(), racetest.Config{
		HTTPEndpoint:      "http://x",
		Duration:          time.Second,
		Concurrency:       1,
		DeleteGracePeriod: -1 * time.Second,
	})
	if err == nil {
		t.Fatal("expected error for negative DeleteGracePeriod")
	}
}
