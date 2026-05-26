package serverapp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/danchupin/strata/cmd/strata/workers"
	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/trustedproxies"
)

// TestRateLimiterDisabledNoOp — PerKey=0 + PerIP=0 → newRateLimiter returns
// nil; Wrap on a nil receiver is the identity path.
func TestRateLimiterDisabledNoOp(t *testing.T) {
	cfg := &config.Config{}
	rl, err := newRateLimiter(cfg, nil)
	if err != nil {
		t.Fatalf("newRateLimiter: %v", err)
	}
	if rl != nil {
		t.Fatalf("expected nil limiter when both layers disabled, got %+v", rl)
	}
	called := atomic.Bool{}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Store(true) })
	h := rl.Wrap(next)
	r := httptest.NewRequest("GET", "/bucket/key", nil)
	r.RemoteAddr = "10.1.2.3:54321"
	h.ServeHTTP(httptest.NewRecorder(), r)
	if !called.Load() {
		t.Fatal("nil-limiter Wrap did not delegate to next")
	}
}

// TestRateLimiterPerIPFires — 5 req/sec + burst=1; second request inside
// the same tick MUST 429.
func TestRateLimiterPerIPFires(t *testing.T) {
	resetIngressCounters(t)
	cfg := &config.Config{RateLimit: config.RateLimitConfig{
		PerIP:     1,
		Burst:     1,
		CacheSize: 1000,
	}}
	rl, err := newRateLimiter(cfg, nil)
	if err != nil {
		t.Fatalf("newRateLimiter: %v", err)
	}
	if rl == nil {
		t.Fatal("expected non-nil limiter (PerIP=1)")
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	h := rl.Wrap(next)

	r := httptest.NewRequest("PUT", "/b/k", nil)
	r.RemoteAddr = "10.1.2.3:55555"

	rr1 := httptest.NewRecorder()
	h.ServeHTTP(rr1, r)
	if rr1.Code != 204 {
		t.Fatalf("first PUT code=%d want 204", rr1.Code)
	}

	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, r)
	if rr2.Code != http.StatusTooManyRequests {
		t.Fatalf("second PUT code=%d want 429", rr2.Code)
	}
	if got := rr2.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After=%q want 1", got)
	}
	if got := rr2.Header().Get("Content-Type"); got != "application/xml" {
		t.Errorf("Content-Type=%q want application/xml", got)
	}
	body, _ := io.ReadAll(rr2.Body)
	if !strings.Contains(string(body), "<Code>SlowDown</Code>") {
		t.Errorf("body missing <Code>SlowDown</Code>: %s", body)
	}
	if !strings.Contains(string(body), "<Message>Rate limit exceeded</Message>") {
		t.Errorf("body missing <Message>: %s", body)
	}
	if v := counterValue(metrics.IngressRateLimitRefused.WithLabelValues("ip")); v != 1 {
		t.Errorf("ip refused counter=%v want 1", v)
	}

	// After 1.1s the bucket refills; next request must succeed again.
	time.Sleep(1100 * time.Millisecond)
	rr3 := httptest.NewRecorder()
	h.ServeHTTP(rr3, r)
	if rr3.Code != 204 {
		t.Fatalf("post-refill code=%d want 204", rr3.Code)
	}
}

// TestRateLimiterPerKeyFires — PerKey=1 + burst=1; second request with same
// access key MUST 429 with reason=key.
func TestRateLimiterPerKeyFires(t *testing.T) {
	resetIngressCounters(t)
	cfg := &config.Config{RateLimit: config.RateLimitConfig{
		PerKey:    1,
		Burst:     1,
		CacheSize: 1000,
	}}
	rl, err := newRateLimiter(cfg, nil)
	if err != nil {
		t.Fatalf("newRateLimiter: %v", err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	h := rl.Wrap(next)

	ctx := auth.WithAuth(context.Background(), &auth.AuthInfo{AccessKey: "AKIATEST"})
	r := httptest.NewRequest("PUT", "/b/k", nil).WithContext(ctx)
	r.RemoteAddr = "10.1.2.3:55555"

	for i := range 2 {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if i == 0 && rr.Code != 204 {
			t.Fatalf("first code=%d want 204", rr.Code)
		}
		if i == 1 && rr.Code != http.StatusTooManyRequests {
			t.Fatalf("second code=%d want 429", rr.Code)
		}
	}
	if v := counterValue(metrics.IngressRateLimitRefused.WithLabelValues("key")); v != 1 {
		t.Errorf("key refused counter=%v want 1", v)
	}
	if v := counterValue(metrics.IngressRateLimitRefused.WithLabelValues("ip")); v != 0 {
		t.Errorf("ip refused counter=%v want 0 (PerIP disabled)", v)
	}
}

// TestRateLimiterPerKeyAnonymousBypasses — anonymous (IsAnonymous=true)
// requests skip the per-key layer entirely. Per-IP still fires.
func TestRateLimiterPerKeyAnonymousBypasses(t *testing.T) {
	resetIngressCounters(t)
	cfg := &config.Config{RateLimit: config.RateLimitConfig{
		PerKey:    1,
		Burst:     1,
		CacheSize: 1000,
	}}
	rl, _ := newRateLimiter(cfg, nil)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	h := rl.Wrap(next)

	ctx := auth.WithAuth(context.Background(), auth.AnonymousIdentity())
	r := httptest.NewRequest("PUT", "/b/k", nil).WithContext(ctx)
	r.RemoteAddr = "10.1.2.3:55555"
	for i := range 5 {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if rr.Code != 204 {
			t.Fatalf("anonymous request %d code=%d want 204 (key layer should skip)", i, rr.Code)
		}
	}
	if v := counterValue(metrics.IngressRateLimitRefused.WithLabelValues("key")); v != 0 {
		t.Errorf("key refused counter=%v want 0 (anonymous bypass)", v)
	}
}

// TestRateLimiterBurstAbsorbs — burst=5 lets 5 back-to-back requests pass
// even when the sustained rate is 1 req/sec.
func TestRateLimiterBurstAbsorbs(t *testing.T) {
	resetIngressCounters(t)
	cfg := &config.Config{RateLimit: config.RateLimitConfig{
		PerIP:     1,
		Burst:     5,
		CacheSize: 1000,
	}}
	rl, _ := newRateLimiter(cfg, nil)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	h := rl.Wrap(next)

	r := httptest.NewRequest("PUT", "/b/k", nil)
	r.RemoteAddr = "10.1.2.3:55555"

	pass := 0
	for range 5 {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if rr.Code == 204 {
			pass++
		}
	}
	if pass != 5 {
		t.Errorf("burst=5 absorbed %d/5 (want 5)", pass)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("6th request code=%d want 429", rr.Code)
	}
}

// TestRateLimiterLRUEvictionAtCap — when CacheSize is exceeded the oldest
// entry is evicted; the evicted client gets a fresh bucket on next hit.
func TestRateLimiterLRUEvictionAtCap(t *testing.T) {
	resetIngressCounters(t)
	cfg := &config.Config{RateLimit: config.RateLimitConfig{
		PerIP:     1,
		Burst:     1,
		CacheSize: 2,
	}}
	rl, _ := newRateLimiter(cfg, nil)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	h := rl.Wrap(next)

	doReq := func(ip string) int {
		r := httptest.NewRequest("PUT", "/b/k", nil)
		r.RemoteAddr = ip + ":1"
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr.Code
	}

	// Saturate IP1's bucket — next IP1 hit would 429.
	if doReq("10.0.0.1") != 204 {
		t.Fatal("ip1 first req should pass")
	}
	if doReq("10.0.0.1") != http.StatusTooManyRequests {
		t.Fatal("ip1 second req should 429")
	}
	// Two more IPs evict ip1's entry (cache size = 2).
	if doReq("10.0.0.2") != 204 {
		t.Fatal("ip2 first req should pass")
	}
	if doReq("10.0.0.3") != 204 {
		t.Fatal("ip3 first req should pass")
	}
	// ip1 now starts fresh — its previous limiter was evicted.
	if doReq("10.0.0.1") != 204 {
		t.Fatal("ip1 post-eviction should pass with fresh bucket")
	}
}

// TestRateLimiterClientIPThroughTrustedProxy — when r.RemoteAddr is in
// the trusted CIDR, X-Forwarded-For is honored and the per-IP cache keys
// on the forwarded client IP.
func TestRateLimiterClientIPThroughTrustedProxy(t *testing.T) {
	resetIngressCounters(t)
	tp, err := trustedproxies.Parse("10.0.0.0/8")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cfg := &config.Config{RateLimit: config.RateLimitConfig{
		PerIP:     1,
		Burst:     1,
		CacheSize: 1000,
	}}
	rl, _ := newRateLimiter(cfg, tp)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	h := rl.Wrap(next)

	// Two distinct forwarded clients via the same trusted hop. They MUST
	// land in separate buckets (otherwise the per-IP layer punishes the
	// LB IP — meaningless).
	for _, ip := range []string{"203.0.113.7", "198.51.100.42"} {
		r := httptest.NewRequest("PUT", "/b/k", nil)
		r.RemoteAddr = "10.5.5.5:5555"
		r.Header.Set("X-Forwarded-For", ip)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		if rr.Code != 204 {
			t.Fatalf("forwarded client %s code=%d want 204", ip, rr.Code)
		}
	}
	// Repeating the FIRST forwarded client now MUST 429 (its bucket
	// drained on the first call).
	r := httptest.NewRequest("PUT", "/b/k", nil)
	r.RemoteAddr = "10.5.5.5:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("repeat forwarded client code=%d want 429", rr.Code)
	}
}

// TestRateLimiterConfigNegativeFails — config.Load should reject negative
// rate-limit knobs at boot.
func TestRateLimiterConfigNegativeFails(t *testing.T) {
	for _, env := range []string{
		"STRATA_RATE_LIMIT_PER_KEY",
		"STRATA_RATE_LIMIT_PER_IP",
		"STRATA_RATE_LIMIT_BURST",
		"STRATA_RATE_LIMIT_CACHE_SIZE",
	} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("STRATA_DATA_BACKEND", "memory")
			t.Setenv("STRATA_META_BACKEND", "memory")
			t.Setenv(env, "-1")
			if _, err := config.Load(); err == nil {
				t.Fatalf("expected config.Load to reject %s=-1", env)
			}
		})
	}
}

// TestRateLimiterConfigBurstDefault — both layers set, Burst=0 → effective
// burst = 2 × max(per_key, per_ip).
func TestRateLimiterBurstAutoSize(t *testing.T) {
	cfg := &config.Config{RateLimit: config.RateLimitConfig{
		PerKey:    3,
		PerIP:     5,
		Burst:     0,
		CacheSize: 1000,
	}}
	rl, err := newRateLimiter(cfg, nil)
	if err != nil {
		t.Fatalf("newRateLimiter: %v", err)
	}
	if rl.keyBurst != 10 || rl.ipBurst != 10 {
		t.Errorf("burst=(%d,%d) want (10,10) = 2 × max(3,5)", rl.keyBurst, rl.ipBurst)
	}
}

// TestRateLimitEndToEndPerIP boots Run() against in-memory backends with
// STRATA_RATE_LIMIT_PER_IP=1 + burst=1 and proves a second consecutive
// PUT-like request from the same source IP returns HTTP 429 +
// <Code>SlowDown</Code> + Retry-After: 1, confirming the middleware
// landed on the gateway hot path (admin / metrics / healthz bypass).
func TestRateLimitEndToEndPerIP(t *testing.T) {
	addr := freePort(t)
	t.Setenv("STRATA_LISTEN", addr)
	t.Setenv("STRATA_DATA_BACKEND", "memory")
	t.Setenv("STRATA_META_BACKEND", "memory")
	t.Setenv("STRATA_AUTH_MODE", "off")
	t.Setenv("STRATA_SHUTDOWN_WAIT", "2s")
	t.Setenv("STRATA_RATE_LIMIT_PER_IP", "1")
	t.Setenv("STRATA_RATE_LIMIT_BURST", "1")
	t.Setenv("STRATA_RATE_LIMIT_CACHE_SIZE", "1000")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	runErr := make(chan error, 1)
	go func() { runErr <- Run(runCtx, cfg, logger, []workers.Worker{}) }()
	waitListen(t, addr)

	client := &http.Client{Timeout: 3 * time.Second}

	// /healthz bypasses rate limiting (registered on the mux directly).
	for range 5 {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err != nil {
			t.Fatalf("/healthz: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("/healthz status=%d want 200", resp.StatusCode)
		}
	}

	// S3 hot path — first hit allowed, second hit 429.
	resp1, err := client.Get("http://" + addr + "/somebucket/")
	if err != nil {
		t.Fatalf("first S3 req: %v", err)
	}
	resp1.Body.Close()
	resp2, err := client.Get("http://" + addr + "/somebucket/")
	if err != nil {
		t.Fatalf("second S3 req: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second S3 req status=%d want 429", resp2.StatusCode)
	}
	if got := resp2.Header.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After=%q want 1", got)
	}
	body, _ := io.ReadAll(resp2.Body)
	if !strings.Contains(string(body), "<Code>SlowDown</Code>") {
		t.Errorf("body missing <Code>SlowDown</Code>: %s", body)
	}

	cancel()
	<-runErr
}

func resetIngressCounters(t *testing.T) {
	t.Helper()
	// Counter values cannot be decremented in Prometheus; reset by
	// re-creating the vector for the duration of the test. Restored on
	// cleanup so other tests see the registered series.
	prev := metrics.IngressRateLimitRefused
	metrics.IngressRateLimitRefused = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "strata_ingress_rate_limit_refused_total",
			Help: "test scratch",
		},
		[]string{"reason"},
	)
	t.Cleanup(func() {
		metrics.IngressRateLimitRefused = prev
	})
}

func counterValue(c prometheus.Counter) float64 {
	return testutil.ToFloat64(c)
}
