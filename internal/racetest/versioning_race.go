package racetest

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// Versioning-race workload knobs. Trials drives how many fresh keys the
// PUT-vs-delete-marker race churns and how many CAS rounds run; Racers is the
// goroutine fan-out per trial. Env overrides (VER_RACE_TRIALS /
// VER_RACE_RACERS) dial the integration soak up without recompiling, mirroring
// the RACE_* / MP_RACE_* knobs.
var (
	VersioningRaceTrials = envIntDefault("VER_RACE_TRIALS", 40)
	VersioningRaceRacers = envIntDefault("VER_RACE_RACERS", 8)
)

// Dedicated buckets, one per sub-scenario, so their invariant accounting never
// cross-contaminates and they can share a single fixture with the mixed-ops /
// multipart-race scenarios.
const (
	verEnabledBucket   = "verx" // versioning-enabled: PUT-vs-delete-marker
	verCASBucket       = "casx" // versioning-enabled: SetObjectStorage CAS
	verSuspendedBucket = "susx" // versioning-suspended: replace-null
)

// RunVersioningRaceScenario exercises the three versioning/CAS contention
// invariants US-008 cares about, then asserts no chunk was orphaned across the
// buckets it touches:
//
//  1. Concurrent PUT + DELETE-marker on a versioned key: the chain stays
//     monotonic, exactly one row is IsLatest=true, and no racer ever sees a
//     5xx. (Memory makes IsLatest structural — head==latest; the real LWT
//     contention test is the TiKV/Cassandra integration variant.)
//  2. CAS contention on SetObjectStorage (lifecycle-transition vs client
//     overwrite, mirrored exactly from internal/lifecycle/worker.go): N racers
//     read the same expectedClass, each writes a fresh manifest under a
//     distinct newClass, and flips via the LWT-backed CAS. Exactly one applies;
//     the read-back manifest+class is the winner's; every loser's freshly
//     written chunks land in the GC queue (the documented lifecycle-CAS
//     invariant), and the winner's superseded chunks land there too.
//  3. Suspended-versioning replace-null under contention: concurrent
//     unversioned PUT (replace null row) + unversioned DELETE
//     (DeleteObjectNullReplacement) on one key. At most one null-versioned row
//     survives, exactly one row is IsLatest, GET ?versionId=null resolves to a
//     single deterministic status (never a 5xx, stable across repeated reads).
//     Empty bodies keep this sub-scenario chunk-free so the global orphan check
//     stays clean (suspended-mode replace-null does not GC the superseded null
//     chunks in the memory backend — a separate, documented concern).
func RunVersioningRaceScenario(t Reporter, f *Fixture) {
	t.Helper()
	for _, b := range []string{verEnabledBucket, verCASBucket} {
		mustStatus(t, f.Do("PUT", "/"+b, nil), 200)
		mustStatus(t, f.Do("PUT", "/"+b+"?versioning",
			strings.NewReader("<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>")), 200)
	}
	mustStatus(t, f.Do("PUT", "/"+verSuspendedBucket, nil), 200)
	mustStatus(t, f.Do("PUT", "/"+verSuspendedBucket+"?versioning",
		strings.NewReader("<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>")), 200)

	runPutDeleteMarkerRace(t, f)
	runSetObjectStorageCASRace(t, f)
	runSuspendedReplaceNullRace(t, f)
	verifyVersioningRaceInvariants(t, f)
}

// runPutDeleteMarkerRace implements invariant (1): for each trial a fresh key
// is hammered by VersioningRaceRacers goroutines, half issuing PUT (new
// version) and half DELETE (delete marker), released by a shared start channel.
// No racer may see a 5xx, and afterwards the chain must carry exactly one
// IsLatest row whose Mtime is no older than the chain max (1s slack).
func runPutDeleteMarkerRace(t Reporter, f *Fixture) {
	t.Helper()
	bucketID := mustBucketID(t, f, verEnabledBucket)

	for trial := 0; trial < VersioningRaceTrials; trial++ {
		key := fmt.Sprintf("pdm-%d", trial)
		path := "/" + verEnabledBucket + "/" + key

		var (
			mu        sync.Mutex
			serverErr int
		)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < VersioningRaceRacers; i++ {
			wg.Add(1)
			isPut := i%2 == 0
			go func(isPut bool, seed int) {
				defer wg.Done()
				<-start
				var resp *http.Response
				if isPut {
					body := fmt.Sprintf("pdm-body-%d", seed)
					resp = f.Do("PUT", path, strings.NewReader(body))
				} else {
					resp = f.Do("DELETE", path, nil)
				}
				if resp == nil {
					return
				}
				code := resp.StatusCode
				DrainBody(resp)
				if code >= 500 {
					mu.Lock()
					serverErr++
					mu.Unlock()
				}
			}(isPut, trial*1_000+i)
		}
		close(start)
		wg.Wait()

		if serverErr > 0 {
			t.Errorf("trial %d: %d racer(s) saw a 5xx on PUT/DELETE-marker race", trial, serverErr)
		}

		versions := keyVersions(t, f, bucketID, key)
		if len(versions) == 0 {
			// Every racer flaked at the transport layer; nothing landed.
			continue
		}
		assertSingleLatestMonotonic(t, f, bucketID, key, fmt.Sprintf("put-delete trial %d", trial))
	}
}

// runSetObjectStorageCASRace implements invariant (2). It seeds one versioned
// object, then fires VersioningRaceRacers goroutines that each mirror the
// lifecycle worker's transition path against the SAME (key, versionID): write a
// fresh manifest under a distinct newClass, then CAS via SetObjectStorage with
// the shared expectedClass. Exactly one applies. The read-back object must be
// the winner's; the loser chunks and the winner's superseded chunks must all be
// in the GC queue (no orphan, no double-applied).
func runSetObjectStorageCASRace(t Reporter, f *Fixture) {
	t.Helper()
	ctx := context.Background()
	bucketID := mustBucketID(t, f, verCASBucket)
	region := f.Server.Region
	if region == "" {
		region = "default"
	}

	const key = "cas-key"
	putResp := f.Do("PUT", "/"+verCASBucket+"/"+key, strings.NewReader("cas-seed-object"))
	if putResp == nil || putResp.StatusCode != http.StatusOK {
		DrainBody(putResp)
		t.Fatalf("CAS race: seed PUT failed")
		return
	}
	versionID := putResp.Header.Get("X-Amz-Version-Id")
	DrainBody(putResp)
	if versionID == "" {
		t.Fatalf("CAS race: seed PUT returned no version id")
		return
	}

	seed, err := f.Server.Meta.GetObject(ctx, bucketID, key, versionID)
	if err != nil {
		t.Fatalf("CAS race: get seed object: %v", err)
		return
	}
	expectedClass := seed.StorageClass
	var oldOIDs []string
	if seed.Manifest != nil {
		for _, c := range seed.Manifest.Chunks {
			oldOIDs = append(oldOIDs, c.OID)
		}
	}

	racers := VersioningRaceRacers
	if racers < 2 {
		racers = 2
	}

	type result struct {
		applied   bool
		newClass  string
		chunkOIDs []string
	}
	results := make([]result, racers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			newClass := fmt.Sprintf("CASCLASS-%d", idx)
			body := []byte(fmt.Sprintf("cas-rewrite-%d", idx))
			newManifest, perr := f.Server.Data.PutChunks(ctx, bytes.NewReader(body), newClass)
			if perr != nil {
				t.Errorf("CAS race: racer %d put chunks: %v", idx, perr)
				return
			}
			oids := make([]string, 0, len(newManifest.Chunks))
			for _, c := range newManifest.Chunks {
				oids = append(oids, c.OID)
			}
			<-start
			applied, serr := f.Server.Meta.SetObjectStorage(ctx, bucketID, key, versionID, expectedClass, newClass, newManifest)
			if serr != nil {
				t.Errorf("CAS race: racer %d SetObjectStorage: %v", idx, serr)
				// On error mirror the worker: discard the freshly written chunks.
				_ = f.Server.Meta.EnqueueChunkDeletion(ctx, region, newManifest.Chunks)
				return
			}
			// Mirror internal/lifecycle/worker.go exactly: applied=false discards
			// the fresh write; applied=true discards the superseded old chunks.
			if applied {
				_ = f.Server.Meta.EnqueueChunkDeletion(ctx, region, seed.Manifest.Chunks)
			} else {
				_ = f.Server.Meta.EnqueueChunkDeletion(ctx, region, newManifest.Chunks)
			}
			results[idx] = result{applied: applied, newClass: newClass, chunkOIDs: oids}
		}(i)
	}
	close(start)
	wg.Wait()

	appliedCount := 0
	var winner result
	for _, r := range results {
		if r.applied {
			appliedCount++
			winner = r
		}
	}
	if appliedCount != 1 {
		t.Errorf("CAS race: expected exactly 1 SetObjectStorage to apply, got %d", appliedCount)
		return
	}

	got, err := f.Server.Meta.GetObject(ctx, bucketID, key, versionID)
	if err != nil {
		t.Fatalf("CAS race: read-back: %v", err)
		return
	}
	if got.StorageClass != winner.newClass {
		t.Errorf("CAS race: read-back class=%q, want winner class %q", got.StorageClass, winner.newClass)
	}
	winnerOIDs := make(map[string]struct{}, len(winner.chunkOIDs))
	for _, o := range winner.chunkOIDs {
		winnerOIDs[o] = struct{}{}
	}
	if got.Manifest == nil {
		t.Errorf("CAS race: read-back manifest is nil")
	} else {
		if len(got.Manifest.Chunks) != len(winner.chunkOIDs) {
			t.Errorf("CAS race: read-back manifest has %d chunks, want %d (winner)",
				len(got.Manifest.Chunks), len(winner.chunkOIDs))
		}
		for _, c := range got.Manifest.Chunks {
			if _, ok := winnerOIDs[c.OID]; !ok {
				t.Errorf("CAS race: read-back manifest carries OID %q not written by the winner", c.OID)
			}
		}
	}

	// Every loser's freshly written chunks AND the winner's superseded original
	// chunks must be queued for GC — none may dangle.
	gcOIDs := gcQueueOIDs(t, f, region)
	for i, r := range results {
		if r.applied {
			continue
		}
		for _, oid := range r.chunkOIDs {
			if _, ok := gcOIDs[oid]; !ok {
				t.Errorf("CAS race: loser %d chunk %q not in GC queue (orphaned write)", i, oid)
			}
		}
	}
	for _, oid := range oldOIDs {
		if _, ok := gcOIDs[oid]; !ok {
			t.Errorf("CAS race: winner did not GC superseded original chunk %q", oid)
		}
	}
}

// runSuspendedReplaceNullRace implements invariant (3). On a suspended bucket,
// concurrent unversioned PUT (replace null) + unversioned DELETE
// (DeleteObjectNullReplacement) race on one key for VersioningRaceTrials
// rounds. After each round at most one null-versioned row may exist, exactly
// one row is IsLatest, and GET ?versionId=null resolves to a single
// deterministic status — never a 5xx, stable across repeated reads. Bodies are
// empty so the workload writes no chunks (keeps the global orphan check clean).
func runSuspendedReplaceNullRace(t Reporter, f *Fixture) {
	t.Helper()
	bucketID := mustBucketID(t, f, verSuspendedBucket)
	const key = "null-key"
	path := "/" + verSuspendedBucket + "/" + key
	racers := VersioningRaceRacers
	if racers < 2 {
		racers = 2
	}

	for trial := 0; trial < VersioningRaceTrials; trial++ {
		var (
			mu        sync.Mutex
			serverErr int
		)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < racers; i++ {
			wg.Add(1)
			isPut := i%2 == 0
			go func(isPut bool) {
				defer wg.Done()
				<-start
				var resp *http.Response
				if isPut {
					resp = f.Do("PUT", path, strings.NewReader(""))
				} else {
					resp = f.Do("DELETE", path, nil)
				}
				if resp == nil {
					return
				}
				code := resp.StatusCode
				DrainBody(resp)
				if code >= 500 {
					mu.Lock()
					serverErr++
					mu.Unlock()
				}
			}(isPut)
		}
		close(start)
		wg.Wait()

		if serverErr > 0 {
			t.Errorf("suspended trial %d: %d racer(s) saw a 5xx on replace-null race", trial, serverErr)
		}

		// At most one null-versioned row may exist for the key (replace-null
		// keeps a single null slot — either an object or a delete marker).
		versions := keyVersions(t, f, bucketID, key)
		nullRows := 0
		for _, v := range versions {
			if v.VersionID == meta.NullVersionID {
				nullRows++
			}
		}
		if nullRows > 1 {
			t.Errorf("suspended trial %d: %d null-versioned rows for key %s (want <=1)", trial, nullRows, key)
		}
		if len(versions) > 0 {
			assertSingleLatestMonotonic(t, f, bucketID, key, fmt.Sprintf("suspended trial %d", trial))
		}

		// GET ?versionId=null must resolve to one deterministic status, stable
		// across repeated reads (no flake, no 5xx).
		first := getNullStatus(f, path)
		if first >= 500 {
			t.Errorf("suspended trial %d: GET ?versionId=null returned 5xx (%d)", trial, first)
		}
		for r := 0; r < 3; r++ {
			if again := getNullStatus(f, path); again != first {
				t.Errorf("suspended trial %d: GET ?versionId=null non-deterministic: %d then %d", trial, first, again)
				break
			}
		}
	}
}

// verifyVersioningRaceInvariants asserts the no-orphan-chunk invariant across
// the chunk-writing buckets this scenario touches (the enabled PUT bucket + the
// CAS bucket; the suspended bucket is chunk-free by construction). Every chunk
// the data backend holds must be referenced by a surviving object manifest OR
// sit in the GC queue.
func verifyVersioningRaceInvariants(t Reporter, f *Fixture) {
	t.Helper()
	if f.MemData == nil {
		return
	}
	region := f.Server.Region
	if region == "" {
		region = "default"
	}

	liveOIDs := make(map[string]struct{})
	for _, b := range []string{verEnabledBucket, verCASBucket, verSuspendedBucket} {
		bucketID := mustBucketID(t, f, b)
		for _, v := range f.AllVersions(bucketID) {
			CollectManifestOIDs(v.Manifest, liveOIDs)
		}
	}
	for oid := range gcQueueOIDs(t, f, region) {
		liveOIDs[oid] = struct{}{}
	}

	for _, oid := range f.MemData.ChunkOIDs() {
		if _, ok := liveOIDs[oid]; !ok {
			t.Errorf("orphan chunk in data backend: %s (not in any manifest or GC queue)", oid)
		}
	}
}

// assertSingleLatestMonotonic checks the per-key chain carries exactly one
// IsLatest=true row whose Mtime is no older than the chain max (1s slack for
// stamp-before-lock). Mirrors the per-key half of VerifyInvariants.
func assertSingleLatestMonotonic(t Reporter, f *Fixture, bucketID uuid.UUID, key, label string) {
	t.Helper()
	versions := keyVersions(t, f, bucketID, key)
	latestCount := 0
	var latestMtime, maxMtime time.Time
	for _, v := range versions {
		if v.IsLatest {
			latestCount++
			latestMtime = v.Mtime
		}
		if v.Mtime.After(maxMtime) {
			maxMtime = v.Mtime
		}
	}
	if latestCount != 1 {
		t.Errorf("%s: key %s expected exactly 1 IsLatest=true row, got %d (versions=%d)",
			label, key, latestCount, len(versions))
	}
	if !maxMtime.IsZero() && latestMtime.Add(time.Second).Before(maxMtime) {
		t.Errorf("%s: key %s latest row Mtime=%s older than chain max=%s",
			label, key, latestMtime, maxMtime)
	}
}

// getNullStatus issues GET ?versionId=null and returns the status code (0 on
// transport error, which is treated as a non-5xx skip by callers).
func getNullStatus(f *Fixture, path string) int {
	resp := f.Do("GET", path+"?versionId=null", nil)
	if resp == nil {
		return 0
	}
	code := resp.StatusCode
	DrainBody(resp)
	return code
}

// gcQueueOIDs returns the set of chunk OIDs currently in the region's GC queue.
func gcQueueOIDs(t Reporter, f *Fixture, region string) map[string]struct{} {
	t.Helper()
	gc, err := f.Server.Meta.ListGCEntries(context.Background(), region, time.Now().Add(time.Hour), 1<<20)
	if err != nil {
		t.Fatalf("list gc entries: %v", err)
	}
	out := make(map[string]struct{}, len(gc))
	for _, e := range gc {
		out[e.Chunk.OID] = struct{}{}
	}
	return out
}
