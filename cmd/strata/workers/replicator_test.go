package workers

import (
	"testing"
	"time"

	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/replication"
)

func TestReplicatorWorkerRegistered(t *testing.T) {
	w, ok := Lookup("replicator")
	if !ok {
		t.Fatal("replicator worker not registered (init() did not fire)")
	}
	if w.Name != "replicator" {
		t.Fatalf("name=%q want replicator", w.Name)
	}
}

func TestBuildReplicatorReadsEnv(t *testing.T) {
	t.Setenv("STRATA_REPLICATOR_INTERVAL", "9s")
	t.Setenv("STRATA_REPLICATOR_MAX_RETRIES", "3")
	t.Setenv("STRATA_REPLICATOR_BACKOFF_BASE", "750ms")
	t.Setenv("STRATA_REPLICATOR_POLL_LIMIT", "42")
	t.Setenv("STRATA_REPLICATOR_HTTP_TIMEOUT", "11s")
	t.Setenv("STRATA_REPLICATOR_PEER_SCHEME", "http")

	deps := Dependencies{
		Meta: metamem.New(),
		Data: datamem.New(),
	}
	r, err := buildReplicator(deps)
	if err != nil {
		t.Fatalf("buildReplicator: %v", err)
	}
	if _, ok := r.(*replication.Worker); !ok {
		t.Fatalf("buildReplicator returned %T, want *replication.Worker", r)
	}
}

func TestBuildReplicatorDefaultsWhenEnvUnset(t *testing.T) {
	t.Setenv("STRATA_REPLICATOR_INTERVAL", "")
	t.Setenv("STRATA_REPLICATOR_MAX_RETRIES", "")
	t.Setenv("STRATA_REPLICATOR_BACKOFF_BASE", "")
	t.Setenv("STRATA_REPLICATOR_POLL_LIMIT", "")
	t.Setenv("STRATA_REPLICATOR_HTTP_TIMEOUT", "")
	t.Setenv("STRATA_REPLICATOR_PEER_SCHEME", "")

	r, err := buildReplicator(Dependencies{Meta: metamem.New(), Data: datamem.New()})
	if err != nil {
		t.Fatalf("buildReplicator: %v", err)
	}
	if _, ok := r.(*replication.Worker); !ok {
		t.Fatalf("buildReplicator returned %T, want *replication.Worker", r)
	}
}

func TestStringFromEnv(t *testing.T) {
	t.Setenv("STRATA_REPLICATOR_PEER_SCHEME_TEST", "")
	if got := stringFromEnv("STRATA_REPLICATOR_PEER_SCHEME_TEST", "https"); got != "https" {
		t.Errorf("stringFromEnv unset = %q, want https", got)
	}
	t.Setenv("STRATA_REPLICATOR_PEER_SCHEME_TEST", "http")
	if got := stringFromEnv("STRATA_REPLICATOR_PEER_SCHEME_TEST", "https"); got != "http" {
		t.Errorf("stringFromEnv set = %q, want http", got)
	}
}

func TestReplicatorDefaultIntervalMatchesLegacy(t *testing.T) {
	want := 5 * time.Second
	if got := durationFromEnv("STRATA_REPLICATOR_INTERVAL_UNSET", want); got != want {
		t.Errorf("durationFromEnv default = %v, want %v", got, want)
	}
}
