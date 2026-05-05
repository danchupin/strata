package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/promclient"
)

func replicationLagGET(t *testing.T, s *Server, bucket, rawQuery string) (int, BucketReplicationLagResponse) {
	t.Helper()
	rr := httptest.NewRecorder()
	url := "/admin/v1/buckets/" + bucket + "/replication-lag"
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	req := withAuditAuthCtx(httptest.NewRequest(http.MethodGet, url, nil), "operator")
	s.routes().ServeHTTP(rr, req)
	var got BucketReplicationLagResponse
	if rr.Body.Len() > 0 && rr.Header().Get("Content-Type") == "application/json" {
		_ = json.NewDecoder(rr.Body).Decode(&got)
	}
	return rr.Code, got
}

// configureReplication writes a stub replication blob so the handler treats
// the bucket as "replication configured". The XML payload shape doesn't
// matter for the lag endpoint — only blob presence does.
func configureReplication(t *testing.T, s *Server, bucket string) {
	t.Helper()
	b, err := s.Meta.GetBucket(context.Background(), bucket)
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	if err := s.Meta.SetBucketReplication(context.Background(), b.ID, []byte("<stub/>")); err != nil {
		t.Fatalf("set replication: %v", err)
	}
}

func TestBucketReplicationLagReturnsValues(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			t.Errorf("path=%q", r.URL.Path)
		}
		expr := r.URL.Query().Get("query")
		want := fmt.Sprintf(replicationLagExprFmt, "alpha")
		if expr != want {
			t.Errorf("query=%q want %q", expr, want)
		}
		body := fmt.Sprintf(matrixResponseBody, matrixSeries("alpha",
			[][2]string{{"1700000000.0", "0.5"}, {"1700000060.0", "1.5"}}))
		_, _ = w.Write([]byte(body))
	}))
	defer prom.Close()

	s := newTestServer()
	s.Prom = promclient.New(prom.URL)
	seedBucketWithOwner(t, s.Meta, "alpha", "alice", 0, 0)
	configureReplication(t, s, "alpha")

	code, got := replicationLagGET(t, s, "alpha", "range=1h")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	if got.Empty {
		t.Fatalf("got empty=true want values")
	}
	if len(got.Values) != 2 {
		t.Fatalf("values len=%d want 2", len(got.Values))
	}
	if got.Values[0].Value != 0.5 || got.Values[1].Value != 1.5 {
		t.Errorf("values=%+v", got.Values)
	}
}

func TestBucketReplicationLagEmptyWhenUnconfigured(t *testing.T) {
	s := newTestServer()
	s.Prom = promclient.New("http://prom.invalid") // would 503 if reached
	seedBucketWithOwner(t, s.Meta, "alpha", "alice", 0, 0)

	code, got := replicationLagGET(t, s, "alpha", "")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	if !got.Empty {
		t.Fatalf("empty=false want true")
	}
	if got.Reason == "" {
		t.Errorf("reason empty")
	}
}

func TestBucketReplicationLagPromUnavailable(t *testing.T) {
	s := newTestServer() // s.Prom == nil-equivalent: BaseURL=""
	seedBucketWithOwner(t, s.Meta, "alpha", "alice", 0, 0)
	configureReplication(t, s, "alpha")

	code, _ := replicationLagGET(t, s, "alpha", "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", code)
	}
}

func TestBucketReplicationLagMissingBucket404(t *testing.T) {
	s := newTestServer()
	code, _ := replicationLagGET(t, s, "missing", "")
	if code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", code)
	}
}

func TestBucketReplicationLagRejectsBadRange(t *testing.T) {
	s := newTestServer()
	seedBucketWithOwner(t, s.Meta, "alpha", "alice", 0, 0)
	configureReplication(t, s, "alpha")
	for _, q := range []string{"range=oops", "range=-1h", "range=0"} {
		t.Run(q, func(t *testing.T) {
			code, _ := replicationLagGET(t, s, "alpha", q)
			if code != http.StatusBadRequest {
				t.Fatalf("status=%d body want 400", code)
			}
		})
	}
}

func TestReplicationLagStepAutoDerives(t *testing.T) {
	cases := []struct {
		rng  time.Duration
		want time.Duration
	}{
		{15 * time.Minute, 15 * time.Second},
		{time.Hour, time.Minute},
		{6 * time.Hour, 5 * time.Minute},
		{24 * time.Hour, 30 * time.Minute},
	}
	for _, c := range cases {
		if got := replicationLagStep(c.rng); got != c.want {
			t.Errorf("range=%s step=%s want %s", c.rng, got, c.want)
		}
	}
}
