package s3api_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/danchupin/strata/internal/data"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/s3api"
	"github.com/stretchr/testify/require"
)

// stampSpyBackend wraps a data.Backend and records every StampBackref call so a
// test can assert the gateway re-stamps a completed multipart object's chunks
// with the final object identity (US-001b). The memory backend itself does NOT
// implement data.BackrefStamper, so this spy is how the gateway wiring is
// exercised CI-green without RADOS.
type stampSpyBackend struct {
	data.Backend
	mu    sync.Mutex
	calls []stampSpyCall
}

type stampSpyCall struct {
	attrs    data.BackrefAttrs
	chunkIDs []string
}

func (s *stampSpyBackend) StampBackref(_ context.Context, m *data.Manifest, attrs data.BackrefAttrs) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(m.Chunks))
	for _, c := range m.Chunks {
		ids = append(ids, c.OID)
	}
	s.calls = append(s.calls, stampSpyCall{attrs: attrs, chunkIDs: ids})
	return nil
}

func (s *stampSpyBackend) lastCall(t *testing.T) stampSpyCall {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.Len(t, s.calls, 1, "expected exactly one StampBackref call")
	return s.calls[0]
}

// TestCompleteMultipartStampsBackref proves the gateway re-stamps the
// back-reference on every chunk of a completed multipart object, with the
// version_id matching the stored meta row and both part chunks handed to the
// stamper in one pass (US-001b). It uses a versioning-enabled bucket so the
// version_id is a real TimeUUID (not the null sentinel) — the case that
// previously left part chunks unrecoverable.
func TestCompleteMultipartStampsBackref(t *testing.T) {
	restore := s3api.SetMultipartMinPartSizeForTest(1)
	t.Cleanup(restore)

	spy := &stampSpyBackend{Backend: datamem.New()}
	h := newHarnessWithBackend(t, spy)
	h.mustStatus(h.doString("PUT", "/bkt", ""), 200)
	h.mustStatus(h.doString("PUT", "/bkt?versioning",
		`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`), 200)

	resp := h.doString("POST", "/bkt/mp?uploads", "")
	h.mustStatus(resp, 200)
	uploadID := uploadIDRE.FindStringSubmatch(h.readBody(resp))[1]

	var completeBody strings.Builder
	completeBody.WriteString("<CompleteMultipartUpload>")
	for pnum := 1; pnum <= 2; pnum++ {
		url := fmt.Sprintf("/bkt/mp?uploadId=%s&partNumber=%d", uploadID, pnum)
		pr := h.do("PUT", url, strings.NewReader(fmt.Sprintf("part-%d-body", pnum)))
		h.mustStatus(pr, 200)
		etag := strings.Trim(pr.Header.Get("Etag"), `"`)
		completeBody.WriteString(fmt.Sprintf(`<Part><PartNumber>%d</PartNumber><ETag>"%s"</ETag></Part>`, pnum, etag))
	}
	completeBody.WriteString("</CompleteMultipartUpload>")

	cr := h.doString("POST", "/bkt/mp?uploadId="+uploadID, completeBody.String())
	h.mustStatus(cr, http.StatusOK)
	storedVersion := cr.Header.Get("x-amz-version-id")
	require.NotEmpty(t, storedVersion, "completed multipart must return a version id")

	call := spy.lastCall(t)
	require.Equal(t, "mp", call.attrs.Key)
	require.Equal(t, storedVersion, call.attrs.VersionID,
		"stamped version_id must match the stored meta row")
	require.False(t, call.attrs.Mtime.IsZero(), "stamped mtime must be set")
	require.Len(t, call.chunkIDs, 2, "both part chunks must be re-stamped in one pass")

	// Cross-check the bucket id and version against the meta store rather than
	// a literal, and prove the stamped version_id resolves to the stored object.
	b, err := h.meta.GetBucket(context.Background(), "bkt")
	require.NoError(t, err)
	require.Equal(t, b.ID, call.attrs.BucketID)

	obj, err := h.meta.GetObject(context.Background(), b.ID, "mp", storedVersion)
	require.NoError(t, err)
	require.Equal(t, storedVersion, obj.VersionID)
}
