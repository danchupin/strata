// Package s3test provides a one-call test fixture that wires a
// data/s3.Backend up to either an in-process httptest.Server or a
// caller-supplied http.RoundTripper, collapsing the inline
// `Config{Bucket, Region, Endpoint, AccessKey, SecretKey, ForcePathStyle,
// SkipProbe, HTTPClient}` boilerplate that every backend unit test would
// otherwise repeat.
//
// The default fixture registers ONE cluster ("primary") backed by the
// auto-spawned httptest.Server, and ONE class ("STANDARD") routing to a
// random bucket name. Multi-cluster scenarios add more clusters / classes
// via the WithCluster / WithClass options. Tests that need to capture or
// inject HTTP responses pass WithRoundTripper or WithHandler to take over
// the transport.
//
// A registered Cleanup hook tears down the spawned httptest.Server (if
// any) and the Backend on test exit, so callers never need to manage
// teardown explicitly.
package s3test

import (
	"crypto/rand"
	"encoding/hex"
	"maps"
	"net/http"
	"net/http/httptest"
	"testing"

	s3 "github.com/danchupin/strata/internal/data/s3"
)

// Fixture carries the wired Backend plus the identifiers a test needs to
// craft assertions about per-class routing.
type Fixture struct {
	Backend   *s3.Backend
	ClusterID string
	ClassName string
	Bucket    string
	// Server is the auto-spawned httptest.Server backing the default
	// cluster. Nil when WithRoundTripper takes over the transport.
	Server *httptest.Server
}

// Option mutates the fixture config before the Backend is built.
type Option func(*config)

type config struct {
	primaryClusterID string
	primaryClassName string
	primaryBucket    string

	extraClusters map[string]s3.S3ClusterSpec
	extraClasses  map[string]s3.ClassSpec

	handler   http.Handler
	transport http.RoundTripper

	region         string
	forcePathStyle bool
	sseMode        string
	sseKMSKeyID    string

	skipCredsCheck bool
}

// WithCluster registers an additional cluster on the fixture. Useful for
// multi-endpoint routing tests. The default "primary" cluster remains
// wired unless the same id is supplied — last write wins.
func WithCluster(id string, spec s3.S3ClusterSpec) Option {
	return func(c *config) {
		c.extraClusters[id] = spec
	}
}

// WithClass adds a per-class routing entry on top of the default
// "STANDARD" class. Use to assert per-class fan-out when more than one
// class is needed.
func WithClass(name, clusterID, bucket string) Option {
	return func(c *config) {
		c.extraClasses[name] = s3.ClassSpec{Cluster: clusterID, Bucket: bucket}
	}
}

// WithHandler swaps the default httptest.Server handler. Use when the
// test needs to inspect or hand-craft the S3-protocol response bodies.
func WithHandler(h http.Handler) Option {
	return func(c *config) { c.handler = h }
}

// WithRoundTripper bypasses the auto-spawned httptest.Server and routes
// SDK calls through the supplied http.RoundTripper. The fixture's Server
// field will be nil in this mode.
func WithRoundTripper(rt http.RoundTripper) Option {
	return func(c *config) { c.transport = rt }
}

// WithSSE configures backend-side server-side encryption knobs on the
// default cluster. Mode is one of data.SSEMode{Passthrough,Strata,Both};
// kmsKeyID is the KMS key ARN to surface in the X-Amz-Server-Side-
// Encryption-Aws-Kms-Key-Id header (empty -> AES256).
func WithSSE(mode, kmsKeyID string) Option {
	return func(c *config) {
		c.sseMode = mode
		c.sseKMSKeyID = kmsKeyID
	}
}

// WithBucket overrides the auto-generated random bucket name on the
// default class. Useful when a test asserts on a known bucket-in-URL
// substring.
func WithBucket(name string) Option {
	return func(c *config) { c.primaryBucket = name }
}

// NewFixture wires a *s3.Backend with one cluster + one class against
// either a fresh httptest.Server (default) or the caller's
// RoundTripper. Test cleanup is registered via t.Cleanup.
//
// Static AWS credentials are pinned via t.Setenv so the SDK's chain
// resolver always succeeds without IMDS / IRSA round-trips.
func NewFixture(t testing.TB, opts ...Option) *Fixture {
	t.Helper()

	cfg := config{
		primaryClusterID: "primary",
		primaryClassName: "STANDARD",
		primaryBucket:    "strata-test-" + randomSuffix(),
		extraClusters:    map[string]s3.S3ClusterSpec{},
		extraClasses:     map[string]s3.ClassSpec{},
		handler:          http.HandlerFunc(defaultHandler),
		region:           "us-east-1",
		forcePathStyle:   true,
		skipCredsCheck:   true,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	pinAWSEnv(t)

	var (
		server   *httptest.Server
		endpoint string
		client   *http.Client
	)
	switch {
	case cfg.transport != nil:
		endpoint = "http://example.invalid"
		client = &http.Client{Transport: cfg.transport}
	default:
		server = httptest.NewServer(cfg.handler)
		t.Cleanup(server.Close)
		endpoint = server.URL
	}

	primarySpec := s3.S3ClusterSpec{
		Endpoint:       endpoint,
		Region:         cfg.region,
		ForcePathStyle: cfg.forcePathStyle,
		SSEMode:        cfg.sseMode,
		SSEKMSKeyID:    cfg.sseKMSKeyID,
		Credentials:    s3.CredentialsRef{Type: s3.CredentialsChain},
	}
	clusters := map[string]s3.S3ClusterSpec{cfg.primaryClusterID: primarySpec}
	maps.Copy(clusters, cfg.extraClusters)
	classes := map[string]s3.ClassSpec{
		cfg.primaryClassName: {Cluster: cfg.primaryClusterID, Bucket: cfg.primaryBucket},
	}
	maps.Copy(classes, cfg.extraClasses)

	backend, err := s3.New(s3.Config{
		Clusters:       clusters,
		Classes:        classes,
		HTTPClient:     client,
		SkipCredsCheck: cfg.skipCredsCheck,
	})
	if err != nil {
		t.Fatalf("s3test: New: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })

	return &Fixture{
		Backend:   backend,
		ClusterID: cfg.primaryClusterID,
		ClassName: cfg.primaryClassName,
		Bucket:    cfg.primaryBucket,
		Server:    server,
	}
}

// defaultHandler answers any S3 request with a generic 200 OK + ETag so
// SDK happy paths run end-to-end without each test wiring its own
// transport. Tests that need richer behaviour pass WithHandler.
func defaultHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("ETag", `"s3test-etag"`)
	w.WriteHeader(http.StatusOK)
}

// pinAWSEnv pins synthetic AWS credentials for the duration of the test
// so the SDK's default chain resolver short-circuits to the env
// provider. Avoids IMDS / IRSA round-trips at first connect.
func pinAWSEnv(t testing.TB) {
	t.Helper()
	type setenver interface {
		Setenv(key, value string)
	}
	if se, ok := t.(setenver); ok {
		se.Setenv("AWS_ACCESS_KEY_ID", "s3test-ak")
		se.Setenv("AWS_SECRET_ACCESS_KEY", "s3test-sk")
		return
	}
	// Benchmark / harness paths without Setenv: caller is responsible
	// for pre-seeding credentials. This branch is rare; the s3test
	// fixture is fundamentally a *testing.T helper.
}

func randomSuffix() string {
	var buf [6]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}
