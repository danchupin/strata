package racetest

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// Multipart-race workload knobs. Trials drives how many fresh upload ids the
// scenario churns (more trials => more chances for the LWT/CAS window to flake
// if it is broken); Racers is the goroutine fan-out per trial. Env overrides
// (MP_RACE_TRIALS / MP_RACE_RACERS) let the integration soak dial them up
// without recompiling, mirroring the RACE_* knobs in fixture.go.
var (
	MultipartRaceTrials = envIntDefault("MP_RACE_TRIALS", 40)
	MultipartRaceRacers = envIntDefault("MP_RACE_RACERS", 8)
)

// mpRaceBucket is the dedicated, versioning-enabled bucket the multipart-race
// scenario hammers. Kept distinct from the mixed-ops "bkt" so the two
// scenarios can run against the same fixture without cross-contaminating each
// other's invariant accounting.
const mpRaceBucket = "mpr"

// RunMultipartRaceScenario exercises the two multipart-concurrency invariants
// US-007 cares about and then asserts no chunk was orphaned:
//
//  1. Complete-vs-Abort race on a SINGLE upload id: many goroutines fire
//     CompleteMultipartUpload + AbortMultipartUpload simultaneously. Exactly
//     one terminal op wins — either a Complete (the object lands as one row)
//     or an Abort (no object lands); the losers see NoSuchUpload (404, the
//     ErrMultipartInProgress/ErrMultipartNotFound mapping) or an idempotent
//     cached 200 replay. Never a second object row, never a 5xx.
//  2. Same-key/different-upload-id race: N independent uploads complete
//     concurrently onto the same key. The chain resolves to exactly one
//     IsLatest=true row whose ETag matches one of the winning uploads.
//
// Both run on a versioning-enabled bucket so every winning Complete appends a
// fresh version (no unversioned-overwrite orphaning, which is a separate
// concern from multipart-race correctness) and the orphan-chunk check has a
// clean accounting baseline: every chunk written by the workload must be
// referenced by some surviving manifest OR sit in the GC queue.
func RunMultipartRaceScenario(t Reporter, f *Fixture) {
	t.Helper()
	mustStatus(t, f.Do("PUT", "/"+mpRaceBucket, nil), 200)
	mustStatus(t, f.Do("PUT", "/"+mpRaceBucket+"?versioning",
		strings.NewReader("<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>")), 200)

	runCompleteAbortRace(t, f)
	runSameKeyDifferentUploadRace(t, f)
	verifyMultipartRaceInvariants(t, f)
}

// runCompleteAbortRace implements invariant (1): for each trial it stands up
// one upload (init + single part), then fires MultipartRaceRacers goroutines —
// half Complete, half Abort — released by a shared start channel so they hit
// the meta.Store at the same instant. It classifies every response and asserts
// at most one terminal winner.
func runCompleteAbortRace(t Reporter, f *Fixture) {
	t.Helper()
	bucketID := mustBucketID(t, f, mpRaceBucket)

	for trial := 0; trial < MultipartRaceTrials; trial++ {
		key := fmt.Sprintf("ca-%d", trial)
		uploadID, partETag, ok := initAndUploadPart(f, key, trial)
		if !ok {
			// Init/part PUT flaked at the transport layer; nothing landed so
			// there is no upload to race. Skip this trial rather than abort.
			continue
		}
		expectedETag := singlePartMultipartETag(partETag)
		completeBody := fmt.Sprintf(
			`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part></CompleteMultipartUpload>`,
			partETag)
		objPath := fmt.Sprintf("/%s/%s?uploadId=%s", mpRaceBucket, key, uploadID)

		var (
			mu                                       sync.Mutex
			complete200, complete404, completeOther  int
			abort204, abort404, abortOther, nilCount int
		)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < MultipartRaceRacers; i++ {
			wg.Add(1)
			isComplete := i%2 == 0
			go func(isComplete bool) {
				defer wg.Done()
				<-start
				var resp *http.Response
				if isComplete {
					resp = f.Do("POST", objPath, strings.NewReader(completeBody))
				} else {
					resp = f.Do("DELETE", objPath, nil)
				}
				code := 0
				if resp != nil {
					code = resp.StatusCode
				}
				DrainBody(resp)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case resp == nil:
					nilCount++
				case isComplete && code == http.StatusOK:
					complete200++
				case isComplete && code == http.StatusNotFound:
					complete404++
				case isComplete:
					completeOther++
				case code == http.StatusNoContent:
					abort204++
				case code == http.StatusNotFound:
					abort404++
				default:
					abortOther++
				}
			}(isComplete)
		}
		close(start)
		wg.Wait()

		if completeOther > 0 {
			t.Errorf("trial %d: Complete returned unexpected status (not 200/404) %d time(s)", trial, completeOther)
		}
		if abortOther > 0 {
			t.Errorf("trial %d: Abort returned unexpected status (not 204/404) %d time(s)", trial, abortOther)
		}
		if abort204 > 1 {
			t.Errorf("trial %d: %d Abort goroutines won (204); at most one may delete the upload", trial, abort204)
		}
		if nilCount > 0 {
			// Transport flake on at least one racer leaves the winner ambiguous
			// from the client's view. The structural store invariants below are
			// still meaningful, so fall through, but skip the win-attribution
			// equivalence that assumes every racer reported a status.
			t.Logf("trial %d: %d racer(s) hit a transport error; skipping win-attribution check", trial, nilCount)
		}

		versions := keyVersions(t, f, bucketID, key)
		switch {
		case complete200 >= 1:
			if len(versions) != 1 {
				t.Errorf("trial %d: Complete won but found %d object rows for key %s (want exactly 1)",
					trial, len(versions), key)
			}
			if len(versions) == 1 && versions[0].ETag != expectedETag {
				t.Errorf("trial %d: object ETag=%q, want %q", trial, versions[0].ETag, expectedETag)
			}
			if abort204 != 0 {
				t.Errorf("trial %d: both a Complete (200) and an Abort (204) won the same upload", trial)
			}
		case nilCount == 0:
			// No Complete reported success and every racer reported a status, so
			// an Abort must have raced in first and deleted the upload.
			if abort204 != 1 {
				t.Errorf("trial %d: no winning Complete yet %d winning Abort(s) (want exactly 1)", trial, abort204)
			}
			if len(versions) != 0 {
				t.Errorf("trial %d: %d object row(s) exist for key %s despite no winning Complete",
					trial, len(versions), key)
			}
		}

		assertUploadGone(t, f, bucketID, uploadID, trial)
	}
}

// runSameKeyDifferentUploadRace implements invariant (2): N independent uploads
// (distinct upload ids) onto the SAME key complete concurrently. With
// versioning enabled every winning Complete appends a version; the chain must
// resolve to exactly one IsLatest=true row.
func runSameKeyDifferentUploadRace(t Reporter, f *Fixture) {
	t.Helper()
	bucketID := mustBucketID(t, f, mpRaceBucket)
	racers := MultipartRaceRacers
	if racers < 2 {
		racers = 2
	}

	// Use a single key for the whole sub-scenario so the latest-pointer
	// contention compounds across trials (each trial adds `racers` versions to
	// the same chain). trials kept modest so AllObjectVersions stays well under
	// its row cap.
	trials := MultipartRaceTrials / 4
	if trials < 1 {
		trials = 1
	}
	const key = "sk-shared"
	winningETags := make(map[string]struct{})
	var etMu sync.Mutex

	for trial := 0; trial < trials; trial++ {
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < racers; i++ {
			wg.Add(1)
			go func(seed int) {
				defer wg.Done()
				uploadID, partETag, ok := initAndUploadPart(f, key, seed)
				if !ok {
					return
				}
				<-start
				completeBody := fmt.Sprintf(
					`<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"%s"</ETag></Part></CompleteMultipartUpload>`,
					partETag)
				resp := f.Do("POST", fmt.Sprintf("/%s/%s?uploadId=%s", mpRaceBucket, key, uploadID),
					strings.NewReader(completeBody))
				if resp != nil && resp.StatusCode == http.StatusOK {
					etMu.Lock()
					winningETags[singlePartMultipartETag(partETag)] = struct{}{}
					etMu.Unlock()
				}
				DrainBody(resp)
			}(trial*1_000 + i)
		}
		close(start)
		wg.Wait()
	}

	// Exactly one IsLatest row, and the latest ETag must be one a winning
	// Complete produced. GET (no versionId) must serve that same head.
	versions := keyVersions(t, f, bucketID, key)
	latestCount := 0
	var latestETag string
	for _, v := range versions {
		if v.IsLatest {
			latestCount++
			latestETag = v.ETag
		}
	}
	if latestCount != 1 {
		t.Errorf("same-key race: expected exactly 1 IsLatest row for key %s, got %d (versions=%d)",
			key, latestCount, len(versions))
	}
	if latestCount == 1 {
		if _, ok := winningETags[latestETag]; !ok {
			t.Errorf("same-key race: latest ETag %q matches no winning Complete", latestETag)
		}
		resp := f.Do("GET", fmt.Sprintf("/%s/%s", mpRaceBucket, key), nil)
		if resp == nil {
			t.Errorf("same-key race: GET head returned transport error")
		} else {
			if resp.StatusCode != http.StatusOK {
				t.Errorf("same-key race: GET head status=%d, want 200", resp.StatusCode)
			}
			gotETag := strings.Trim(resp.Header.Get("Etag"), `"`)
			if resp.StatusCode == http.StatusOK && gotETag != latestETag {
				t.Errorf("same-key race: GET head ETag=%q, store latest=%q", gotETag, latestETag)
			}
			DrainBody(resp)
		}
	}
}

// verifyMultipartRaceInvariants asserts the no-orphan-chunk invariant for the
// multipart-race bucket: every chunk the data backend holds must be referenced
// by a surviving object manifest OR sit in the GC queue. Aborted uploads route
// their part chunks to GC (abortMultipart -> enqueueChunks), winning Completes
// keep theirs referenced — so an orphan here is a real leak.
func verifyMultipartRaceInvariants(t Reporter, f *Fixture) {
	t.Helper()
	if f.MemData == nil {
		return
	}
	ctx := context.Background()
	bucketID := mustBucketID(t, f, mpRaceBucket)

	liveOIDs := make(map[string]struct{})
	for _, v := range f.AllVersions(bucketID) {
		CollectManifestOIDs(v.Manifest, liveOIDs)
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

	for _, oid := range f.MemData.ChunkOIDs() {
		if _, ok := liveOIDs[oid]; !ok {
			t.Errorf("orphan chunk in data backend: %s (not in any manifest or GC queue)", oid)
		}
	}
}

// initAndUploadPart stands up a fresh multipart upload on `key` and writes a
// single part, returning the upload id and the part ETag. ok=false signals a
// transport/status flake at any step (caller skips the trial). The body is
// seeded so distinct callers write distinct (md5-distinct) part payloads.
func initAndUploadPart(f *Fixture, key string, seed int) (uploadID, partETag string, ok bool) {
	path := "/" + mpRaceBucket + "/" + key
	initResp := f.Do("POST", path+"?uploads", nil)
	if initResp == nil || initResp.StatusCode != http.StatusOK {
		DrainBody(initResp)
		return "", "", false
	}
	var initBytes strings.Builder
	mm := uploadIDRE.FindStringSubmatch(readBody(initResp, &initBytes))
	if len(mm) != 2 {
		return "", "", false
	}
	uploadID = mm[1]

	partBody := fmt.Sprintf("mp-race-part-seed-%d", seed)
	partResp := f.Do("PUT",
		fmt.Sprintf("%s?uploadId=%s&partNumber=1", path, uploadID),
		strings.NewReader(partBody))
	if partResp != nil {
		partETag = strings.Trim(partResp.Header.Get("Etag"), `"`)
	}
	okPart := partResp != nil && partResp.StatusCode == http.StatusOK && partETag != ""
	DrainBody(partResp)
	if !okPart {
		DrainBody(f.Do("DELETE", fmt.Sprintf("%s?uploadId=%s", path, uploadID), nil))
		return "", "", false
	}
	return uploadID, partETag, true
}

// singlePartMultipartETag returns the strata multipart ETag for a one-part
// upload whose only part has hex-md5 ETag partETag: md5(rawmd5(part)) + "-1".
// Mirrors the computation in doMultipartCycle. Returns "" if partETag is not
// valid hex (the caller's ETag equality check then fails loudly).
func singlePartMultipartETag(partETag string) string {
	raw, err := hex.DecodeString(partETag)
	if err != nil {
		return ""
	}
	sum := md5.Sum(raw)
	return hex.EncodeToString(sum[:]) + "-1"
}

// keyVersions returns every stored version of `key` in the bucket, via the
// backend-supplied AllVersions hook (bypasses the ListObjectVersions 1000-row
// cap).
func keyVersions(t Reporter, f *Fixture, bucketID uuid.UUID, key string) []*meta.Object {
	t.Helper()
	var out []*meta.Object
	for _, v := range f.AllVersions(bucketID) {
		if v.Key == key {
			out = append(out, v)
		}
	}
	return out
}

// assertUploadGone fails if the upload id is still listed after the race — a
// winning Complete or Abort must remove it from the active-upload set.
func assertUploadGone(t Reporter, f *Fixture, bucketID uuid.UUID, uploadID string, trial int) {
	t.Helper()
	uploads, err := f.Server.Meta.ListMultipartUploads(context.Background(), bucketID, "", 10000)
	if err != nil {
		t.Fatalf("trial %d: list multipart uploads: %v", trial, err)
	}
	for _, u := range uploads {
		if u.UploadID == uploadID {
			t.Errorf("trial %d: upload %s still active after race", trial, uploadID)
			return
		}
	}
}

// mustBucketID resolves a bucket name to its id, failing the test on error.
func mustBucketID(t Reporter, f *Fixture, name string) uuid.UUID {
	t.Helper()
	b, err := f.Server.Meta.GetBucket(context.Background(), name)
	if err != nil {
		t.Fatalf("get bucket %q: %v", name, err)
	}
	return b.ID
}

// readBody reads a response body into sb and returns it as a string, closing
// the body. Used where the caller needs the body text (UploadId extraction)
// rather than a plain drain.
func readBody(resp *http.Response, sb *strings.Builder) string {
	if resp == nil {
		return ""
	}
	_, _ = io.Copy(sb, resp.Body)
	_ = resp.Body.Close()
	return sb.String()
}
