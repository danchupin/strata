package s3api_test

import (
	"net/http"
	"testing"
)

// TestRangeZeroLength locks AWS S3 parity for Range requests against a
// zero-length object (US-008). The existing nonzero-size happy paths are
// covered by TestObjectRangeGet in object_test.go; this file consolidates
// the zero-length edge cases that previously lived nowhere.
func TestRangeZeroLength(t *testing.T) {
	h := newHarness(t)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt/empty", ""), 200)

	cases := []struct {
		name       string
		hdr        string
		wantStatus int
		wantCR     string
		wantCL     string
	}{
		{"no-range", "", http.StatusOK, "", "0"},
		{"open-ended", "bytes=0-", http.StatusRequestedRangeNotSatisfiable, "bytes */0", ""},
		{"suffix", "bytes=-10", http.StatusOK, "", "0"},
		{"explicit", "bytes=0-9", http.StatusRequestedRangeNotSatisfiable, "bytes */0", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := []string{}
			if tc.hdr != "" {
				headers = append(headers, "Range", tc.hdr)
			}
			resp := h.doString("GET", "/bkt/empty", "", headers...)
			h.mustStatus(resp, tc.wantStatus)
			if cr := resp.Header.Get("Content-Range"); cr != tc.wantCR {
				t.Errorf("Content-Range: got %q want %q", cr, tc.wantCR)
			}
			if tc.wantCL != "" {
				if cl := resp.Header.Get("Content-Length"); cl != tc.wantCL {
					t.Errorf("Content-Length: got %q want %q", cl, tc.wantCL)
				}
			}
			if body := h.readBody(resp); body != "" {
				t.Errorf("body: got %q want empty", body)
			}
		})
	}
}
