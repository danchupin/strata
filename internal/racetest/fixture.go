// Package racetest hosts the strata race-harness workload and its in-process
// fixture. It exists as a separate package (carved out of
// internal/s3api/race_test.go) so it can be driven by both `go test` and the
// standalone strata-racecheck binary (cmd/strata-racecheck).
//
// The package exposes two layers:
//
//   - In-process layer: Fixture + RunScenario + VerifyInvariants. The s3api
//     test files (race_test.go, race_integration_test.go) build a Fixture with
//     their backend-specific meta.Store and reuse RunScenario/VerifyInvariants
//     unchanged.
//   - External-endpoint layer: Config + Report + Run(ctx, cfg). The Run
//     entrypoint is what the strata-racecheck binary calls — it talks pure
//     HTTP and does not depend on s3api/Fixture, so it can drive a remote
//     gateway from a CI runner.
//
// Both layers share NewClient, drainBody, and the workload constants so the
// "what we hammer the gateway with" choice stays in one place.
package racetest

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/s3api"
)

// Workers / Iters / Keys are the workload-size knobs used by the in-process
// scenario. Defaults are tuned for a high density of PUT/DELETE/Multipart-
// Complete races on a small key set without making the memory-backed test
// slow under -race. Env overrides (RACE_WORKERS / RACE_ITERS / RACE_KEYS) let
// the integration soak targets dial up workload size without recompiling.
// Negative or zero env values fall through to the default.
var (
	Workers = envIntDefault("RACE_WORKERS", 32)
	Iters   = envIntDefault("RACE_ITERS", 1000)
	Keys    = envIntDefault("RACE_KEYS", 4)
)

func envIntDefault(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// uploadIDRE extracts the UploadId from a CreateMultipartUpload XML response.
// Same shape as the s3api test helper.
var uploadIDRE = regexp.MustCompile(`<UploadId>([^<]+)</UploadId>`)

// Reporter is the small testing-T-shaped interface RunScenario and
// VerifyInvariants take so the package does not depend on `testing` and stays
// usable from non-test driver code. *testing.T satisfies it implicitly.
type Reporter interface {
	Helper()
	Fatalf(format string, args ...any)
	Errorf(format string, args ...any)
	Logf(format string, args ...any)
}

// Fixture is the in-process test fixture wired by per-backend constructors.
// The same RunScenario+VerifyInvariants pair drives the memory (always-on)
// and cassandra/tikv (-tags integration) variants.
//
// AllVersions is provided by each backend so VerifyInvariants does not have
// to fight the meta.Store.ListObjectVersions 1000-row hard cap when rolling
// many thousands of writes through the harness.
type Fixture struct {
	Server      *s3api.Server
	TS          *httptest.Server
	Client      *http.Client
	MemData     *datamem.Backend
	AllVersions func(bucketID uuid.UUID) []*meta.Object
}

// Do mirrors testHarness.do but targets the fixture's pooled-connection
// http.Client so the race scenario does not exhaust ephemeral ports. Returns
// nil on transport error; the per-iteration switch handles the nil case as a
// skipped op so a flaky connection does not abort the run.
func (f *Fixture) Do(method, path string, body io.Reader) *http.Response {
	req, err := http.NewRequest(method, f.TS.URL+path, body)
	if err != nil {
		return nil
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil
	}
	return resp
}

// NewClient returns an http.Client whose Transport pool sizes are large
// enough to avoid running the kernel out of ephemeral ports under the
// 32-goroutine x 1000-op race scenario. The default http.DefaultClient has
// MaxIdleConnsPerHost = 2, which causes every other request to land in
// TIME_WAIT and exhausts the source port range on macOS within seconds.
func NewClient(workers int) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        workers * 2,
			MaxIdleConnsPerHost: workers * 2,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
	}
}

// RunScenario hammers the gateway with Workers goroutines doing Iters mixed
// PUT/DELETE/Multipart-Complete ops on Keys keys and then drains any in-flight
// multipart uploads (via ListMultipartUploads + abort) so the orphan-chunk
// invariant has a clean accounting baseline.
func RunScenario(t Reporter, f *Fixture) {
	t.Helper()
	mustStatus(t, f.Do("PUT", "/bkt", nil), 200)
	mustStatus(t, f.Do("PUT", "/bkt?versioning",
		strings.NewReader("<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>")), 200)

	var wg sync.WaitGroup
	for w := 0; w < Workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(workerID)*1_000_003 + 1))
			for i := 0; i < Iters; i++ {
				key := fmt.Sprintf("k-%d", rng.Intn(Keys))
				path := "/bkt/" + key
				switch rng.Intn(3) {
				case 0:
					body := fmt.Sprintf("w%d-i%d", workerID, i)
					DrainBody(f.Do("PUT", path, strings.NewReader(body)))
				case 1:
					DrainBody(f.Do("DELETE", path, nil))
				case 2:
					initResp := f.Do("POST", path+"?uploads", nil)
					if initResp == nil || initResp.StatusCode != 200 {
						DrainBody(initResp)
						continue
					}
					initBytes, _ := io.ReadAll(initResp.Body)
					_ = initResp.Body.Close()
					mm := uploadIDRE.FindStringSubmatch(string(initBytes))
					if len(mm) != 2 {
						continue
					}
					uploadID := mm[1]
					partBody := fmt.Sprintf("part-w%d-i%d", workerID, i)
					partResp := f.Do("PUT",
						fmt.Sprintf("%s?uploadId=%s&partNumber=1", path, uploadID),
						strings.NewReader(partBody))
					var etag string
					if partResp != nil {
						etag = strings.Trim(partResp.Header.Get("Etag"), `"`)
					}
					okPart := partResp != nil && partResp.StatusCode == 200 && etag != ""
					DrainBody(partResp)
					if !okPart {
						DrainBody(f.Do("DELETE",
							fmt.Sprintf("%s?uploadId=%s", path, uploadID), nil))
						continue
					}
					completeBody := fmt.Sprintf(
						`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part></CompleteMultipartUpload>`,
						etag)
					cResp := f.Do("POST",
						fmt.Sprintf("%s?uploadId=%s", path, uploadID),
						strings.NewReader(completeBody))
					okComplete := cResp != nil && cResp.StatusCode == 200
					DrainBody(cResp)
					if !okComplete {
						DrainBody(f.Do("DELETE",
							fmt.Sprintf("%s?uploadId=%s", path, uploadID), nil))
					}
				}
			}
		}(w)
	}
	wg.Wait()

	ctx := context.Background()
	b, err := f.Server.Meta.GetBucket(ctx, "bkt")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	uploads, err := f.Server.Meta.ListMultipartUploads(ctx, b.ID, "", 10000)
	if err != nil {
		t.Fatalf("list multipart uploads: %v", err)
	}
	for _, u := range uploads {
		manifests, err := f.Server.Meta.AbortMultipartUpload(ctx, b.ID, u.UploadID)
		if err != nil {
			t.Logf("abort upload %s: %v", u.UploadID, err)
			continue
		}
		region := f.Server.Region
		if region == "" {
			region = "default"
		}
		for _, m := range manifests {
			if m == nil {
				continue
			}
			if err := f.Server.Meta.EnqueueChunkDeletion(ctx, region, m.Chunks); err != nil {
				t.Logf("gc enqueue: %v", err)
			}
		}
	}
}

// VerifyInvariants checks the three documented invariants:
//  1. per touched key, exactly one is_latest=true row in objects
//  2. per key, the row marked latest carries an Mtime no older than any
//     other row in the chain (allowing 1s slack for stamp-before-lock)
//  3. every chunk in the data backend is referenced by some object manifest
//     OR sits in the GC queue (no orphans).
func VerifyInvariants(t Reporter, f *Fixture) {
	t.Helper()
	ctx := context.Background()
	b, err := f.Server.Meta.GetBucket(ctx, "bkt")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}

	allVersions := f.AllVersions(b.ID)
	versionsByKey := make(map[string][]*meta.Object)
	for _, v := range allVersions {
		versionsByKey[v.Key] = append(versionsByKey[v.Key], v)
	}

	liveOIDs := make(map[string]struct{})
	for key, vs := range versionsByKey {
		latestCount := 0
		var latestMtime, maxMtime time.Time
		for _, v := range vs {
			if v.IsLatest {
				latestCount++
				latestMtime = v.Mtime
			}
			if v.Mtime.After(maxMtime) {
				maxMtime = v.Mtime
			}
			CollectManifestOIDs(v.Manifest, liveOIDs)
		}
		if latestCount != 1 {
			t.Errorf("key %s: expected exactly 1 IsLatest=true row, got %d (versions=%d)",
				key, latestCount, len(vs))
		}
		// Stamp-before-lock leaves a small window where a slightly-older Mtime
		// can win the lock and end up at the head; allow 1s slack but flag any
		// gap big enough to indicate broken ordering.
		if !maxMtime.IsZero() && latestMtime.Add(time.Second).Before(maxMtime) {
			t.Errorf("key %s: latest row Mtime=%s is older than chain max=%s",
				key, latestMtime, maxMtime)
		}
	}

	region := f.Server.Region
	if region == "" {
		region = "default"
	}
	gc, err := f.Server.Meta.ListGCEntries(ctx, region, time.Now().Add(time.Hour), 1<<20)
	if err != nil {
		t.Fatalf("list gc entries: %v", err)
	}
	for _, e := range gc {
		liveOIDs[e.Chunk.OID] = struct{}{}
	}

	if f.MemData != nil {
		for _, oid := range f.MemData.ChunkOIDs() {
			if _, ok := liveOIDs[oid]; !ok {
				t.Errorf("orphan chunk in data backend: %s (not in any manifest or GC queue)", oid)
			}
		}
	}
}

// DrainBody discards and closes a response body. Safe to call with a nil
// response (no-op).
func DrainBody(resp *http.Response) {
	if resp == nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// CollectManifestOIDs walks a Manifest's chunk list and records each OID in
// the provided set. Used by VerifyInvariants and by the external-endpoint
// reconciliation paths.
func CollectManifestOIDs(m *data.Manifest, out map[string]struct{}) {
	if m == nil {
		return
	}
	for _, c := range m.Chunks {
		out[c.OID] = struct{}{}
	}
}

// mustStatus is the in-package equivalent of testHarness.mustStatus — it
// fatally fails the Reporter if the response status does not match.
func mustStatus(t Reporter, resp *http.Response, want int) {
	t.Helper()
	if resp == nil {
		t.Fatalf("status: nil response, want %d", want)
		return
	}
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("status: got %d want %d; body=%s", resp.StatusCode, want, string(body))
		return
	}
	DrainBody(resp)
}
