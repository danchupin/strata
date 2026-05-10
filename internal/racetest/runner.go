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

	// Duration is the wall-clock window each worker runs for. Workers exit
	// the op-loop on the first iteration after Duration elapses.
	Duration time.Duration

	// Concurrency is the number of worker goroutines. Refused if greater
	// than 64 (the empirical OOM ceiling on 7 GB ubuntu-latest, per the
	// race-harness PRD).
	Concurrency int

	// BucketCount is the number of buckets the workload spreads ops across.
	// Buckets are named "rc-bkt-<i>"; created once at start.
	BucketCount int

	// ObjectKeys is the per-bucket key cardinality. Keys are named
	// "k-<i>"; same key set is reused by every worker so PUT/DELETE races
	// stay dense.
	ObjectKeys int

	// VerifyEvery is the interval between verifier passes. Zero disables
	// the verifier (the in-process scenario is the canonical
	// invariant-checker today; US-004 wires the verifier oracle for the
	// external-endpoint path).
	VerifyEvery time.Duration
}

// Inconsistency is one row in Report.Inconsistencies. The verifier oracle
// (US-004) populates the full diagnostic payload; for US-001 only the
// schema is established.
type Inconsistency struct {
	Kind      string    `json:"kind"`
	Bucket    string    `json:"bucket,omitempty"`
	Key       string    `json:"key,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// Report is the structured outcome of a Run. JSON-lines event emission
// (op_started / op_done / inconsistency / summary) is wired by US-002 and
// US-008; for US-001 the in-memory aggregate is the deliverable.
type Report struct {
	StartedAt       time.Time        `json:"started_at"`
	EndedAt         time.Time        `json:"ended_at"`
	Duration        time.Duration    `json:"duration_ns"`
	OpsByClass      map[string]int64 `json:"ops_by_class"`
	Inconsistencies []Inconsistency  `json:"inconsistencies"`
}

// MaxConcurrency is the hard cap enforced by Run, matching the racecheck
// script contract from the PRD (US-006). Set high enough for nightly
// throughput yet low enough that 7 GB ubuntu-latest does not OOM.
const MaxConcurrency = 64

// Run drives a time-bounded mixed PUT/DELETE/Multipart workload against
// cfg.HTTPEndpoint and returns a structured Report. The workload mix is
// kept identical to the in-process RunScenario for US-001 (relocation
// only); US-003 extends it with versioning flips, conditional PUTs, and
// DeleteObjects batches.
//
// Run is anonymous (no SigV4) at this stage — US-002 wires
// access-key/secret-key signing so the harness can talk to a gateway in
// signed mode. Today Run targets a gateway running with
// STRATA_AUTH_MODE=optional.
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

	buckets := make([]string, cfg.BucketCount)
	for i := 0; i < cfg.BucketCount; i++ {
		buckets[i] = fmt.Sprintf("rc-bkt-%d", i)
		if err := ensureBucket(ctx, client, endpoint, buckets[i]); err != nil {
			return nil, fmt.Errorf("racetest: ensure bucket %s: %w", buckets[i], err)
		}
		if err := enableVersioning(ctx, client, endpoint, buckets[i]); err != nil {
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
					body := fmt.Sprintf("w%d-i%d", workerID, i)
					DrainBody(doRequest(runCtx, client, endpoint, "PUT", path, strings.NewReader(body)))
					bump("put")
				case 1:
					DrainBody(doRequest(runCtx, client, endpoint, "DELETE", path, nil))
					bump("delete")
				case 2:
					if doMultipartCycle(runCtx, client, endpoint, path, workerID, i) {
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
	return report, nil
}

func doRequest(ctx context.Context, client *http.Client, endpoint, method, path string, body io.Reader) *http.Response {
	req, err := http.NewRequestWithContext(ctx, method, endpoint+path, body)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	return resp
}

func ensureBucket(ctx context.Context, client *http.Client, endpoint, bucket string) error {
	resp := doRequest(ctx, client, endpoint, "PUT", "/"+bucket, nil)
	if resp == nil {
		return fmt.Errorf("transport error creating bucket")
	}
	defer DrainBody(resp)
	// 200 OK on create; 409 BucketAlreadyOwnedByYou is also acceptable on
	// retry runs against a stack that wasn't torn down between invocations.
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusConflict {
		return nil
	}
	return fmt.Errorf("status %d", resp.StatusCode)
}

func enableVersioning(ctx context.Context, client *http.Client, endpoint, bucket string) error {
	body := strings.NewReader("<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>")
	resp := doRequest(ctx, client, endpoint, "PUT", "/"+bucket+"?versioning", body)
	if resp == nil {
		return fmt.Errorf("transport error enabling versioning")
	}
	defer DrainBody(resp)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func doMultipartCycle(ctx context.Context, client *http.Client, endpoint, path string, workerID, iter int) bool {
	initResp := doRequest(ctx, client, endpoint, "POST", path+"?uploads", nil)
	if initResp == nil || initResp.StatusCode != http.StatusOK {
		DrainBody(initResp)
		return false
	}
	initBytes, _ := io.ReadAll(initResp.Body)
	_ = initResp.Body.Close()
	mm := uploadIDRE.FindStringSubmatch(string(initBytes))
	if len(mm) != 2 {
		return false
	}
	uploadID := mm[1]
	partBody := fmt.Sprintf("part-w%d-i%d", workerID, iter)
	partResp := doRequest(ctx, client, endpoint, "PUT",
		fmt.Sprintf("%s?uploadId=%s&partNumber=1", path, uploadID),
		strings.NewReader(partBody))
	var etag string
	if partResp != nil {
		etag = strings.Trim(partResp.Header.Get("Etag"), `"`)
	}
	okPart := partResp != nil && partResp.StatusCode == http.StatusOK && etag != ""
	DrainBody(partResp)
	if !okPart {
		DrainBody(doRequest(ctx, client, endpoint, "DELETE",
			fmt.Sprintf("%s?uploadId=%s", path, uploadID), nil))
		return false
	}
	completeBody := fmt.Sprintf(
		`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part></CompleteMultipartUpload>`,
		etag)
	cResp := doRequest(ctx, client, endpoint, "POST",
		fmt.Sprintf("%s?uploadId=%s", path, uploadID),
		strings.NewReader(completeBody))
	okComplete := cResp != nil && cResp.StatusCode == http.StatusOK
	DrainBody(cResp)
	if !okComplete {
		DrainBody(doRequest(ctx, client, endpoint, "DELETE",
			fmt.Sprintf("%s?uploadId=%s", path, uploadID), nil))
		return false
	}
	return true
}
