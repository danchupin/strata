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

// Run drives a time-bounded mixed PUT/DELETE/Multipart workload against
// cfg.HTTPEndpoint and returns a structured Report. The workload mix is
// kept identical to the in-process RunScenario for US-001/002 (only
// SigV4 + JSON-lines events added); US-003 extends it with versioning
// flips, conditional PUTs, and DeleteObjects batches.
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
		OpsByClass: map[string]int64{"put": 0, "delete": 0, "multipart": 0},
	}

	var counters sync.Map // op class -> *int64
	for _, k := range []string{"put", "delete", "multipart"} {
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

	var wg sync.WaitGroup
	for w := 0; w < cfg.Concurrency; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(workerID)*1_000_003 + 1))
			i := 0
			for runCtx.Err() == nil {
				bucket := buckets[rng.Intn(len(buckets))]
				key := fmt.Sprintf("k-%d", rng.Intn(cfg.ObjectKeys))
				path := "/" + bucket + "/" + key
				switch rng.Intn(3) {
				case 0:
					body := []byte(fmt.Sprintf("w%d-i%d", workerID, i))
					runOp(runCtx, sink, workerID, "put", bucket, key, func() (int, error) {
						return doSigned(runCtx, client, sgn, endpoint, "PUT", path, body)
					})
					bump("put")
				case 1:
					runOp(runCtx, sink, workerID, "delete", bucket, key, func() (int, error) {
						return doSigned(runCtx, client, sgn, endpoint, "DELETE", path, nil)
					})
					bump("delete")
				case 2:
					if doMultipartCycle(runCtx, sink, client, sgn, endpoint, bucket, key, workerID, i) {
						bump("multipart")
					}
				}
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

// runOp wraps a single HTTP op with op_started + op_done events. The
// closure returns (status, err). A non-nil err is recorded on the
// op_done event; status==0 indicates a transport-level failure where
// the body was never read.
func runOp(ctx context.Context, sink EventSink, workerID int, class, bucket, key string, do func() (int, error)) {
	if ctx.Err() != nil {
		return
	}
	start := time.Now().UTC()
	sink.Emit(Event{Event: "op_started", Timestamp: start, WorkerID: workerID, Class: class, Bucket: bucket, Key: key})
	status, err := do()
	end := time.Now().UTC()
	ev := Event{
		Event:      "op_done",
		Timestamp:  end,
		WorkerID:   workerID,
		Class:      class,
		Bucket:     bucket,
		Key:        key,
		Status:     status,
		DurationMs: end.Sub(start).Milliseconds(),
	}
	if err != nil {
		ev.Error = err.Error()
	}
	sink.Emit(ev)
}

// doSigned signs (if a signer is configured) and dispatches one HTTP
// request, returning (status, err). nil err + status==0 means a
// transport error after which the response body is already drained.
func doSigned(ctx context.Context, client *http.Client, sgn *signer, endpoint, method, path string, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint+path, nil)
	if err != nil {
		return 0, err
	}
	if sgn != nil {
		if err := sgn.sign(ctx, req, body); err != nil {
			return 0, err
		}
	} else if body != nil {
		req.Body = io.NopCloser(strings.NewReader(string(body)))
		req.ContentLength = int64(len(body))
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	status := resp.StatusCode
	DrainBody(resp)
	return status, nil
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

// doMultipartCycle runs Create+UploadPart+Complete (or aborts on
// failure). Returns true on a fully-completed cycle so the caller bumps
// the multipart op counter only on the success path.
func doMultipartCycle(ctx context.Context, sink EventSink, client *http.Client, sgn *signer, endpoint, bucket, key string, workerID, iter int) bool {
	path := "/" + bucket + "/" + key
	cycleStart := time.Now().UTC()
	sink.Emit(Event{Event: "op_started", Timestamp: cycleStart, WorkerID: workerID, Class: "multipart", Bucket: bucket, Key: key})

	finishDone := func(status int, err error) {
		end := time.Now().UTC()
		ev := Event{
			Event:      "op_done",
			Timestamp:  end,
			WorkerID:   workerID,
			Class:      "multipart",
			Bucket:     bucket,
			Key:        key,
			Status:     status,
			DurationMs: end.Sub(cycleStart).Milliseconds(),
		}
		if err != nil {
			ev.Error = err.Error()
		}
		sink.Emit(ev)
	}

	initBody, status, err := doSignedRead(ctx, client, sgn, endpoint, "POST", path+"?uploads", nil)
	if err != nil || status != http.StatusOK {
		finishDone(status, err)
		return false
	}
	mm := uploadIDRE.FindStringSubmatch(string(initBody))
	if len(mm) != 2 {
		finishDone(status, fmt.Errorf("multipart: missing UploadId"))
		return false
	}
	uploadID := mm[1]
	abort := func() {
		_, _ = doSigned(ctx, client, sgn, endpoint, "DELETE",
			fmt.Sprintf("%s?uploadId=%s", path, uploadID), nil)
	}

	partBody := []byte(fmt.Sprintf("part-w%d-i%d", workerID, iter))
	partResp, partStatus, partErr := doSignedReadResp(ctx, client, sgn, endpoint, "PUT",
		fmt.Sprintf("%s?uploadId=%s&partNumber=1", path, uploadID), partBody)
	var etag string
	if partResp != nil {
		etag = strings.Trim(partResp.Header.Get("Etag"), `"`)
		DrainBody(partResp)
	}
	if partErr != nil || partStatus != http.StatusOK || etag == "" {
		abort()
		finishDone(partStatus, partErr)
		return false
	}
	completeBody := []byte(fmt.Sprintf(
		`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part></CompleteMultipartUpload>`,
		etag))
	cStatus, cErr := doSigned(ctx, client, sgn, endpoint, "POST",
		fmt.Sprintf("%s?uploadId=%s", path, uploadID), completeBody)
	if cErr != nil || cStatus != http.StatusOK {
		abort()
		finishDone(cStatus, cErr)
		return false
	}
	finishDone(http.StatusOK, nil)
	return true
}

// doSignedRead signs + dispatches a request and returns the body bytes
// alongside the status code. Used by multipart Create which needs to
// extract the UploadId from the response XML.
func doSignedRead(ctx context.Context, client *http.Client, sgn *signer, endpoint, method, path string, body []byte) ([]byte, int, error) {
	resp, status, err := doSignedReadResp(ctx, client, sgn, endpoint, method, path, body)
	if resp == nil {
		return nil, status, err
	}
	defer DrainBody(resp)
	out, _ := io.ReadAll(resp.Body)
	return out, status, err
}

// doSignedReadResp returns the *http.Response so the caller can read
// headers (e.g. Etag on UploadPart) before draining the body.
func doSignedReadResp(ctx context.Context, client *http.Client, sgn *signer, endpoint, method, path string, body []byte) (*http.Response, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint+path, nil)
	if err != nil {
		return nil, 0, err
	}
	if sgn != nil {
		if err := sgn.sign(ctx, req, body); err != nil {
			return nil, 0, err
		}
	} else if body != nil {
		req.Body = io.NopCloser(strings.NewReader(string(body)))
		req.ContentLength = int64(len(body))
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	return resp, resp.StatusCode, nil
}
