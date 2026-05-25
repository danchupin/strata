package workers

import (
	"testing"
	"time"

	"github.com/danchupin/strata/internal/accesslog"
	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

func TestAccessLogWorkerRegistered(t *testing.T) {
	w, ok := Lookup("access-log")
	if !ok {
		t.Fatal("access-log worker not registered (init() did not fire)")
	}
	if w.Name != "access-log" {
		t.Fatalf("name=%q want access-log", w.Name)
	}
}

func TestBuildAccessLogReadsEnv(t *testing.T) {
	t.Setenv("STRATA_ACCESS_LOG_INTERVAL", "9s")
	t.Setenv("STRATA_ACCESS_LOG_MAX_FLUSH_BYTES", "12345")
	t.Setenv("STRATA_ACCESS_LOG_POLL_LIMIT", "42")

	deps := Dependencies{
		Meta: metamem.New(),
		Data: datamem.New(),
	}
	r, err := buildAccessLog(deps)
	if err != nil {
		t.Fatalf("buildAccessLog: %v", err)
	}
	if _, ok := r.(*accesslog.Worker); !ok {
		t.Fatalf("buildAccessLog returned %T, want *accesslog.Worker", r)
	}
}

func TestBuildAccessLogDefaultsWhenEnvUnset(t *testing.T) {
	t.Setenv("STRATA_ACCESS_LOG_INTERVAL", "")
	t.Setenv("STRATA_ACCESS_LOG_MAX_FLUSH_BYTES", "")
	t.Setenv("STRATA_ACCESS_LOG_POLL_LIMIT", "")

	r, err := buildAccessLog(Dependencies{Meta: metamem.New(), Data: datamem.New()})
	if err != nil {
		t.Fatalf("buildAccessLog: %v", err)
	}
	if _, ok := r.(*accesslog.Worker); !ok {
		t.Fatalf("buildAccessLog returned %T, want *accesslog.Worker", r)
	}
}

func TestOrInt64(t *testing.T) {
	if got := orInt64(0, 5); got != 5 {
		t.Errorf("orInt64 unset = %d, want 5", got)
	}
	if got := orInt64(1024, 5); got != 1024 {
		t.Errorf("orInt64 set = %d, want 1024", got)
	}
}

func TestAccessLogDefaultIntervalMatchesLegacy(t *testing.T) {
	want := 5 * time.Minute
	if got := orDuration(0, want); got != want {
		t.Errorf("orDuration default = %v, want %v", got, want)
	}
}
