package s3api_test

import (
	"os"
	"testing"

	"github.com/danchupin/strata/internal/s3api"
)

// TestMain disables the 5 MiB minimum-part-size enforcement by default so
// the bulk of multipart tests (checksum, partNumber, preconditions, etc.)
// can use small bodies and stay fast. Tests that specifically exercise the
// size validation re-arm it via s3api.SetMultipartMinPartSizeForTest.
func TestMain(m *testing.M) {
	restore := s3api.SetMultipartMinPartSizeForTest(0)
	defer restore()
	os.Exit(m.Run())
}
