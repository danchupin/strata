package racetest_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/racetest"
	"github.com/danchupin/strata/internal/s3api"
)

// TestRunSmoke wires the racetest.Run entrypoint to an in-process gateway and
// asserts it (a) succeeds, (b) populates OpsByClass with positive counts for
// every op class in the default mix, and (c) honours Duration. The standalone
// strata-racecheck binary (US-002) exercises the same code path against a
// remote endpoint.
func TestRunSmoke(t *testing.T) {
	api := s3api.New(datamem.New(), metamem.New())
	api.Region = "default"
	ts := httptest.NewServer(http.HandlerFunc(api.ServeHTTP))
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	report, err := racetest.Run(ctx, racetest.Config{
		HTTPEndpoint: ts.URL,
		// 2s window is enough for 4 workers x ~5k ops each to hit every
		// 5%+-weighted class with overwhelming probability.
		Duration:    2 * time.Second,
		Concurrency: 4,
		BucketCount: 2,
		ObjectKeys:  4,
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
	for _, class := range []string{
		racetest.OpPut, racetest.OpGet, racetest.OpDelete, racetest.OpList,
		racetest.OpMultipart, racetest.OpVersioningFlip,
		racetest.OpConditionalPut, racetest.OpDeleteObjects,
	} {
		if report.OpsByClass[class] == 0 {
			t.Errorf("op class %q: expected >0 ops, got 0 (full=%v)", class, report.OpsByClass)
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
		{"streaming ratio > 1", racetest.Config{
			HTTPEndpoint: "http://x", Duration: time.Second,
			Concurrency: 1, StreamingRatio: 1.5,
		}},
		{"negative mix weight", racetest.Config{
			HTTPEndpoint: "http://x", Duration: time.Second,
			Concurrency: 1, Mix: map[string]float64{racetest.OpPut: -1.0},
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

// TestRunStreamingSigV4 forces 100% streaming-SigV4 routing on a gateway with
// auth.ModeRequired and a static credential, asserting the chained-HMAC verifier
// (internal/auth/streaming.go) accepts every PUT the harness emits. This is the
// regression gate for the streaming path; signed/anonymous fixed-payload is
// covered by TestRunSmoke and TestRunSmokeBinary.
func TestRunStreamingSigV4(t *testing.T) {
	const (
		ak = "AKIATESTHARNESS00000"
		sk = "secretsecretsecretsecretsecret00"
	)
	store := auth.NewStaticStore(map[string]*auth.Credential{
		ak: {AccessKey: ak, Secret: sk, Owner: "harness"},
	})
	multi := auth.NewMultiStore(time.Minute, store)
	mw := &auth.Middleware{Store: multi, Mode: auth.ModeRequired}

	api := s3api.New(datamem.New(), metamem.New())
	api.Region = "us-east-1"
	ts := httptest.NewServer(mw.Wrap(api, s3api.NewAuthDenyHandler(api.Meta)))
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Mix down to body-carrying classes only so every signing decision exercises
	// streaming. Conditional + Put both route through doPUTBody.
	report, err := racetest.Run(ctx, racetest.Config{
		HTTPEndpoint: ts.URL,
		Duration:     1 * time.Second,
		Concurrency:  2,
		BucketCount:  1,
		ObjectKeys:   3,
		AccessKey:    ak,
		SecretKey:    sk,
		Region:       "us-east-1",
		Mix: map[string]float64{
			racetest.OpPut:            0.5,
			racetest.OpConditionalPut: 0.5,
		},
		StreamingRatio: 1.0,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := report.OpsByClass[racetest.OpPut]; got == 0 {
		t.Errorf("expected at least one streaming PUT, got 0")
	}
	if got := report.OpsByClass[racetest.OpConditionalPut]; got == 0 {
		t.Errorf("expected at least one streaming conditional PUT, got 0")
	}
}
