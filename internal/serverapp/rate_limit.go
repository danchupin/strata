// Package serverapp — ingress rate limiter (US-009 harden-gateway).
//
// Two token-bucket layers — per-access-key + per-remote-IP — backed by a
// fixed-size LRU of `golang.org/x/time/rate.Limiter` entries. Both layers
// default off (PerKey=0, PerIP=0 = disabled) to preserve backwards-compat
// with the rest of the hardening cycle.
//
// Per-key keys on `auth.FromContext(ctx).AccessKey`; empty (anonymous
// mode, `_anon`) skips the per-key layer. Per-IP resolves through
// `*trustedproxies.TrustedProxies.ClientIP` so the IP-bucket key is the
// real client IP when a trusted proxy is upstream (otherwise the bare
// `r.RemoteAddr` host portion).
//
// On refusal: HTTP 429 + AWS S3 `<Code>SlowDown</Code>` body + `Retry-
// After: 1` + `strata_ingress_rate_limit_refused_total{reason}` increment.
// The per-key layer fires first when both layers are configured (so a
// hot key never punishes the IP wheel).
//
// LRU eviction on full: oldest entry evicted; the next hit for that
// (key|IP) gets a fresh token bucket — conservative (forgets recent
// usage). Per-IP IPv6 entries are stored as the literal address; the
// caller is expected to canonicalise (no /64-aggregation).
//
// Both limiters apply to S3 hot path only — `serverapp.Run` wires the
// middleware between auth + audit on the s3Chain. Admin / console /
// metrics / healthz / readyz endpoints register on a different mux (or
// register directly when the admin listener is split) and bypass the
// chain entirely.

package serverapp

import (
	"encoding/xml"
	"net"
	"net/http"
	"strings"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/time/rate"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/metrics"
	"github.com/danchupin/strata/internal/trustedproxies"
)

// rateLimiter holds the two LRU-backed token-bucket caches plus the
// per-layer rate + burst. Cache shape is `*lru.Cache[string, *rate.Limiter]`;
// the Limiter zero value would be useless (rate=0 = never), so the entries
// are constructed lazily by `entry()`.
//
// A nil receiver is safe — the Middleware short-circuits to next.ServeHTTP.
type rateLimiter struct {
	keyCache *lru.Cache[string, *rate.Limiter]
	ipCache  *lru.Cache[string, *rate.Limiter]

	keyRate  float64
	keyBurst int
	ipRate   float64
	ipBurst  int

	trusted *trustedproxies.TrustedProxies
}

// newRateLimiter builds a rate limiter from cfg. Both layers off → returns
// nil so the caller wires the no-op path. Cache size <=0 falls back to the
// default cap.
//
// Burst falls back to max(PerKey, PerIP, 1) × 2 when zero — covers the
// "short spike" case without requiring an operator to compute the value.
func newRateLimiter(cfg *config.Config, trusted *trustedproxies.TrustedProxies) (*rateLimiter, error) {
	if cfg.RateLimit.PerKey == 0 && cfg.RateLimit.PerIP == 0 {
		return nil, nil
	}
	size := cfg.RateLimit.CacheSize
	if size <= 0 {
		size = 100_000
	}
	burst := cfg.RateLimit.Burst
	if burst == 0 {
		burst = max(1, 2*max(cfg.RateLimit.PerKey, cfg.RateLimit.PerIP))
	}
	rl := &rateLimiter{
		keyRate:  float64(cfg.RateLimit.PerKey),
		keyBurst: burst,
		ipRate:   float64(cfg.RateLimit.PerIP),
		ipBurst:  burst,
		trusted:  trusted,
	}
	if cfg.RateLimit.PerKey > 0 {
		c, err := lru.New[string, *rate.Limiter](size)
		if err != nil {
			return nil, err
		}
		rl.keyCache = c
	}
	if cfg.RateLimit.PerIP > 0 {
		c, err := lru.New[string, *rate.Limiter](size)
		if err != nil {
			return nil, err
		}
		rl.ipCache = c
	}
	return rl, nil
}

// Wrap returns next when the limiter is nil (no-op for the disabled
// path), otherwise wraps with the per-key + per-IP allowance check.
func (l *rateLimiter) Wrap(next http.Handler) http.Handler {
	if l == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allow consults both layers in order: per-key first (caps the noisy
// signed client), per-IP second (catches anonymous floods). Refusal
// emits 429 directly and returns false.
func (l *rateLimiter) allow(w http.ResponseWriter, r *http.Request) bool {
	if l.keyCache != nil {
		if key := accessKeyOf(r); key != "" {
			lim := l.entry(l.keyCache, key, l.keyRate, l.keyBurst)
			if !lim.Allow() {
				writeRateLimitRefused(w, r, "key")
				return false
			}
		}
	}
	if l.ipCache != nil {
		if ip := l.clientIP(r); ip != "" {
			lim := l.entry(l.ipCache, ip, l.ipRate, l.ipBurst)
			if !lim.Allow() {
				writeRateLimitRefused(w, r, "ip")
				return false
			}
		}
	}
	return true
}

// entry resolves the (cache, key) pair through the LRU; constructs a
// fresh `rate.Limiter` on miss. `lru.Cache.Get` is concurrent-safe;
// `Add` is also concurrent-safe — the rare race where two requests
// Add() at the same time produces one wasted Limiter (the second Add
// evicts the first; both requests then proceed against their own
// instance for the current request). Acceptable — the next request
// resolves to whichever ended up in the cache.
func (l *rateLimiter) entry(cache *lru.Cache[string, *rate.Limiter], key string, perSec float64, burst int) *rate.Limiter {
	if lim, ok := cache.Get(key); ok {
		return lim
	}
	lim := rate.NewLimiter(rate.Limit(perSec), burst)
	cache.Add(key, lim)
	return lim
}

// clientIP picks the per-IP cache key. Per-IP is keyed on the resolved
// client IP from US-007's trusted-proxies pipeline so a request behind a
// trusted load balancer is rate-limited by its real source, not the LB.
// Untrusted source → `r.RemoteAddr` host portion.
func (l *rateLimiter) clientIP(r *http.Request) string {
	if l.trusted != nil {
		return l.trusted.ClientIP(r)
	}
	return remoteHostOnly(r.RemoteAddr)
}

func accessKeyOf(r *http.Request) string {
	ai := auth.FromContext(r.Context())
	if ai == nil || ai.IsAnonymous {
		return ""
	}
	return ai.AccessKey
}

func remoteHostOnly(addr string) string {
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// writeRateLimitRefused emits the AWS S3 SlowDown response and increments
// the per-reason refused counter. Body shape matches the rest of the S3
// XML error contract (see internal/s3api/errors.go) so SDKs surface a
// consistent error code regardless of which middleware emitted it.
func writeRateLimitRefused(w http.ResponseWriter, r *http.Request, reason string) {
	metrics.IngressRateLimitRefused.WithLabelValues(reason).Inc()
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusTooManyRequests)
	resource, _, _ := strings.Cut(r.URL.Path, "?")
	_ = xml.NewEncoder(w).Encode(rateLimitErrorXML{
		Code:     "SlowDown",
		Message:  "Rate limit exceeded",
		Resource: resource,
	})
}

type rateLimitErrorXML struct {
	XMLName  xml.Name `xml:"Error"`
	Code     string
	Message  string
	Resource string
}
