package racetest

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config drives Run against an already-running gateway over HTTP. It is the
// surface the strata-racecheck binary (cmd/strata-racecheck) speaks; the
// in-process Fixture path uses RunScenario directly with the per-test
// constants instead.
type Config struct {
	// HTTPEndpoint is the gateway base URL (e.g. http://localhost:9999).
	// No trailing slash; path-style bucket addressing is used.
	HTTPEndpoint string

	// Duration is the wall-clock window each worker runs for. Workers
	// exit the op-loop on the first iteration after Duration elapses.
	Duration time.Duration

	// Concurrency is the number of worker goroutines. Refused if greater
	// than MaxConcurrency (the empirical OOM ceiling on 7 GB
	// ubuntu-latest, per the race-harness PRD).
	Concurrency int

	// BucketCount is the number of buckets the workload spreads ops
	// across. Buckets are named "rc-bkt-<i>"; created once at start.
	BucketCount int

	// ObjectKeys is the per-bucket key cardinality. Keys are named
	// "k-<i>"; same key set is reused by every worker so PUT/DELETE
	// races stay dense.
	ObjectKeys int

	// VerifyEvery is the interval between verifier passes. Zero disables
	// the verifier (US-004 wires the verifier oracle for the
	// external-endpoint path; US-002 only establishes the schema).
	VerifyEvery time.Duration

	// AccessKey + SecretKey + Region drive SigV4 signing. Empty
	// AccessKey turns signing off (anonymous HTTP) — useful for in-process
	// tests against a STRATA_AUTH_MODE=optional gateway. Region defaults
	// to us-east-1 when empty.
	AccessKey string
	SecretKey string
	Region    string

	// ReportPath, when non-empty, is the JSON-lines events file Run
	// writes one line per op_started / op_done / inconsistency / summary
	// event into. Truncated on open.
	ReportPath string

	// EventWriter is an alternative to ReportPath for callers that
	// already hold an io.Writer (in-process tests, stdout, etc.). When
	// both are set, ReportPath wins and EventWriter is ignored.
	EventWriter io.Writer

	// Mix is the per-op-class selection weight. Keys are op classes
	// (see DefaultMix); values are non-negative floats — the weighted
	// random picker normalises them. nil/empty falls through to
	// DefaultMix. Unknown keys are ignored.
	Mix map[string]float64

	// StreamingRatio is the fraction of body-carrying ops (put,
	// conditional_put) that use streaming SigV4 instead of the
	// pre-computed-SHA fixed-payload signing flavor. 0..1; defaulted
	// to DefaultStreamingRatio when zero. Anonymous mode (no signer)
	// always uses fixed payload regardless of this value.
	StreamingRatio float64
}

// Inconsistency is one row in Report.Inconsistencies. The verifier
// oracle (US-004) populates the full diagnostic payload; for US-001/002
// only the schema is established.
type Inconsistency struct {
	Kind      string    `json:"kind"`
	Bucket    string    `json:"bucket,omitempty"`
	Key       string    `json:"key,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// Report is the structured outcome of a Run. The summary event mirrors
// these fields so a downstream tool can read either the JSON-lines tail
// or the in-memory return value, whichever is more convenient.
type Report struct {
	StartedAt       time.Time        `json:"started_at"`
	EndedAt         time.Time        `json:"ended_at"`
	Duration        time.Duration    `json:"duration_ns"`
	OpsByClass      map[string]int64 `json:"ops_by_class"`
	Inconsistencies []Inconsistency  `json:"inconsistencies"`
}

// MaxConcurrency is the hard cap enforced by Run, matching the
// racecheck script contract from the PRD (US-006). Set high enough for
// nightly throughput yet low enough that 7 GB ubuntu-latest does not
// OOM.
const MaxConcurrency = 64

// DefaultStreamingRatio is the share of body-carrying ops (put,
// conditional_put) that take the streaming-SigV4 path when a signer
// is configured. Picked at ~50% per the cycle PRD: high enough to
// exercise the chained-HMAC verifier, low enough that pre-computed-SHA
// regressions still surface.
const DefaultStreamingRatio = 0.5

// DefaultMix is the per-op-class workload mix used when Config.Mix is
// empty. Every class is exercised at ≥5%; versioning_flip is weighted
// at ≥10% per the PRD's Cassandra-LWT hot-path note. Sums to 1.0; the
// weighted picker normalises so any rebalanced custom mix works.
var DefaultMix = map[string]float64{
	OpPut:            0.20,
	OpGet:            0.10,
	OpDelete:         0.10,
	OpList:           0.05,
	OpMultipart:      0.10,
	OpVersioningFlip: 0.10,
	OpConditionalPut: 0.20,
	OpDeleteObjects:  0.15,
}

// Op-class identifiers. Used as keys in Config.Mix, OpsByClass, and the
// JSON-lines event Class field — keep these in sync with consumers
// (scripts/racecheck/summarize.sh, US-008).
const (
	OpPut            = "put"
	OpGet            = "get"
	OpDelete         = "delete"
	OpList           = "list"
	OpMultipart      = "multipart"
	OpVersioningFlip = "versioning_flip"
	OpConditionalPut = "conditional_put"
	OpDeleteObjects  = "delete_objects"
)

// allOpClasses is the canonical iteration order for OpsByClass /
// counters. Anything in Config.Mix not in this list is silently
// dropped — keeps unknown keys from skewing the picker.
var allOpClasses = []string{
	OpPut, OpGet, OpDelete, OpList,
	OpMultipart, OpVersioningFlip, OpConditionalPut, OpDeleteObjects,
}

// Run drives a time-bounded mixed workload (PUT/GET/DELETE/list/Multipart/
// versioning-flip/conditional-PUT/DeleteObjects) against cfg.HTTPEndpoint
// and returns a structured Report. Per-class weight is configurable via
// Config.Mix; body-carrying ops randomly route through streaming-SigV4
// at Config.StreamingRatio when a signer is configured.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	if cfg.HTTPEndpoint == "" {
		return nil, fmt.Errorf("racetest: HTTPEndpoint required")
	}
	if cfg.Duration <= 0 {
		return nil, fmt.Errorf("racetest: Duration must be > 0")
	}
	if cfg.Concurrency <= 0 {
		return nil, fmt.Errorf("racetest: Concurrency must be > 0")
	}
	if cfg.Concurrency > MaxConcurrency {
		return nil, fmt.Errorf("racetest: Concurrency %d exceeds MaxConcurrency %d",
			cfg.Concurrency, MaxConcurrency)
	}
	if cfg.BucketCount <= 0 {
		cfg.BucketCount = 1
	}
	if cfg.ObjectKeys <= 0 {
		cfg.ObjectKeys = 4
	}
	if cfg.StreamingRatio < 0 || cfg.StreamingRatio > 1 {
		return nil, fmt.Errorf("racetest: StreamingRatio %.3f out of [0,1]", cfg.StreamingRatio)
	}
	if cfg.StreamingRatio == 0 {
		cfg.StreamingRatio = DefaultStreamingRatio
	}

	picker, err := newOpPicker(cfg.Mix)
	if err != nil {
		return nil, err
	}

	endpoint := strings.TrimRight(cfg.HTTPEndpoint, "/")
	client := NewClient(cfg.Concurrency)
	sgn := newSigner(cfg.AccessKey, cfg.SecretKey, cfg.Region)

	sink, err := buildSink(cfg)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sink.Close() }()

	buckets := make([]string, cfg.BucketCount)
	for i := 0; i < cfg.BucketCount; i++ {
		buckets[i] = fmt.Sprintf("rc-bkt-%d", i)
		if err := ensureBucket(ctx, client, sgn, endpoint, buckets[i]); err != nil {
			return nil, fmt.Errorf("racetest: ensure bucket %s: %w", buckets[i], err)
		}
		if err := enableVersioning(ctx, client, sgn, endpoint, buckets[i]); err != nil {
			return nil, fmt.Errorf("racetest: enable versioning %s: %w", buckets[i], err)
		}
	}

	report := &Report{
		StartedAt:  time.Now().UTC(),
		OpsByClass: make(map[string]int64, len(allOpClasses)),
	}
	for _, k := range allOpClasses {
		report.OpsByClass[k] = 0
	}

	var counters sync.Map // op class -> *int64
	for _, k := range allOpClasses {
		v := int64(0)
		counters.Store(k, &v)
	}
	bump := func(class string) {
		if v, ok := counters.Load(class); ok {
			atomic.AddInt64(v.(*int64), 1)
		}
	}

	deadline := time.Now().Add(cfg.Duration)
	runCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	wctx := &workerCtx{
		ctx:            runCtx,
		client:         client,
		sgn:            sgn,
		endpoint:       endpoint,
		sink:           sink,
		buckets:        buckets,
		objectKeys:     cfg.ObjectKeys,
		streamingRatio: cfg.StreamingRatio,
		bump:           bump,
	}

	var wg sync.WaitGroup
	for w := 0; w < cfg.Concurrency; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(workerID)*1_000_003 + 1))
			i := 0
			for runCtx.Err() == nil {
				class := picker.pick(rng)
				wctx.runOnce(workerID, i, rng, class)
				i++
			}
		}(w)
	}
	wg.Wait()

	report.EndedAt = time.Now().UTC()
	report.Duration = report.EndedAt.Sub(report.StartedAt)
	counters.Range(func(k, v any) bool {
		report.OpsByClass[k.(string)] = atomic.LoadInt64(v.(*int64))
		return true
	})

	sink.Emit(Event{
		Event:     "summary",
		Timestamp: report.EndedAt,
		Summary: map[string]any{
			"started_at":            report.StartedAt,
			"ended_at":              report.EndedAt,
			"duration_ns":           int64(report.Duration),
			"ops_by_class":          report.OpsByClass,
			"inconsistencies_count": len(report.Inconsistencies),
		},
	})

	return report, nil
}

// buildSink picks the configured event sink. ReportPath beats
// EventWriter; both empty falls through to a no-op sink.
func buildSink(cfg Config) (EventSink, error) {
	if cfg.ReportPath != "" {
		return newFileSink(cfg.ReportPath)
	}
	if cfg.EventWriter != nil {
		return newWriterSink(cfg.EventWriter), nil
	}
	return nopSink{}, nil
}

func ensureBucket(ctx context.Context, client *http.Client, sgn *signer, endpoint, bucket string) error {
	status, err := doSigned(ctx, client, sgn, endpoint, "PUT", "/"+bucket, nil)
	if err != nil {
		return fmt.Errorf("transport: %w", err)
	}
	// 200 OK on create; 409 BucketAlreadyOwnedByYou is also acceptable
	// on retry runs against a stack that wasn't torn down between
	// invocations.
	if status == http.StatusOK || status == http.StatusConflict {
		return nil
	}
	return fmt.Errorf("status %d", status)
}

func enableVersioning(ctx context.Context, client *http.Client, sgn *signer, endpoint, bucket string) error {
	body := []byte("<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>")
	status, err := doSigned(ctx, client, sgn, endpoint, "PUT", "/"+bucket+"?versioning", body)
	if err != nil {
		return fmt.Errorf("transport: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("status %d", status)
	}
	return nil
}
