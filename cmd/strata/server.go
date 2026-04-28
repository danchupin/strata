package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
)

// knownWorkers lists every background-worker name a `--workers=` value may
// reference. Entries land in this slice as they migrate into the registry
// (US-005 onwards). Until then the names appear in --help so operators can
// see the planned shape.
var knownWorkers = []string{
	"gc",
	"lifecycle",
	"notify",
	"replicator",
	"access-log",
	"inventory",
	"audit-export",
	"manifest-rewriter",
}

// serverFlags is the cross-cutting flag set understood by `strata server`.
// Per-worker tunables are STRATA_* env vars only — the flag set stays small
// on purpose.
type serverFlags struct {
	listen       string
	workers      string
	authMode     string
	vhostPattern string
	logLevel     string
}

func newServerFlagSet(out *serverFlags, errWriter io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("strata server", flag.ContinueOnError)
	fs.SetOutput(errWriter)
	fs.StringVar(&out.listen, "listen", "", "HTTP listen address (overrides STRATA_LISTEN; default :9000)")
	fs.StringVar(&out.workers, "workers", "", "comma-separated worker names to run alongside the gateway (overrides STRATA_WORKERS)")
	fs.StringVar(&out.authMode, "auth-mode", "", "authentication mode: anonymous|sigv4 (overrides STRATA_AUTH_MODE)")
	fs.StringVar(&out.vhostPattern, "vhost-pattern", "", "comma-separated virtual-host suffix patterns; '-' to disable (overrides STRATA_VHOST_PATTERN)")
	fs.StringVar(&out.logLevel, "log-level", "", "log level: DEBUG|INFO|WARN|ERROR (overrides STRATA_LOG_LEVEL)")
	return fs
}

func (a *app) runServer(ctx context.Context, args []string) int {
	flags := &serverFlags{}
	fs := newServerFlagSet(flags, a.err)
	fs.Usage = func() { a.printServerHelp(fs) }

	if err := fs.Parse(args); err != nil {
		return 2
	}

	// US-003 wires the real gateway entrypoint here; the skeleton refuses to
	// start so a misconfigured invocation cannot silently no-op.
	fmt.Fprintln(a.err, "strata server: not implemented yet (see PRD US-003)")
	return 1
}

func (a *app) printServerHelp(fs *flag.FlagSet) {
	fmt.Fprintln(a.out, "usage: strata server [flags]")
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "Runs the S3 gateway and any background workers selected via --workers.")
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "Flags:")
	fs.SetOutput(a.out)
	fs.PrintDefaults()
	fs.SetOutput(a.err)
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "Known workers (selectable via --workers / STRATA_WORKERS):")
	for _, name := range knownWorkers {
		fmt.Fprintf(a.out, "  %s\n", name)
	}
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "Per-worker tunables remain STRATA_* environment variables; the flag set is")
	fmt.Fprintln(a.out, "intentionally cross-cutting only.")
}

// parseWorkers splits a comma-separated worker list and dedupes; empty entries
// are dropped. Used by US-004 once the registry lands; exposed here so the
// skeleton has at least one helper covered by tests.
func parseWorkers(spec string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, raw := range strings.Split(spec, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}
