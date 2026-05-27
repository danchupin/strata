package serverapp

import (
	"net/http"
	"net/http/pprof"
	"runtime"

	"github.com/danchupin/strata/internal/adminapi"
	"github.com/danchupin/strata/internal/config"
)

// pprofHandler builds a fresh *http.ServeMux carrying the standard
// /debug/pprof/* endpoints (US-004 prod-observability). The mux is
// returned without auth — the caller wraps it via
// adminapi.Server.WrapWithAdminAuth so the same session-cookie / SigV4
// chain that protects /admin/v1/* protects the profiler.
//
// Routes are wired explicitly (NOT via side-effect import of net/http/pprof)
// because Strata does not use http.DefaultServeMux — the side-effect import
// would silently leak the handlers onto a mux Strata never serves.
func pprofHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
	mux.Handle("/debug/pprof/block", pprof.Handler("block"))
	mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
	mux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
	return mux
}

// applyPprofProfilingRates flips on block + mutex profiling when the
// operator sets the corresponding env / TOML knob. runtime defaults leave
// both at 0 ("disabled"); the profiles still register but return empty
// data. Caller must invoke once at boot — repeated calls just reset the
// thresholds.
func applyPprofProfilingRates(cfg *config.Config) {
	if cfg.Pprof.BlockRate > 0 {
		runtime.SetBlockProfileRate(cfg.Pprof.BlockRate)
	}
	if cfg.Pprof.MutexRate > 0 {
		runtime.SetMutexProfileFraction(cfg.Pprof.MutexRate)
	}
}

// pprofWiring describes how the pprof handlers attach to the gateway.
// When Dedicated is non-nil, serverapp spawns a third listener bound to
// that handler; otherwise pprof routes attach to the admin mux at boot.
// Both branches share the same auth wrapper.
type pprofWiring struct {
	// Dedicated, when non-nil, is the standalone pprof listener mux. The
	// caller is responsible for launching http.Server.ListenAndServe.
	Dedicated http.Handler
	// AdminAttach, when non-nil, is the auth-wrapped handler that the
	// admin mux must register under "/debug/pprof/". Nil when Dedicated
	// is set (pprof attaches to its own listener) OR pprof is disabled.
	AdminAttach http.Handler
}

// resolvePprofWiring returns the wiring for cfg.Pprof. Returns the zero
// value when pprof is disabled. Config validation guarantees an enabled
// pprof has either cfg.Pprof.Listen non-empty OR an admin listener set.
func resolvePprofWiring(cfg *config.Config, admin *adminapi.Server) pprofWiring {
	if !cfg.Pprof.Enabled {
		return pprofWiring{}
	}
	wrapped := admin.WrapWithAdminAuth(pprofHandler())
	if cfg.Pprof.Listen != "" {
		return pprofWiring{Dedicated: wrapped}
	}
	return pprofWiring{AdminAttach: wrapped}
}
