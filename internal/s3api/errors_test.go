package s3api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danchupin/strata/internal/auth"
)

// TestWriteAuthDenied_TrailerFormat translates auth.ErrTrailerFormatUnsupported
// into 501 NotImplemented with the trailer-specific XML body (US-003).
// This is the s3api half of the trailer-format rejection wiring; the
// auth-side detection is covered in internal/auth/middleware_test.go.
func TestWriteAuthDenied_TrailerFormat(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/bucket/key", nil)
	WriteAuthDenied(rec, req, auth.ErrTrailerFormatUnsupported)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<Code>NotImplemented</Code>") {
		t.Errorf("body missing NotImplemented code: %s", body)
	}
	if !strings.Contains(body, "aws-chunked-trailer format is not yet supported") {
		t.Errorf("body missing trailer-format message: %s", body)
	}
}
