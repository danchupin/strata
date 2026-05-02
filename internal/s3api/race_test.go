package s3api_test

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/s3api"
)

// raceWorkers / raceIters / raceKeys are tuned to produce a high density of
// PUT/DELETE/Multipart-Complete races on a small key set without making the
// memory-backed test slow under -race. The cassandra- and tikv-tagged
// variants reuse the same constants.
//
// Defaults match the original prd-race-harness US-035 shape; env overrides
// (RACE_WORKERS / RACE_ITERS / RACE_KEYS) let the integration soak targets
// (e.g. `make race-soak-tikv`) dial up workload size without recompiling.
// Negative or zero env values fall through to the default — the make
// targets do the explicit "scale" choice.
var (
	raceWorkers = envIntDefault("RACE_WORKERS", 32)
	raceIters   = envIntDefault("RACE_ITERS", 1000)
	raceKeys    = envIntDefault("RACE_KEYS", 4)
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

// raceFixture is the test fixture wired by the per-backend constructor. The
// same runRaceScenario+verifyRaceInvariants pair drives both the memory
// (always-on) and cassandra (-tags integration) variants.
//
// allVersions is provided by each backend so verifyRaceInvariants does not
// have to fight the meta.Store.ListObjectVersions 1000-row hard cap when
// rolling many thousands of writes through the harness.
type raceFixture struct {
	server      *s3api.Server
	ts          *httptest.Server
	client      *http.Client
	memData     *datamem.Backend
	allVersions func(bucketID uuid.UUID) []*meta.Object
}

// newRaceClient returns an http.Client whose Transport pool sizes are large
// enough to avoid running the kernel out of ephemeral ports under the
// 32-goroutine × 1000-op race scenario. The default http.DefaultClient has
// MaxIdleConnsPerHost = 2, which causes every other request to land in
// TIME_WAIT and exhausts the source port range on macOS within seconds.
func newRaceClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        raceWorkers * 2,
			MaxIdleConnsPerHost: raceWorkers * 2,
			IdleConnTimeout:     90 * time.Second,
			DisableCompression:  true,
		},
	}
}

func newMemoryRaceFixture(t *testing.T) *raceFixture {
	t.Helper()
	d := datamem.New()
	m := metamem.New()
	api := s3api.New(d, m)
	api.Region = "default"
	api.Master = harnessMasterProvider{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.Header.Get(testPrincipalHeader); p != "" {
			ctx := auth.WithAuth(r.Context(), &auth.AuthInfo{Owner: p, AccessKey: p})
			r = r.WithContext(ctx)
		}
		api.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &raceFixture{
		server:      api,
		ts:          ts,
		client:      newRaceClient(),
		memData:     d,
		allVersions: m.AllObjectVersions,
	}
}

func TestRaceMixedOpsMemory(t *testing.T) {
	f := newMemoryRaceFixture(t)
	runRaceScenario(t, f)
	verifyRaceInvariants(t, f)
}

// runRaceScenario hammers the gateway with raceWorkers goroutines doing
// raceIters mixed PUT/DELETE/Multipart-Complete ops on raceKeys keys and
// then drains any in-flight multipart uploads (via ListMultipartUploads +
// abort) so the orphan-chunk invariant has a clean accounting baseline.
func runRaceScenario(t *testing.T, f *raceFixture) {
	t.Helper()
	mustStatus(t, f.do("PUT", "/bkt", nil), 200)
	mustStatus(t, f.do("PUT", "/bkt?versioning",
		strings.NewReader("<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>")), 200)

	var wg sync.WaitGroup
	for w := 0; w < raceWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(workerID)*1_000_003 + 1))
			for i := 0; i < raceIters; i++ {
				key := fmt.Sprintf("k-%d", rng.Intn(raceKeys))
				path := "/bkt/" + key
				switch rng.Intn(3) {
				case 0:
					body := fmt.Sprintf("w%d-i%d", workerID, i)
					drainBody(f.do("PUT", path, strings.NewReader(body)))
				case 1:
					drainBody(f.do("DELETE", path, nil))
				case 2:
					initResp := f.do("POST", path+"?uploads", nil)
					if initResp == nil || initResp.StatusCode != 200 {
						drainBody(initResp)
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
					partResp := f.do("PUT",
						fmt.Sprintf("%s?uploadId=%s&partNumber=1", path, uploadID),
						strings.NewReader(partBody))
					var etag string
					if partResp != nil {
						etag = strings.Trim(partResp.Header.Get("Etag"), `"`)
					}
					okPart := partResp != nil && partResp.StatusCode == 200 && etag != ""
					drainBody(partResp)
					if !okPart {
						drainBody(f.do("DELETE",
							fmt.Sprintf("%s?uploadId=%s", path, uploadID), nil))
						continue
					}
					completeBody := fmt.Sprintf(
						`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part></CompleteMultipartUpload>`,
						etag)
					cResp := f.do("POST",
						fmt.Sprintf("%s?uploadId=%s", path, uploadID),
						strings.NewReader(completeBody))
					okComplete := cResp != nil && cResp.StatusCode == 200
					drainBody(cResp)
					if !okComplete {
						drainBody(f.do("DELETE",
							fmt.Sprintf("%s?uploadId=%s", path, uploadID), nil))
					}
				}
			}
		}(w)
	}
	wg.Wait()

	ctx := context.Background()
	b, err := f.server.Meta.GetBucket(ctx, "bkt")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}
	uploads, err := f.server.Meta.ListMultipartUploads(ctx, b.ID, "", 10000)
	if err != nil {
		t.Fatalf("list multipart uploads: %v", err)
	}
	for _, u := range uploads {
		manifests, err := f.server.Meta.AbortMultipartUpload(ctx, b.ID, u.UploadID)
		if err != nil {
			t.Logf("abort upload %s: %v", u.UploadID, err)
			continue
		}
		region := f.server.Region
		if region == "" {
			region = "default"
		}
		for _, m := range manifests {
			if m == nil {
				continue
			}
			if err := f.server.Meta.EnqueueChunkDeletion(ctx, region, m.Chunks); err != nil {
				t.Logf("gc enqueue: %v", err)
			}
		}
	}
}

// verifyRaceInvariants checks the three documented invariants:
//   1) per touched key, exactly one is_latest=true row in objects
//   2) per key, the row marked latest carries an Mtime no older than any
//      other row in the chain (allowing 1s slack for stamp-before-lock)
//   3) every chunk in the data backend is referenced by some object manifest
//      OR sits in the GC queue (no orphans).
func verifyRaceInvariants(t *testing.T, f *raceFixture) {
	t.Helper()
	ctx := context.Background()
	b, err := f.server.Meta.GetBucket(ctx, "bkt")
	if err != nil {
		t.Fatalf("get bucket: %v", err)
	}

	allVersions := f.allVersions(b.ID)
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
			collectManifestOIDs(v.Manifest, liveOIDs)
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

	region := f.server.Region
	if region == "" {
		region = "default"
	}
	gc, err := f.server.Meta.ListGCEntries(ctx, region, time.Now().Add(time.Hour), 1<<20)
	if err != nil {
		t.Fatalf("list gc entries: %v", err)
	}
	for _, e := range gc {
		liveOIDs[e.Chunk.OID] = struct{}{}
	}

	if f.memData != nil {
		for _, oid := range f.memData.ChunkOIDs() {
			if _, ok := liveOIDs[oid]; !ok {
				t.Errorf("orphan chunk in data backend: %s (not in any manifest or GC queue)", oid)
			}
		}
	}
}

func drainBody(resp *http.Response) {
	if resp == nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func collectManifestOIDs(m *data.Manifest, out map[string]struct{}) {
	if m == nil {
		return
	}
	for _, c := range m.Chunks {
		out[c.OID] = struct{}{}
	}
}

// do mirrors testHarness.do but targets the fixture's pooled-connection
// http.Client so the race scenario does not exhaust ephemeral ports.
// Returns nil on transport error; the per-iteration switch handles the nil
// case as a skipped op so a flaky connection does not abort the run.
func (f *raceFixture) do(method, path string, body io.Reader) *http.Response {
	req, err := http.NewRequest(method, f.ts.URL+path, body)
	if err != nil {
		return nil
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil
	}
	return resp
}

