package s3api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/danchupin/strata/internal/auth"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/racetest"
	"github.com/danchupin/strata/internal/s3api"
)

// newMemoryRaceFixture wires a racetest.Fixture against in-memory data and
// meta backends. The shared workload + invariants live in internal/racetest;
// this file is the per-backend constructor + the always-on TestRace*
// entrypoint.
func newMemoryRaceFixture(t *testing.T) *racetest.Fixture {
	t.Helper()
	d := datamem.New()
	m := metamem.New()
	api := s3api.New(d, m)
	api.Region = "default"
	api.Master = harnessMasterProvider{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.Header.Get(testPrincipalHeader); p != "" {
			ctx := auth.WithAuth(r.Context(), &auth.AuthInfo{Owner: p, AccessKey: p})
			r = r.WithContext(ctx)
		}
		api.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &racetest.Fixture{
		Server:      api,
		TS:          ts,
		Client:      racetest.NewClient(racetest.Workers),
		MemData:     d,
		AllVersions: m.AllObjectVersions,
	}
}

func TestRaceMixedOpsMemory(t *testing.T) {
	f := newMemoryRaceFixture(t)
	racetest.RunScenario(t, f)
	racetest.VerifyInvariants(t, f)
}

// TestRaceMultipartMemory drives the focused multipart-concurrency scenario
// (Complete-vs-Abort on one upload id + same-key/different-upload-id races)
// against the memory backend. Always-on so the -race CI job covers it; the
// TiKV variant lives in race_integration_test.go.
func TestRaceMultipartMemory(t *testing.T) {
	f := newMemoryRaceFixture(t)
	racetest.RunMultipartRaceScenario(t, f)
}

// TestRaceVersioningMemory drives the focused versioning/CAS-contention
// scenario (PUT-vs-delete-marker, SetObjectStorage CAS, suspended replace-null)
// against the memory backend. Always-on so the -race CI job covers it; the
// TiKV variant lives in race_integration_test.go.
func TestRaceVersioningMemory(t *testing.T) {
	f := newMemoryRaceFixture(t)
	racetest.RunVersioningRaceScenario(t, f)
}
