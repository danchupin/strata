// Package workers wires background workers into `strata server`. Each
// worker registers a name and a Build constructor; the Supervisor in
// supervisor.go runs each requested worker in its own leader-elected
// goroutine with panic recovery and exponential-backoff restart.
package workers

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/leader"
	"github.com/danchupin/strata/internal/meta"
	strataotel "github.com/danchupin/strata/internal/otel"
	"github.com/danchupin/strata/internal/rebalance"
)

// Runner is the long-running loop a worker exposes. Run must honour ctx
// cancellation and return as soon as ctx is Done.
type Runner interface {
	Run(ctx context.Context) error
}

// RunnerFunc adapts a plain function into a Runner.
type RunnerFunc func(ctx context.Context) error

// Run satisfies Runner.
func (f RunnerFunc) Run(ctx context.Context) error { return f(ctx) }

// Dependencies carries the shared services every worker may need at Build
// time. Per-worker tunables live under deps.Cfg.Workers.<Worker>.* (env > TOML
// precedence applied at config.Load() time); workers reach for env directly
// only for vars owned by other config sections (e.g. STRATA_AUDIT_RETENTION
// — wired in a later cycle).
type Dependencies struct {
	Logger *slog.Logger
	Meta   meta.Store
	Data   data.Backend
	Tracer *strataotel.Provider
	Locker leader.Locker
	Region string
	// Cfg is the loaded gateway config. Workers consume their per-knob
	// tunables via cfg.Workers.<X>. Tests that build a Dependencies struct
	// without setting Cfg fall back to a fresh config.Load() (which honors
	// the in-test STRATA_* env vars set via t.Setenv); see workerCfg().
	Cfg *config.Config
	// EmitLeader is wired by the supervisor at Run-time so workers that
	// manage their own leader sessions (SkipLease=true) can publish lease
	// transitions on the supervisor's LeaderEvents channel. Workers under
	// the supervisor's lease never need to call this. May be nil when the
	// supervisor was built without a Run() invocation (tests).
	EmitLeader func(name string, acquired bool)
	// RebalanceProgress is the in-process draining-progress cache shared
	// between the rebalance worker (writer) and the adminapi
	// GET /admin/v1/clusters/{id}/drain-progress handler (reader). nil
	// disables the per-tick scan accumulator — the move-planning side of
	// the loop is unaffected. Wired by serverapp ahead of supervisor.Run.
	RebalanceProgress *rebalance.ProgressTracker
}

// Worker pairs a registered name with a constructor that builds the runner
// from the shared Dependencies. Build runs once per leader-acquisition;
// returning an error fails the current attempt and triggers backoff.
type Worker struct {
	Name  string
	Build func(deps Dependencies) (Runner, error)
	// SkipLease=true tells the supervisor to skip the per-worker
	// `<Name>-leader` lease. Used by workers that do their own
	// leader-election internally — e.g. the gc fan-out (US-004) takes per-
	// shard leases keyed `gc-leader-<shardID>` so a single replica can
	// drain multiple shards in parallel and lose only one shard's lease on
	// a panic. The supervisor still wraps Run with panic recovery and
	// backoff for these workers.
	SkipLease bool
}

var (
	regMu sync.RWMutex
	reg   = map[string]Worker{}
)

// Register adds a worker to the package-level registry. Panics on empty
// name, nil Build, or duplicate registration so programmer errors surface
// at process startup rather than at runtime.
func Register(w Worker) {
	if w.Name == "" {
		panic("workers.Register: empty Name")
	}
	if w.Build == nil {
		panic("workers.Register: nil Build for " + w.Name)
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := reg[w.Name]; dup {
		panic("workers.Register: duplicate registration for " + w.Name)
	}
	reg[w.Name] = w
}

// Lookup returns the worker registered under name and a found flag.
func Lookup(name string) (Worker, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	w, ok := reg[name]
	return w, ok
}

// Names returns every registered worker name in lexical order.
func Names() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(reg))
	for n := range reg {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Reset clears the registry. Intended for tests that mutate the global
// registry; production code never calls this.
func Reset() {
	regMu.Lock()
	defer regMu.Unlock()
	reg = map[string]Worker{}
}

// workerCfg returns deps.Cfg if set, otherwise loads a fresh Config from
// env + defaults. The fallback is the path tests take: they t.Setenv
// individual STRATA_* knobs and call buildX directly without plumbing a
// Cfg through Dependencies. Production callers always set deps.Cfg.
//
// A Load() failure in the fallback branch silently degrades to a zero
// Config — Build functions then see zero-valued knobs and apply their
// own type-zero handling. Defaults are merged from config.defaults() so
// this stays a quiet fall-through in tests; in production a Load failure
// would have already failed startup before any worker Build ran.
func workerCfg(deps Dependencies) *config.Config {
	if deps.Cfg != nil {
		return deps.Cfg
	}
	cfg, err := config.Load()
	if err != nil {
		return &config.Config{}
	}
	return cfg
}

// Resolve maps a list of names (already deduplicated by the caller) to the
// matching Worker entries in input order. Unknown names cause an immediate
// error so startup catches typos before any worker boots.
func Resolve(names []string) ([]Worker, error) {
	out := make([]Worker, 0, len(names))
	for _, n := range names {
		w, ok := Lookup(n)
		if !ok {
			return nil, fmt.Errorf("unknown worker %q (registered: %v)", n, Names())
		}
		out = append(out, w)
	}
	return out, nil
}
