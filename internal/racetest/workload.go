package racetest

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"time"
)

// opPicker is a weighted random picker over op classes. Pre-builds a
// sorted (cumulative-weight, class) slice so each per-iteration pick is
// O(log N) rather than re-sorting the map every time.
type opPicker struct {
	classes []string
	cums    []float64
	total   float64
}

func newOpPicker(mix map[string]float64) (*opPicker, error) {
	src := mix
	if len(src) == 0 {
		src = DefaultMix
	}
	keys := make([]string, 0, len(src))
	for k := range src {
		// Drop unknown classes — keeps a typo from skewing the picker.
		if !isKnownOpClass(k) {
			continue
		}
		if src[k] < 0 {
			return nil, fmt.Errorf("racetest: Mix weight for %q is negative", k)
		}
		if src[k] > 0 {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("racetest: Mix has no positive weights")
	}
	sort.Strings(keys)
	p := &opPicker{classes: keys, cums: make([]float64, len(keys))}
	for i, k := range keys {
		p.total += src[k]
		p.cums[i] = p.total
	}
	return p, nil
}

func (p *opPicker) pick(rng *rand.Rand) string {
	r := rng.Float64() * p.total
	for i, c := range p.cums {
		if r < c {
			return p.classes[i]
		}
	}
	return p.classes[len(p.classes)-1]
}

func isKnownOpClass(c string) bool {
	for _, k := range allOpClasses {
		if k == c {
			return true
		}
	}
	return false
}

// workerCtx bundles the per-Run knobs each worker goroutine reuses.
// Pulled out of the worker closure so runOnce stays a method and the
// per-op functions don't need 8-arg signatures.
type workerCtx struct {
	ctx            context.Context
	client         *http.Client
	sgn            *signer
	endpoint       string
	sink           EventSink
	buckets        []string
	objectKeys     int
	streamingRatio float64
	bump           func(string)
}

// runOnce dispatches a single op of the picked class. Counter bump is
// per-class even on transport / 4xx — what we count is "the op was
// attempted", since failures are also evidence the workload exercised
// that class. Multipart bumps only on a fully-completed cycle to keep
// parity with the pre-US-003 counter.
func (w *workerCtx) runOnce(workerID, iter int, rng *rand.Rand, class string) {
	bucket := w.buckets[rng.Intn(len(w.buckets))]
	key := fmt.Sprintf("k-%d", rng.Intn(w.objectKeys))
	path := "/" + bucket + "/" + key
	switch class {
	case OpPut:
		body := []byte(fmt.Sprintf("w%d-i%d", workerID, iter))
		streaming := w.shouldStream(rng)
		w.runOp(workerID, OpPut, bucket, key, func() (int, error) {
			return w.doPUTBody(path, body, streaming, nil)
		})
		w.bump(OpPut)
	case OpGet:
		w.runOp(workerID, OpGet, bucket, key, func() (int, error) {
			return doSigned(w.ctx, w.client, w.sgn, w.endpoint, "GET", path, nil)
		})
		w.bump(OpGet)
	case OpDelete:
		w.runOp(workerID, OpDelete, bucket, key, func() (int, error) {
			return doSigned(w.ctx, w.client, w.sgn, w.endpoint, "DELETE", path, nil)
		})
		w.bump(OpDelete)
	case OpList:
		// ListObjectsV2 with a small max-keys to keep the response
		// small; the workload churns enough write/delete ops that any
		// listing-vs-truth divergence will surface even on tiny pages.
		listPath := "/" + bucket + "?list-type=2&max-keys=20"
		w.runOp(workerID, OpList, bucket, "", func() (int, error) {
			return doSigned(w.ctx, w.client, w.sgn, w.endpoint, "GET", listPath, nil)
		})
		w.bump(OpList)
	case OpMultipart:
		if doMultipartCycle(w.ctx, w.sink, w.client, w.sgn, w.endpoint, bucket, key, workerID, iter) {
			w.bump(OpMultipart)
		}
	case OpVersioningFlip:
		// Flip Status between Enabled and Suspended. Both transitions
		// hit the LWT hot path on Cassandra; we bias 50/50 from the
		// rng so neither direction starves.
		status := "Enabled"
		if rng.Intn(2) == 0 {
			status = "Suspended"
		}
		body := []byte(fmt.Sprintf("<VersioningConfiguration><Status>%s</Status></VersioningConfiguration>", status))
		flipPath := "/" + bucket + "?versioning"
		w.runOp(workerID, OpVersioningFlip, bucket, "", func() (int, error) {
			return doSigned(w.ctx, w.client, w.sgn, w.endpoint, "PUT", flipPath, body)
		})
		w.bump(OpVersioningFlip)
	case OpConditionalPut:
		body := []byte(fmt.Sprintf("cw%d-i%d", workerID, iter))
		streaming := w.shouldStream(rng)
		headers := http.Header{"If-None-Match": []string{"*"}}
		w.runOp(workerID, OpConditionalPut, bucket, key, func() (int, error) {
			return w.doPUTBody(path, body, streaming, headers)
		})
		w.bump(OpConditionalPut)
	case OpDeleteObjects:
		// Pick up to 4 keys from the per-bucket pool. Quiet=false so the
		// gateway returns the standard <Deleted> entries, which is the
		// shape consumers expect under normal nightly conditions. Clamp
		// to objectKeys so the dedup loop never spins indefinitely on
		// a small-key fixture.
		n := 2 + rng.Intn(3)
		if n > w.objectKeys {
			n = w.objectKeys
		}
		if n < 1 {
			n = 1
		}
		seen := make(map[int]struct{}, n)
		var sb strings.Builder
		sb.WriteString("<Delete>")
		for len(seen) < n {
			idx := rng.Intn(w.objectKeys)
			if _, ok := seen[idx]; ok {
				continue
			}
			seen[idx] = struct{}{}
			fmt.Fprintf(&sb, "<Object><Key>k-%d</Key></Object>", idx)
		}
		sb.WriteString("</Delete>")
		body := []byte(sb.String())
		delPath := "/" + bucket + "?delete"
		w.runOp(workerID, OpDeleteObjects, bucket, "", func() (int, error) {
			return doSigned(w.ctx, w.client, w.sgn, w.endpoint, "POST", delPath, body)
		})
		w.bump(OpDeleteObjects)
	}
}

// shouldStream returns true when this body-carrying op should take the
// streaming-SigV4 path. Anonymous mode (no signer) always returns false
// so the flow degrades gracefully against in-process tests.
func (w *workerCtx) shouldStream(rng *rand.Rand) bool {
	if w.sgn == nil {
		return false
	}
	return rng.Float64() < w.streamingRatio
}

// doPUTBody dispatches a PUT with body via either the streaming-SigV4
// path or the pre-computed-SHA fixed-payload path, selected by
// `streaming`. Extra headers (e.g. If-None-Match) are merged before
// signing so they are covered by the signature.
func (w *workerCtx) doPUTBody(path string, body []byte, streaming bool, extra http.Header) (int, error) {
	req, err := http.NewRequestWithContext(w.ctx, "PUT", w.endpoint+path, nil)
	if err != nil {
		return 0, err
	}
	for k, vs := range extra {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if w.sgn != nil {
		if streaming {
			if err := w.sgn.signStreaming(w.ctx, req, body); err != nil {
				return 0, err
			}
		} else {
			if err := w.sgn.sign(w.ctx, req, body); err != nil {
				return 0, err
			}
		}
	} else if body != nil {
		req.Body = io.NopCloser(strings.NewReader(string(body)))
		req.ContentLength = int64(len(body))
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, err
	}
	status := resp.StatusCode
	DrainBody(resp)
	return status, nil
}

// runOp wraps a single HTTP op with op_started + op_done events. The
// closure returns (status, err). A non-nil err is recorded on the
// op_done event; status==0 indicates a transport-level failure where
// the body was never read.
func (w *workerCtx) runOp(workerID int, class, bucket, key string, do func() (int, error)) {
	if w.ctx.Err() != nil {
		return
	}
	start := time.Now().UTC()
	w.sink.Emit(Event{Event: "op_started", Timestamp: start, WorkerID: workerID, Class: class, Bucket: bucket, Key: key})
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
	w.sink.Emit(ev)
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

// doMultipartCycle runs Create+UploadPart+Complete (or aborts on
// failure). Returns true on a fully-completed cycle so the caller bumps
// the multipart op counter only on the success path.
func doMultipartCycle(ctx context.Context, sink EventSink, client *http.Client, sgn *signer, endpoint, bucket, key string, workerID, iter int) bool {
	path := "/" + bucket + "/" + key
	cycleStart := time.Now().UTC()
	sink.Emit(Event{Event: "op_started", Timestamp: cycleStart, WorkerID: workerID, Class: OpMultipart, Bucket: bucket, Key: key})

	finishDone := func(status int, err error) {
		end := time.Now().UTC()
		ev := Event{
			Event:      "op_done",
			Timestamp:  end,
			WorkerID:   workerID,
			Class:      OpMultipart,
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
