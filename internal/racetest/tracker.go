package racetest

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// trackerHistoryCap caps the per-key ring of recent (etag, size) pairs.
// 32 is enough to cover the in-flight write window even at peak concurrency
// without unbounded memory growth on a 1h soak run.
const trackerHistoryCap = 32

// trackerEntry is a single recorded successful PUT — the (etag, size,
// versionID, observedAt) tuple captured by the workload from the PUT
// response.
type trackerEntry struct {
	etag      string
	size      int64
	versionID string
	at        time.Time
}

// trackerKeyState is the per-(bucket, key) record.
//
//   - history is a recent ring (capped) used for diagnostic strings —
//     the validator pastes recent etags into Inconsistency.Expected so
//     triage sees a tractable window.
//   - allEtags is the unbounded set of (etag → size) ever recorded on
//     this key. It backs the read-after-write check so a slow write
//     that's been bumped out of the recency ring still validates as a
//     legitimate response.
//   - deletedAt is non-zero only when the most recent op on the key
//     was a delete (any subsequent PUT clears it).
type trackerKeyState struct {
	history    []trackerEntry
	allEtags   map[string]int64
	deletedAt  time.Time
	versionIDs []string
}

// Tracker is the source-of-truth state the verifier oracle reads against
// when it GETs / lists the gateway. Workers feed it via RecordPut /
// RecordDelete / RecordBatchDelete after a successful op; the verifier
// reads expected ETags and pending-delete state to flag inconsistencies.
//
// All methods are safe for concurrent use.
type Tracker struct {
	mu           sync.Mutex
	keys         map[string]map[string]*trackerKeyState // bucket -> key -> state
	versioningOn map[string]bool                        // bucket -> versioning enabled

	sink  EventSink
	grace time.Duration

	incMu           sync.Mutex
	inconsistencies []Inconsistency
}

// NewTracker constructs a Tracker. grace is the per-key delete grace
// window — keys that stay visible in ListObjects after grace has elapsed
// without an intervening PUT trigger a delete-grace inconsistency.
func NewTracker(sink EventSink, grace time.Duration) *Tracker {
	if sink == nil {
		sink = nopSink{}
	}
	if grace <= 0 {
		grace = DefaultDeleteGrace
	}
	return &Tracker{
		keys:         make(map[string]map[string]*trackerKeyState),
		versioningOn: make(map[string]bool),
		sink:         sink,
		grace:        grace,
	}
}

// Grace exposes the configured delete grace window — read-only, for
// the verifier to format diagnostic strings without reaching into
// internal state.
func (t *Tracker) Grace() time.Duration { return t.grace }

func (t *Tracker) ensureKey(bucket, key string) *trackerKeyState {
	bk := t.keys[bucket]
	if bk == nil {
		bk = make(map[string]*trackerKeyState)
		t.keys[bucket] = bk
	}
	s := bk[key]
	if s == nil {
		s = &trackerKeyState{allEtags: make(map[string]int64)}
		bk[key] = s
	}
	return s
}

// RecordIntent pre-registers an etag the workload is about to attempt.
// Computed from the request body BEFORE the PUT goes on the wire so
// the read-after-write verifier doesn't false-flag in the race window
// between a worker's PUT response landing and that worker calling
// RecordPut. Intents that never commit (transport error, 412 from
// conditional-PUT, etc.) are harmless — the per-key allEtags set only
// loosens the validator's "etag the workload never PUT" predicate.
func (t *Tracker) RecordIntent(bucket, key, etag string, size int64) {
	if etag == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.ensureKey(bucket, key)
	s.allEtags[etag] = size
}

// RecordPut tracks a successful PUT. etag is the response Etag header
// (unquoted); versionID is x-amz-version-id (empty when versioning is
// suspended). size is the body byte count.
//
// versionID == "null" is the AWS-spec sentinel for the single
// no-version-id slot a versioning-suspended bucket exposes; multiple
// PUTs while suspended overwrite that one slot. Tracking each as a
// distinct version_id would produce false-positive
// versioning_missing flags from ListObjectVersions, which only ever
// returns one "null" entry per key. So skip it.
func (t *Tracker) RecordPut(bucket, key, etag, versionID string, size int64, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.ensureKey(bucket, key)
	s.history = append(s.history, trackerEntry{etag: etag, size: size, versionID: versionID, at: at})
	if len(s.history) > trackerHistoryCap {
		s.history = s.history[len(s.history)-trackerHistoryCap:]
	}
	if etag != "" {
		s.allEtags[etag] = size
	}
	if versionID != "" && versionID != "null" {
		s.versionIDs = append(s.versionIDs, versionID)
		if len(s.versionIDs) > trackerHistoryCap {
			s.versionIDs = s.versionIDs[len(s.versionIDs)-trackerHistoryCap:]
		}
	}
	// Any PUT clears the pending-delete marker — the key is live again.
	s.deletedAt = time.Time{}
}

// RecordDelete marks a single-key delete. Subsequent ListObjects calls
// after the grace window must not surface the key.
func (t *Tracker) RecordDelete(bucket, key string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.ensureKey(bucket, key)
	s.deletedAt = at
}

// RecordBatchDelete marks a DeleteObjects batch. keys is the list parsed
// from the response's <Deleted> entries.
func (t *Tracker) RecordBatchDelete(bucket string, keys []string, at time.Time) {
	if len(keys) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, k := range keys {
		s := t.ensureKey(bucket, k)
		s.deletedAt = at
	}
}

// EnableVersioning records that the workload has set this bucket's
// versioning to Enabled at start. Used by the verifier to skip the
// ListObjectVersions check on buckets where versioning was never on.
func (t *Tracker) EnableVersioning(bucket string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.versioningOn[bucket] = true
}

// Snapshot returns a copy of the inconsistencies recorded so far. Safe
// to call concurrently with the verifier.
func (t *Tracker) Snapshot() []Inconsistency {
	t.incMu.Lock()
	defer t.incMu.Unlock()
	out := make([]Inconsistency, len(t.inconsistencies))
	copy(out, t.inconsistencies)
	return out
}

// Flag records a verifier-detected inconsistency with full diagnostic
// payload and emits the matching JSON-lines event.
func (t *Tracker) Flag(inc Inconsistency) {
	if inc.Timestamp.IsZero() {
		inc.Timestamp = time.Now().UTC()
	}
	t.incMu.Lock()
	t.inconsistencies = append(t.inconsistencies, inc)
	t.incMu.Unlock()
	cp := inc
	t.sink.Emit(Event{
		Event:     "inconsistency",
		Timestamp: inc.Timestamp,
		Bucket:    inc.Bucket,
		Key:       inc.Key,
		Inconsist: &cp,
	})
}

// validateETag returns (matched, expectedDescription). If matched is
// false, expectedDescription holds a human-readable summary of the
// recent etag history the caller can paste into Inconsistency.Expected.
//
// Lookup uses the unbounded allEtags set so a slow earlier write
// rotated out of the diagnostic ring still counts as a legitimate
// response — only an etag the workload never PUT triggers a flag.
func (t *Tracker) validateETag(bucket, key, etag string, size int64) (bool, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	bk := t.keys[bucket]
	if bk == nil {
		return true, ""
	}
	s := bk[key]
	if s == nil || len(s.allEtags) == 0 {
		return true, ""
	}
	if want, ok := s.allEtags[etag]; ok {
		if size < 0 || want == size {
			return true, ""
		}
		return false, fmt.Sprintf("etag matched but tracked size=%d (observed %d)", want, size)
	}
	etags := make([]string, 0, len(s.history))
	for _, e := range s.history {
		etags = append(etags, e.etag)
	}
	return false, fmt.Sprintf("recent etags=%v (total tracked=%d)", etags, len(s.allEtags))
}

// expectedVersionIDs returns a copy of the per-key version_id list for
// versioning-presence checks.
func (t *Tracker) expectedVersionIDs(bucket, key string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	bk := t.keys[bucket]
	if bk == nil {
		return nil
	}
	s := bk[key]
	if s == nil {
		return nil
	}
	out := make([]string, len(s.versionIDs))
	copy(out, s.versionIDs)
	return out
}

// pendingExpiredDeletes returns keys whose deletedAt + grace ≤ now and
// where no PUT has happened since the delete (deletedAt is reset by
// RecordPut). The returned slice is sorted for stable iteration.
func (t *Tracker) pendingExpiredDeletes(bucket string, now time.Time) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	bk := t.keys[bucket]
	if bk == nil {
		return nil
	}
	var out []string
	for k, s := range bk {
		if s.deletedAt.IsZero() {
			continue
		}
		if now.Sub(s.deletedAt) < t.grace {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// trackedKeys returns the keys recorded for a bucket (any state).
func (t *Tracker) trackedKeys(bucket string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	bk := t.keys[bucket]
	out := make([]string, 0, len(bk))
	for k := range bk {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// versioningEnabled reports whether the workload pre-enabled versioning
// on this bucket. Used by the verifier's ListObjectVersions check.
func (t *Tracker) versioningEnabled(bucket string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.versioningOn[bucket]
}

// DefaultDeleteGrace is the per-key delete-grace window applied by
// NewTracker when no override is supplied. PRD US-004 default: 5s.
const DefaultDeleteGrace = 5 * time.Second
