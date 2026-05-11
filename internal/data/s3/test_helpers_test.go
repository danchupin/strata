package s3

import (
	"context"
	"net/http"
	"testing"
)

// openTestBackend wires a single-cluster *Backend pointed at the
// example.invalid sentinel endpoint with the supplied transport. Used
// across every package-internal test that previously inlined the same
// 11-line Config{...} + Open boilerplate. The companion s3test.NewFixture
// (internal/data/s3/s3test) is the equivalent public-API helper for
// external test packages.
func openTestBackend(t testing.TB, rt http.RoundTripper, mods ...func(*Config)) *Backend {
	t.Helper()
	cfg := Config{
		Bucket:         "strata-test",
		Region:         "us-east-1",
		Endpoint:       "http://example.invalid",
		AccessKey:      "ak",
		SecretKey:      "sk",
		ForcePathStyle: true,
		SkipProbe:      true,
		HTTPClient:     &http.Client{Transport: rt},
	}
	for _, m := range mods {
		m(&cfg)
	}
	b, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("openTestBackend: Open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// withSSE returns a Config mod for openTestBackend that flips the legacy
// SSEMode + KMS knobs. Mirrors s3test.WithSSE for in-package tests.
func withSSE(mode, kmsKey string) func(*Config) {
	return func(c *Config) {
		c.SSEMode = mode
		c.SSEKMSKeyID = kmsKey
	}
}
