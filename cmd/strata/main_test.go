package main

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

func runApp(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errBuf bytes.Buffer
	a := newApp(&out, &errBuf, args)
	code = a.run(context.Background())
	return out.String(), errBuf.String(), code
}

func TestRoot_NoArgsPrintsHelp(t *testing.T) {
	out, _, code := runApp(t)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, want := range []string{"usage: strata", "subcommands:", "server", "admin", "version"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestAdmin_HelpDispatchesToAdminPackage exercises the top-level dispatcher
// route to the admin subcommand. The admin help banner is printed to stderr
// by the flag package; we assert on the headline subcommand list so the
// dispatcher wiring (admin.RunWith honouring writers) stays correct.
func TestAdmin_HelpDispatchesToAdminPackage(t *testing.T) {
	_, errOut, code := runApp(t, "admin", "--help")
	// flag.ContinueOnError on --help returns flag.ErrHelp, which admin.Run
	// maps to ErrUsage → exit 2. The check that matters is that the help
	// banner reaches stderr.
	_ = code
	for _, want := range []string{"usage: strata admin", "iam", "lifecycle", "gc", "sse", "replicate", "bucket", "rewrap", "bench-gc", "bench-lifecycle"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("admin help missing %q\n--- stderr ---\n%s", want, errOut)
		}
	}
}

// TestAdmin_NoArgsExitsTwo: `strata admin` with no further args prints the
// usage banner and exits 2 (legacy strata-admin contract).
func TestAdmin_NoArgsExitsTwo(t *testing.T) {
	_, errOut, code := runApp(t, "admin")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut, "usage: strata admin") {
		t.Errorf("admin no-args missing usage banner: %s", errOut)
	}
}

// TestAdmin_UnknownSubcommandExitsTwo: pre-flag-parse routing falls into
// the group/sub switch which prints "unknown command:" then the banner.
func TestAdmin_UnknownSubcommandExitsTwo(t *testing.T) {
	_, errOut, code := runApp(t, "admin", "frobnicate", "subcmd")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut, "unknown command") {
		t.Errorf("admin unknown subcommand missing 'unknown command': %s", errOut)
	}
}

// TestAdmin_RewrapDryRun verifies the dispatcher routes `admin rewrap` to
// the rewrap subcommand. The dry-run path runs against an in-memory meta
// backend so the test has no external dependency.
func TestAdmin_RewrapDryRun(t *testing.T) {
	t.Setenv("STRATA_SSE_MASTER_KEYS", "active:"+strings.Repeat("11", 32))
	t.Setenv("STRATA_META_BACKEND", "memory")

	_, errOut, code := runApp(t, "admin", "rewrap", "--dry-run")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0\nstderr:\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "dry-run requested") {
		t.Errorf("missing dry-run log in stderr: %s", errOut)
	}
}

func TestRoot_HelpFlag(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		out, _, code := runApp(t, arg)
		if code != 0 {
			t.Fatalf("%s: exit code = %d, want 0", arg, code)
		}
		if !strings.Contains(out, "usage: strata") {
			t.Errorf("%s: help missing usage line", arg)
		}
	}
}

func TestRoot_UnknownSubcommand(t *testing.T) {
	_, errOut, code := runApp(t, "frobnicate")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut, "unknown subcommand") {
		t.Errorf("stderr missing 'unknown subcommand': %s", errOut)
	}
}

func TestVersion_PrintsSHAAndRuntime(t *testing.T) {
	out, _, code := runApp(t, "version")
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "sha=") {
		t.Errorf("version output missing sha=: %s", out)
	}
	if !strings.Contains(out, "runtime="+runtime.Version()) {
		t.Errorf("version output missing runtime=%s: %s", runtime.Version(), out)
	}
}

func TestServer_HelpListsFlagsAndWorkers(t *testing.T) {
	out, _, code := runApp(t, "server", "--help")
	// flag.ContinueOnError returns flag.ErrHelp which we map to exit 2;
	// regardless of code, the help text must include the cross-cutting
	// flag set and every known worker name.
	_ = code
	for _, want := range []string{
		"usage: strata server",
		"-listen",
		"-workers",
		"-auth-mode",
		"-vhost-pattern",
		"-log-level",
		"Known workers",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q\n--- output ---\n%s", want, out)
		}
	}
	for _, w := range knownWorkers {
		if !strings.Contains(out, w.name) {
			t.Errorf("help missing worker name %q", w.name)
		}
	}
	// Per-worker env-var documentation must surface in --help (US-005/US-006).
	for _, want := range []string{
		"STRATA_GC_INTERVAL", "STRATA_GC_GRACE", "STRATA_GC_BATCH_SIZE",
		"STRATA_LIFECYCLE_INTERVAL", "STRATA_LIFECYCLE_UNIT",
		"STRATA_NOTIFY_TARGETS", "STRATA_NOTIFY_INTERVAL", "STRATA_NOTIFY_MAX_RETRIES",
		"STRATA_NOTIFY_BACKOFF_BASE", "STRATA_NOTIFY_POLL_LIMIT",
		"STRATA_REPLICATOR_INTERVAL", "STRATA_REPLICATOR_MAX_RETRIES",
		"STRATA_REPLICATOR_BACKOFF_BASE", "STRATA_REPLICATOR_POLL_LIMIT",
		"STRATA_REPLICATOR_HTTP_TIMEOUT", "STRATA_REPLICATOR_PEER_SCHEME",
		"STRATA_ACCESS_LOG_INTERVAL", "STRATA_ACCESS_LOG_MAX_FLUSH_BYTES", "STRATA_ACCESS_LOG_POLL_LIMIT",
		"STRATA_INVENTORY_INTERVAL", "STRATA_INVENTORY_REGION",
		"STRATA_AUDIT_EXPORT_BUCKET", "STRATA_AUDIT_EXPORT_PREFIX",
		"STRATA_AUDIT_EXPORT_AFTER", "STRATA_AUDIT_EXPORT_INTERVAL",
		"STRATA_MANIFEST_REWRITER_INTERVAL", "STRATA_MANIFEST_REWRITER_BATCH_LIMIT",
		"STRATA_MANIFEST_REWRITER_DRY_RUN",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing worker env var %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestServer_StartsAndShutsDownOnContextCancel(t *testing.T) {
	// Force the gateway onto an ephemeral loopback port + memory backends so
	// the test never touches Cassandra/RADOS and does not collide with a
	// running process. STRATA_VHOST_PATTERN=- disables vhost rewriting,
	// keeping the harness independent of the test host's hostname.
	t.Setenv("STRATA_LISTEN", "127.0.0.1:0")
	t.Setenv("STRATA_DATA_BACKEND", "memory")
	t.Setenv("STRATA_META_BACKEND", "memory")
	t.Setenv("STRATA_AUTH_MODE", "off")
	t.Setenv("STRATA_VHOST_PATTERN", "-")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var out, errOut bytes.Buffer
	a := newApp(&out, &errOut, []string{"server"})

	done := make(chan int, 1)
	go func() { done <- a.run(ctx) }()

	// Give the listener a moment to bind, then trigger a clean shutdown.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("strata server exit code = %d, want 0\nstderr:\n%s", code, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("strata server did not shut down within 10s\nstderr:\n%s", errOut.String())
	}
}

func TestServer_RejectsUnknownWorkerName(t *testing.T) {
	t.Setenv("STRATA_LISTEN", "127.0.0.1:0")
	t.Setenv("STRATA_DATA_BACKEND", "memory")
	t.Setenv("STRATA_META_BACKEND", "memory")
	t.Setenv("STRATA_AUTH_MODE", "off")
	t.Setenv("STRATA_VHOST_PATTERN", "-")

	_, errOut, code := runApp(t, "server", "--workers=ghost")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 for unknown worker", code)
	}
	if !strings.Contains(errOut, "unknown worker") {
		t.Errorf("stderr missing 'unknown worker': %s", errOut)
	}
}

func TestParseWorkers(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"gc", []string{"gc"}},
		{"gc,lifecycle", []string{"gc", "lifecycle"}},
		{" gc , lifecycle , gc ", []string{"gc", "lifecycle"}},
		{",,", nil},
	}
	for _, tc := range cases {
		got := parseWorkers(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("parseWorkers(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseWorkers(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}
