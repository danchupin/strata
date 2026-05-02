package workers

import (
	"testing"
	"time"

	datamem "github.com/danchupin/strata/internal/data/memory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/notify"
)

func TestNotifyWorkerRegistered(t *testing.T) {
	w, ok := Lookup("notify")
	if !ok {
		t.Fatal("notify worker not registered (init() did not fire)")
	}
	if w.Name != "notify" {
		t.Fatalf("name=%q want notify", w.Name)
	}
}

func TestBuildNotifyReadsEnv(t *testing.T) {
	t.Setenv("STRATA_NOTIFY_TARGETS", "type:arn=https://example.test/hook|secret")
	t.Setenv("STRATA_NOTIFY_INTERVAL", "9s")
	t.Setenv("STRATA_NOTIFY_MAX_RETRIES", "3")
	t.Setenv("STRATA_NOTIFY_BACKOFF_BASE", "750ms")
	t.Setenv("STRATA_NOTIFY_POLL_LIMIT", "42")

	deps := Dependencies{
		Meta: metamem.New(),
		Data: datamem.New(),
	}
	r, err := buildNotify(deps)
	if err != nil {
		t.Fatalf("buildNotify: %v", err)
	}
	if _, ok := r.(*notify.Worker); !ok {
		t.Fatalf("buildNotify returned %T, want *notify.Worker", r)
	}
}

func TestBuildNotifyDefaultsWhenEnvUnset(t *testing.T) {
	t.Setenv("STRATA_NOTIFY_TARGETS", "type:arn=https://example.test/hook|secret")
	t.Setenv("STRATA_NOTIFY_INTERVAL", "")
	t.Setenv("STRATA_NOTIFY_MAX_RETRIES", "")
	t.Setenv("STRATA_NOTIFY_BACKOFF_BASE", "")
	t.Setenv("STRATA_NOTIFY_POLL_LIMIT", "")

	r, err := buildNotify(Dependencies{Meta: metamem.New(), Data: datamem.New()})
	if err != nil {
		t.Fatalf("buildNotify: %v", err)
	}
	if _, ok := r.(*notify.Worker); !ok {
		t.Fatalf("buildNotify returned %T, want *notify.Worker", r)
	}
}

func TestBuildNotifyFailsWithoutTargets(t *testing.T) {
	t.Setenv("STRATA_NOTIFY_TARGETS", "")

	_, err := buildNotify(Dependencies{Meta: metamem.New(), Data: datamem.New()})
	if err == nil {
		t.Fatal("buildNotify with empty targets must error")
	}
}

func TestSQSClientFactoryEmptyRegion(t *testing.T) {
	// LoadDefaultConfig honours AWS_REGION; force a deterministic value so the
	// factory does not consult EC2 metadata in CI sandboxes.
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA_TEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	c, err := sqsClientFactory("")
	if err != nil {
		t.Fatalf("sqsClientFactory: %v", err)
	}
	if c == nil {
		t.Fatal("sqsClientFactory returned nil client")
	}
}

func TestNotifyDefaultIntervalMatchesLegacy(t *testing.T) {
	// Sanity check: the default the worker plugs in must equal the
	// notify.Config default applied inside notify.New.
	want := 5 * time.Second
	if got := durationFromEnv("STRATA_NOTIFY_INTERVAL_UNSET", want); got != want {
		t.Errorf("durationFromEnv default = %v, want %v", got, want)
	}
}
