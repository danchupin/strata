package s3api_test

import (
	"net/http"
	"testing"
)

// US-002: adversarial RFC 7232 conditional-request matrix for the GET/HEAD
// read path (`checkConditional` in conditional.go) plus the PUT-side
// inline `If-Match`/`If-None-Match` guards (`putObject` in server.go).
//
// Scope split vs existing coverage:
//   - PUT-side multipart-Complete preconditions: multipart_preconditions_test.go
//   - copy-side preconditions: copy_object_test.go (TestCopyObjectIfMatch*)
//   - Range mechanics: range_test.go / object_test.go
// This file owns the GET/HEAD read-path matrix, which had no direct test, and
// the single-PUT object-create/lost-update precondition pairs.

const (
	condPast   = "Mon, 02 Jan 2006 15:04:05 GMT" // older than any put-time mtime
	condFuture = "Fri, 01 Jan 2100 00:00:00 GMT" // newer than any put-time mtime
)

// putCondObject creates /bkt/key with the given body and returns the quoted
// strong ETag the gateway assigned (the form clients send back in If-Match).
func putCondObject(t *testing.T, h *testHarness, key, body string) string {
	t.Helper()
	resp := h.doString("PUT", "/bkt/"+key, body)
	h.mustStatus(resp, http.StatusOK)
	etag := resp.Header.Get("ETag")
	_ = h.readBody(resp)
	if etag == "" {
		t.Fatalf("PUT %s returned empty ETag", key)
	}
	return etag
}

// TestConditionalGetMatrix drives GET + HEAD through every If-* branch and
// asserts the exact status code (200 / 206 / 304 / 412) per RFC 7232.
func TestConditionalGetMatrix(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), http.StatusOK)
	etag := putCondObject(t, h, "obj", "hello-world-conditional")
	wrong := `"00000000000000000000000000000000"`

	cases := []struct {
		name    string
		headers []string
		want    int
	}{
		// --- If-Match ---
		{"if-match strong-hit", []string{"If-Match", etag}, http.StatusOK},
		{"if-match mismatch", []string{"If-Match", wrong}, http.StatusPreconditionFailed},
		{"if-match wildcard", []string{"If-Match", "*"}, http.StatusOK},
		{"if-match list-hit", []string{"If-Match", wrong + ", " + etag}, http.StatusOK},
		{"if-match list-miss", []string{"If-Match", wrong + `, "deadbeef"`}, http.StatusPreconditionFailed},

		// --- If-None-Match ---
		{"if-none-match hit-304", []string{"If-None-Match", etag}, http.StatusNotModified},
		{"if-none-match wildcard-304", []string{"If-None-Match", "*"}, http.StatusNotModified},
		{"if-none-match miss-200", []string{"If-None-Match", wrong}, http.StatusOK},
		{"if-none-match list-hit-304", []string{"If-None-Match", wrong + ", " + etag}, http.StatusNotModified},

		// --- If-Modified-Since ---
		{"if-modified-since future-304", []string{"If-Modified-Since", condFuture}, http.StatusNotModified},
		{"if-modified-since past-200", []string{"If-Modified-Since", condPast}, http.StatusOK},

		// --- If-Unmodified-Since ---
		{"if-unmodified-since past-412", []string{"If-Unmodified-Since", condPast}, http.StatusPreconditionFailed},
		{"if-unmodified-since future-200", []string{"If-Unmodified-Since", condFuture}, http.StatusOK},

		// --- Precedence (RFC 7232 §6 + AWS docs) ---
		// If-Match matches AND If-Unmodified-Since would fail → 200 (If-Match wins).
		{"prec if-match-over-unmod", []string{"If-Match", etag, "If-Unmodified-Since", condPast}, http.StatusOK},
		// If-None-Match matches AND If-Modified-Since would 200 → 304 (If-None-Match wins).
		{"prec if-none-match-over-mod", []string{"If-None-Match", etag, "If-Modified-Since", condPast}, http.StatusNotModified},
		// If-Match mismatch still 412 even though If-Unmodified-Since would pass.
		{"prec if-match-miss-over-unmod", []string{"If-Match", wrong, "If-Unmodified-Since", condFuture}, http.StatusPreconditionFailed},
	}

	for _, method := range []string{"GET", "HEAD"} {
		for _, tc := range cases {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				resp := h.doString(method, "/bkt/obj", "", tc.headers...)
				h.mustStatus(resp, tc.want)
				_ = h.readBody(resp)
			})
		}
	}
}

// TestConditionalGetRange206 proves a Range GET that passes its precondition
// returns 206 (the conditional gate runs before range shaping), and a Range
// GET whose precondition fails short-circuits to 412 before any 206.
func TestConditionalGetRange206(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), http.StatusOK)
	etag := putCondObject(t, h, "obj", "0123456789abcdef")
	wrong := `"00000000000000000000000000000000"`

	// Precondition passes → range honoured → 206.
	resp := h.doString("GET", "/bkt/obj", "", "If-Match", etag, "Range", "bytes=0-3")
	h.mustStatus(resp, http.StatusPartialContent)
	if body := h.readBody(resp); body != "0123" {
		t.Errorf("range body: got %q want %q", body, "0123")
	}

	// Precondition fails → 412 short-circuit, range never applied.
	resp = h.doString("GET", "/bkt/obj", "", "If-Match", wrong, "Range", "bytes=0-3")
	h.mustStatus(resp, http.StatusPreconditionFailed)
	_ = h.readBody(resp)
}

// TestConditionalPutPreconditions covers the single-PUT lost-update and
// create-if-absent precondition guards inline in putObject.
func TestConditionalPutPreconditions(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), http.StatusOK)
	etag := putCondObject(t, h, "obj", "v1-body")
	wrong := `"00000000000000000000000000000000"`

	t.Run("if-none-match-star rejects overwrite", func(t *testing.T) {
		// Object exists → If-None-Match: * must 412 (no clobber).
		resp := h.doString("PUT", "/bkt/obj", "v2", "If-None-Match", "*")
		h.mustStatus(resp, http.StatusPreconditionFailed)
		_ = h.readBody(resp)
	})

	t.Run("if-none-match-star creates absent", func(t *testing.T) {
		// Fresh key → If-None-Match: * succeeds (create-if-absent).
		resp := h.doString("PUT", "/bkt/fresh", "new", "If-None-Match", "*")
		h.mustStatus(resp, http.StatusOK)
		_ = h.readBody(resp)
	})

	t.Run("if-match hit allows update", func(t *testing.T) {
		// Current ETag → lost-update guard passes.
		resp := h.doString("PUT", "/bkt/obj", "v2", "If-Match", etag)
		h.mustStatus(resp, http.StatusOK)
		_ = h.readBody(resp)
	})

	t.Run("if-match mismatch blocks update", func(t *testing.T) {
		resp := h.doString("PUT", "/bkt/obj", "v3", "If-Match", wrong)
		h.mustStatus(resp, http.StatusPreconditionFailed)
		_ = h.readBody(resp)
	})

	t.Run("if-match on missing object 412", func(t *testing.T) {
		// RFC 7232 §3.1: single-PUT If-Match on an absent object → 412
		// (multipart-Complete diverges to 404 NoSuchKey; single-PUT keeps 412).
		resp := h.doString("PUT", "/bkt/ghost", "x", "If-Match", etag)
		h.mustStatus(resp, http.StatusPreconditionFailed)
		_ = h.readBody(resp)
	})
}
