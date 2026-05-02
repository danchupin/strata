// Package health exposes /healthz (liveness) and /readyz (readiness)
// endpoints for the strata-gateway. Probes are injected by the cmd layer so
// the package stays free of backend imports (cassandra, rados).
package health

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// Probe checks one dependency. Implementations must return promptly and
// honour ctx cancellation.
type Probe func(ctx context.Context) error

// Handler serves liveness + readiness HTTP responses. Probes are evaluated
// concurrently with a per-request Timeout (default 1s); /readyz returns 200
// only when every probe returns nil.
type Handler struct {
	Probes  map[string]Probe
	Timeout time.Duration
}

const defaultTimeout = time.Second

// Healthz returns 200 with body "ok" without consulting any probe.
func (h *Handler) Healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// Readyz fans out probes concurrently and returns 200 only when all succeed
// before the timeout. Probe failures are written into the response body as
// "<name>: <error>" lines so operators can see which dependency is down.
func (h *Handler) Readyz(w http.ResponseWriter, r *http.Request) {
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	type result struct {
		name string
		err  error
	}
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []result
	)
	for name, probe := range h.Probes {
		wg.Add(1)
		go func(name string, probe Probe) {
			defer wg.Done()
			err := probe(ctx)
			mu.Lock()
			results = append(results, result{name: name, err: err})
			mu.Unlock()
		}(name, probe)
	}
	wg.Wait()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	failed := false
	body := make([]byte, 0, 64)
	for _, res := range results {
		if res.err != nil {
			failed = true
			body = append(body, []byte(res.name+": "+res.err.Error()+"\n")...)
		}
	}
	if failed {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(body)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
