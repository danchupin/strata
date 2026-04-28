package main

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"
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
	for _, want := range []string{"usage: strata", "subcommands:", "server", "version"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q\n--- output ---\n%s", want, out)
		}
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
		if !strings.Contains(out, w) {
			t.Errorf("help missing worker name %q", w)
		}
	}
}

func TestServer_StubExitsNonZero(t *testing.T) {
	_, errOut, code := runApp(t, "server")
	if code == 0 {
		t.Fatalf("server stub should not exit 0 yet (US-003 wires the real entrypoint)")
	}
	if !strings.Contains(errOut, "not implemented") {
		t.Errorf("stub stderr missing 'not implemented': %s", errOut)
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
