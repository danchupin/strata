package racetest

import (
	"context"
	"encoding/xml"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// verifier polls the gateway on a fixed cadence and asserts the three
// invariants from the PRD US-004:
//
//  1. Read-after-write: GET on a tracked key must return an ETag the
//     workload PUT (and a Content-Length matching the recorded size).
//  2. Versioning monotonicity: every tracked version_id must appear
//     in ListObjectVersions for that key.
//  3. Delete-grace: keys deleted more than tracker.grace ago, with no
//     subsequent PUT, must not surface in ListObjectsV2.
type verifier struct {
	ctx      context.Context
	client   *http.Client
	sgn      *signer
	endpoint string
	buckets  []string
	tracker  *Tracker
	every    time.Duration
	rng      *rand.Rand
}

// startVerifier constructs the verifier; caller runs v.run().
func startVerifier(ctx context.Context, w *workerCtx, every time.Duration) *verifier {
	return &verifier{
		ctx:      ctx,
		client:   w.client,
		sgn:      w.sgn,
		endpoint: w.endpoint,
		buckets:  append([]string(nil), w.buckets...),
		tracker:  w.tracker,
		every:    every,
		rng:      rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// run blocks until ctx is cancelled, then performs one final pass so
// trailing-state inconsistencies still surface in the report.
func (v *verifier) run() {
	if v.tracker == nil || v.every <= 0 {
		return
	}
	ticker := time.NewTicker(v.every)
	defer ticker.Stop()
	for {
		select {
		case <-v.ctx.Done():
			// Final pass with a fresh background context bounded by the
			// per-op http client timeouts; the run-context is already done.
			v.runPass(context.Background())
			return
		case <-ticker.C:
			v.runPass(v.ctx)
		}
	}
}

func (v *verifier) runPass(ctx context.Context) {
	for _, b := range v.buckets {
		v.checkReadAfterWrite(ctx, b)
		v.checkVersioningPresence(ctx, b)
		v.checkDeleteGrace(ctx, b)
	}
}

// rawETag strips the surrounding quotes the S3 spec mandates on
// response Etag headers (and the multipart-complete XML body).
func rawETag(s string) string { return strings.Trim(s, `"`) }

// checkReadAfterWrite samples up to 8 tracked keys per bucket and
// validates GET returns an ETag the workload recorded. Mismatches go
// through a backoff-retry loop to absorb the per-PUT race window
// between a peer worker's PUT response landing on the wire and that
// worker calling Tracker.RecordPut. Only persistent mismatches across
// the full retry window flag a real inconsistency.
func (v *verifier) checkReadAfterWrite(ctx context.Context, bucket string) {
	keys := v.tracker.trackedKeys(bucket)
	if len(keys) == 0 {
		return
	}
	if len(keys) > 8 {
		v.rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
		keys = keys[:8]
	}
	for _, k := range keys {
		v.verifyKeyETag(ctx, bucket, k)
	}
}

// recheckMaxAttempts and recheckDelay tune the race-window absorption
// for the read-after-write oracle. 5 × 100ms = 500ms total — long
// enough that a peer worker that received a PUT response will have
// completed its RecordPut call (microseconds in-process; ms on a
// real gateway), short enough not to swallow an actual corruption.
const (
	recheckMaxAttempts = 5
	recheckDelay       = 100 * time.Millisecond
)

func (v *verifier) verifyKeyETag(ctx context.Context, bucket, key string) {
	path := "/" + bucket + "/" + url.PathEscape(key)
	etag, size, requestID, ok := v.fetchObject(ctx, path)
	if !ok {
		return
	}
	matched, expected := v.tracker.validateETag(bucket, key, etag, size)
	if matched {
		return
	}
	// Persistent mismatch loop: re-validate, then re-fetch, up to
	// recheckMaxAttempts times. If at any iteration the etag becomes
	// known to the tracker (or a fresh GET returns a known etag), the
	// race window is absorbed and we stop.
	for range recheckMaxAttempts {
		select {
		case <-ctx.Done():
			return
		case <-time.After(recheckDelay):
		}
		if matched, expected = v.tracker.validateETag(bucket, key, etag, size); matched {
			return
		}
		etag2, size2, rid2, ok := v.fetchObject(ctx, path)
		if !ok {
			return
		}
		etag, size, requestID = etag2, size2, rid2
		if matched, expected = v.tracker.validateETag(bucket, key, etag, size); matched {
			return
		}
	}
	v.tracker.Flag(Inconsistency{
		Kind:      "read_after_write",
		Bucket:    bucket,
		Key:       key,
		Detail:    "GET returned an etag/size pair the workload never PUT",
		Expected:  expected,
		Observed:  fmt.Sprintf("etag=%q size=%d", etag, size),
		RequestID: requestID,
	})
}

// fetchObject issues a single GET and returns (etag, size, requestID,
// ok). ok=false indicates the response was a transport error, a 404, a
// non-200 status, or had an empty Etag — in all cases the verifier
// cannot validate this iteration so the caller bails out cleanly.
func (v *verifier) fetchObject(ctx context.Context, path string) (string, int64, string, bool) {
	resp, status, err := doSignedReadResp(ctx, v.client, v.sgn, v.endpoint, "GET", path, nil)
	if err != nil || resp == nil {
		return "", 0, "", false
	}
	if status != http.StatusOK {
		DrainBody(resp)
		return "", 0, "", false
	}
	etag := rawETag(resp.Header.Get("Etag"))
	size, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	requestID := resp.Header.Get("X-Request-Id")
	DrainBody(resp)
	if etag == "" {
		return "", 0, "", false
	}
	return etag, size, requestID, true
}

// checkVersioningPresence picks one tracked key per bucket and
// asserts the listing contains every version_id the workload has
// observed in PUT responses.
func (v *verifier) checkVersioningPresence(ctx context.Context, bucket string) {
	if !v.tracker.versioningEnabled(bucket) {
		return
	}
	keys := v.tracker.trackedKeys(bucket)
	if len(keys) == 0 {
		return
	}
	k := keys[v.rng.Intn(len(keys))]
	expected := v.tracker.expectedVersionIDs(bucket, k)
	if len(expected) == 0 {
		return
	}
	listPath := fmt.Sprintf("/%s?versions&prefix=%s", bucket, url.QueryEscape(k))
	body, status, err := doSignedRead(ctx, v.client, v.sgn, v.endpoint, "GET", listPath, nil)
	if err != nil || status != http.StatusOK {
		return
	}
	var listed struct {
		XMLName xml.Name `xml:"ListVersionsResult"`
		Version []struct {
			Key       string `xml:"Key"`
			VersionId string `xml:"VersionId"`
		} `xml:"Version"`
	}
	if err := xml.Unmarshal(body, &listed); err != nil {
		return
	}
	seen := make(map[string]struct{}, len(listed.Version))
	for _, vv := range listed.Version {
		if vv.Key == k {
			seen[vv.VersionId] = struct{}{}
		}
	}
	for _, want := range expected {
		if want == "" {
			continue
		}
		if _, ok := seen[want]; ok {
			continue
		}
		v.tracker.Flag(Inconsistency{
			Kind:     "versioning_missing",
			Bucket:   bucket,
			Key:      k,
			Detail:   "tracked version_id not present in ListObjectVersions",
			Expected: fmt.Sprintf("version_id=%s present", want),
			Observed: fmt.Sprintf("listed=%d versions", len(seen)),
		})
		return // one flag per pass per key
	}
}

// checkDeleteGrace lists each pending-expired delete via ListObjectsV2
// (prefix=key, max-keys=1) and flags any key still surfacing. A HEAD
// confirms the key is actually absent on the gateway before flagging:
// the tracker's deletedAt is cleared in worker-call order, not gateway-
// commit order, so a peer worker that PUT after a DeleteObjects can
// leave deletedAt set in the tracker while the gateway treats the key
// as live. HEAD = 200 absorbs that race; HEAD = 404 / 405 (delete
// marker) means the gateway agrees the key is gone yet ListObjectsV2
// surfaced it — that is the divergence we want to catch.
func (v *verifier) checkDeleteGrace(ctx context.Context, bucket string) {
	pending := v.tracker.pendingExpiredDeletes(bucket, time.Now().UTC())
	if len(pending) == 0 {
		return
	}
	if len(pending) > 8 {
		v.rng.Shuffle(len(pending), func(i, j int) { pending[i], pending[j] = pending[j], pending[i] })
		pending = pending[:8]
	}
	for _, k := range pending {
		listPath := fmt.Sprintf("/%s?list-type=2&prefix=%s&max-keys=1", bucket, url.QueryEscape(k))
		body, status, err := doSignedRead(ctx, v.client, v.sgn, v.endpoint, "GET", listPath, nil)
		if err != nil || status != http.StatusOK {
			continue
		}
		var listed struct {
			XMLName  xml.Name `xml:"ListBucketResult"`
			Contents []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
		}
		if err := xml.Unmarshal(body, &listed); err != nil {
			continue
		}
		present := false
		for _, c := range listed.Contents {
			if c.Key == k {
				present = true
				break
			}
		}
		if !present {
			continue
		}
		// Confirm via HEAD: 200 means the gateway treats the key as
		// live (a post-delete PUT raced through), so the tracker is
		// stale — not an inconsistency. Anything else (404, 405 for
		// delete-marker latest, transport error) leaves the key
		// genuinely absent from the gateway's GET path while
		// ListObjectsV2 still returns it: the divergence we flag.
		headPath := "/" + bucket + "/" + url.PathEscape(k)
		hStatus, hErr := doSigned(ctx, v.client, v.sgn, v.endpoint, "HEAD", headPath, nil)
		if hErr == nil && hStatus == http.StatusOK {
			continue
		}
		v.tracker.Flag(Inconsistency{
			Kind:     "delete_grace",
			Bucket:   bucket,
			Key:      k,
			Detail:   fmt.Sprintf("key still listed after delete grace (%s); HEAD status=%d", v.tracker.Grace(), hStatus),
			Expected: "absent from ListObjectsV2",
			Observed: "present",
		})
	}
}
