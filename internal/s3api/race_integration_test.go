//go:build integration

package s3api_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	tccassandra "github.com/testcontainers/testcontainers-go/modules/cassandra"
	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/auth"
	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/meta/cassandra"
	"github.com/danchupin/strata/internal/s3api"
)

// TestRaceMixedOpsCassandra runs the same race scenario as the memory variant
// against a Cassandra-backed meta.Store. Build-tag-gated to integration since
// it spins a testcontainers Cassandra (~1 minute to start) on every invocation.
func TestRaceMixedOpsCassandra(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := tccassandra.Run(ctx, "cassandra:5.0")
	if err != nil {
		t.Fatalf("start cassandra: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate: %v", err)
		}
	})

	host, err := container.ConnectionHost(ctx)
	if err != nil {
		t.Fatalf("connection host: %v", err)
	}

	ks := fmt.Sprintf("strata_race_%d", atomic.AddInt64(&raceKeyspaceSeq, 1))
	store, err := cassandra.Open(cassandra.SessionConfig{
		Hosts:       []string{host},
		Keyspace:    ks,
		LocalDC:     "datacenter1",
		Replication: "{'class': 'SimpleStrategy', 'replication_factor': '1'}",
		Timeout:     30 * time.Second,
	}, cassandra.Options{DefaultShardCount: 16})
	if err != nil {
		t.Fatalf("open keyspace: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	d := datamem.New()
	api := s3api.New(d, store)
	api.Region = "default"
	api.Master = harnessMasterProvider{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := r.Header.Get(testPrincipalHeader); p != "" {
			actx := auth.WithAuth(r.Context(), &auth.AuthInfo{Owner: p, AccessKey: p})
			r = r.WithContext(actx)
		}
		api.ServeHTTP(w, r)
	})
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	f := &raceFixture{
		server:  api,
		ts:      ts,
		client:  newRaceClient(),
		memData: d,
		allVersions: func(bucketID uuid.UUID) []*meta.Object {
			out, err := store.AllObjectVersions(context.Background(), bucketID)
			if err != nil {
				t.Errorf("all object versions: %v", err)
				return nil
			}
			return out
		},
	}

	runRaceScenario(t, f)
	verifyRaceInvariants(t, f)
}

var raceKeyspaceSeq int64
