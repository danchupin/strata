package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/danchupin/strata/cmd/strata/workers"
	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/logging"
	"github.com/danchupin/strata/internal/serverapp"
)

// knownWorker pairs a registered worker name with the per-worker env vars
// `strata server --help` documents. Per-worker tunables stay STRATA_* env
// vars per US-004 acceptance ("Per-worker tunables remain STRATA_*
// environment variables; the flag set is intentionally cross-cutting
// only.").
type knownWorker struct {
	name string
	envs []string
}

var knownWorkers = []knownWorker{
	{"gc", []string{
		"STRATA_GC_INTERVAL (default 30s) — drain tick interval",
		"STRATA_GC_GRACE (default 5m) — minimum age before a tombstoned chunk is eligible",
		"STRATA_GC_BATCH_SIZE (default 500) — max chunks ack'd per pass",
	}},
	{"lifecycle", []string{
		"STRATA_LIFECYCLE_INTERVAL (default 60s) — bucket scan tick interval",
		"STRATA_LIFECYCLE_UNIT (default day) — age unit for Days-based rules: second|minute|hour|day",
	}},
	{"notify", nil},
	{"replicator", nil},
	{"access-log", nil},
	{"inventory", nil},
	{"audit-export", nil},
	{"manifest-rewriter", nil},
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
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	applyServerFlagOverrides(flags)

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(a.err, "strata server: config:", err.Error())
		return 2
	}

	logger := logging.Setup()

	// Resolve the requested worker list against the package-level registry
	// before any backend is built so unknown names fail startup immediately
	// (US-004 acceptance: "unknown names cause immediate startup error").
	selected, err := workers.Resolve(parseWorkers(os.Getenv("STRATA_WORKERS")))
	if err != nil {
		fmt.Fprintln(a.err, "strata server: workers:", err.Error())
		return 2
	}

	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runServer(sigCtx, cfg, logger, selected); err != nil {
		logger.Error("strata server", "error", err.Error())
		return 1
	}
	return 0
}

// runServer is the body of the server subcommand: every cross-cutting
// initialisation has already happened (flag parse, env apply, config load,
// logger setup). Delegates to the shared serverapp.Run so cmd/strata-gateway
// stays bug-for-bug equivalent until US-014 deletes it.
func runServer(ctx context.Context, cfg *config.Config, logger *slog.Logger, selected []workers.Worker) error {
	return serverapp.Run(ctx, cfg, logger, selected)
}

// applyServerFlagOverrides promotes non-empty cross-cutting flags to the
// matching STRATA_* env vars before config.Load runs, so --flag overrides
// env which overrides defaults — the precedence required by US-003.
func applyServerFlagOverrides(f *serverFlags) {
	if f.listen != "" {
		os.Setenv("STRATA_LISTEN", f.listen)
	}
	if f.workers != "" {
		os.Setenv("STRATA_WORKERS", f.workers)
	}
	if f.authMode != "" {
		os.Setenv("STRATA_AUTH_MODE", f.authMode)
	}
	if f.vhostPattern != "" {
		os.Setenv("STRATA_VHOST_PATTERN", f.vhostPattern)
	}
	if f.logLevel != "" {
		os.Setenv("STRATA_LOG_LEVEL", f.logLevel)
	}
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
	for _, kw := range knownWorkers {
		fmt.Fprintf(a.out, "  %s\n", kw.name)
		for _, env := range kw.envs {
			fmt.Fprintf(a.out, "      %s\n", env)
		}
	}
	fmt.Fprintln(a.out)
	fmt.Fprintln(a.out, "Per-worker tunables remain STRATA_* environment variables; the flag set is")
	fmt.Fprintln(a.out, "intentionally cross-cutting only.")
}

// parseWorkers splits a comma-separated worker list and dedupes; empty
// entries are dropped. The result feeds workers.Resolve so unknown names
// fail startup before any backend is built.
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
