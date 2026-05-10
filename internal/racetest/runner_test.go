package racetest_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/racetest"
	"github.com/danchupin/strata/internal/s3api"
)

// TestRunSmoke wires the racetest.Run entrypoint to an in-process gateway and
// asserts it (a) succeeds, (b) populates OpsByClass with positive counts for
// every op class, and (c) honours Duration. The standalone strata-racecheck
// binary (US-002) exercises the same code path against a remote endpoint.
func TestRunSmoke(t *testing.T) {
	api := s3api.New(datamem.New(), metamem.New())
	api.Region = "default"
	ts := httptest.NewServer(http.HandlerFunc(api.ServeHTTP))
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	report, err := racetest.Run(ctx, racetest.Config{
		HTTPEndpoint: ts.URL,
		Duration:     500 * time.Millisecond,
		Concurrency:  4,
		BucketCount:  2,
		ObjectKeys:   3,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report == nil {
		t.Fatal("nil report")
	}
	if report.Duration <= 0 {
		t.Errorf("expected positive duration, got %v", report.Duration)
	}
	for _, class := range []string{"put", "delete", "multipart"} {
		if report.OpsByClass[class] == 0 {
			t.Errorf("op class %q: expected >0 ops, got 0", class)
		}
	}
}

func TestRunRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  racetest.Config
	}{
		{"no endpoint", racetest.Config{Duration: time.Second, Concurrency: 1}},
		{"zero duration", racetest.Config{HTTPEndpoint: "http://x", Concurrency: 1}},
		{"zero concurrency", racetest.Config{HTTPEndpoint: "http://x", Duration: time.Second}},
		{"over cap", racetest.Config{
			HTTPEndpoint: "http://x", Duration: time.Second,
			Concurrency: racetest.MaxConcurrency + 1,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := racetest.Run(context.Background(), tc.cfg); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}
