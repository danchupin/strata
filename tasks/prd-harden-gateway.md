# PRD: Harden Gateway (Cycle A — Prod-Readiness)

## Introduction

Cycle A of the 2026-05-25 prod-readiness audit closes six P0 HTTP-surface gaps surfaced before any critical-prod cutover. Strata today exposes a plain `http.Server` with no timeouts, no built-in TLS, no separate admin listener, no source-IP rate limiting, and blindly trusts forwarded headers — every one of which would cause Strata to fail a basic external pentest, k8s NetworkPolicy review, or security-team gate.

This cycle delivers:

1. **HTTP server timeouts** (closes slowloris + connection-exhaustion vector).
2. **Built-in TLS listener** with SNI multi-cert + hot-reload (operator-terminated TLS without an external sidecar).
3. **mTLS to all backend connections** (Cassandra / TiKV / S3 upstream; RADOS doc-only via cephx).
4. **Trusted-proxies-aware forwarded-header parsing** (closes `X-Forwarded-Proto` blind-trust).
5. **Admin endpoint on a separate listener** (network-level isolation from S3 surface).
6. **Per-IP + per-access-key ingress rate limiter** (DoS protection on the S3 hot path).

US-010 closes the cycle with smoke validation, new operator docs, and the ROADMAP entry flip to Done.

After this cycle Strata can be exposed on a public network with operator-issued certificates and survive a basic external scan without changing default behavior — every hardening knob is opt-in (empty default keeps existing labs / smoke / CI working unchanged).

## User Journey

An operator preparing to cut over a critical-prod workload to Strata walks through this path:

1. **Read the new TLS termination runbook** at `/operate/tls-termination.md`. The page explains four supported deploy shapes:
   - **(A)** plain HTTP behind nginx-ingress / Envoy with TLS termination at the ingress (current default; documented for backwards-compat).
   - **(B)** Strata-terminated TLS single-port (cert mounted as k8s Secret; operator runs Strata's HTTPS listener directly).
   - **(C)** Strata-terminated TLS with split admin / S3 ports (admin listener bound to 127.0.0.1 or behind separate NetworkPolicy).
   - **(D)** Strata-terminated TLS + mTLS to backends (production-grade; cert-manager provisions client certs for Cassandra/TiKV/S3-upstream).
2. **Provision certificates** via cert-manager (operator's `Certificate` CRD), Vault PKI, or openssl. Mount as k8s `Secret`.
3. **Read `/best-practices/production-hardening.md`** (new page) checklist: 12 line items covering TLS, mTLS, trusted proxies, rate limits, admin isolation, timeouts, backend mTLS. Each line points back to the runbook section.
4. **Set environment**:
   ```
   STRATA_TLS_CERT_FILE=/etc/strata/tls/tls.crt
   STRATA_TLS_KEY_FILE=/etc/strata/tls/tls.key
   STRATA_TLS_MIN_VERSION=TLS1.3
   STRATA_TLS_CIPHER_PROFILE=mozilla-modern
   STRATA_ADMIN_LISTEN=127.0.0.1:9001
   STRATA_TRUSTED_PROXIES=10.0.0.0/8
   STRATA_RATE_LIMIT_PER_KEY=1000
   STRATA_RATE_LIMIT_PER_IP=10000
   STRATA_HTTP_READ_HEADER_TIMEOUT=10s
   STRATA_HTTP_WRITE_TIMEOUT=30m
   STRATA_TIKV_TLS_CA_FILE=/etc/strata/tikv-ca.crt
   STRATA_TIKV_TLS_CERT_FILE=/etc/strata/tikv-client.crt
   STRATA_TIKV_TLS_KEY_FILE=/etc/strata/tikv-client.key
   ```
5. **Roll Strata**. The operator runs `make smoke-harden-gateway` against the staging lab; the new smoke script probes:
   - HTTPS handshake completes; HTTP/2 negotiated.
   - mTLS to TiKV succeeds (probe via admin `/admin/v1/storage/meta`).
   - Slowloris-style connection (open TCP, no headers) is dropped at `STRATA_HTTP_READ_HEADER_TIMEOUT`.
   - Rate limiter fires at the configured threshold; HTTP 429 returned with `Retry-After: 1` header + `<Code>SlowDown</Code>` body.
   - Admin listener bound to loopback only (`curl http://10.x.x.x:9001/admin/v1/clusters` from a peer container → connection refused).
   - `X-Forwarded-Proto: https` from a peer container OUTSIDE `STRATA_TRUSTED_PROXIES` → cookie issued WITHOUT `Secure` flag.
   - `X-Forwarded-Proto: https` from a peer container INSIDE `STRATA_TRUSTED_PROXIES` → cookie issued WITH `Secure` flag.
6. **Operator marks the production-hardening checklist** in their internal runbook; rolls Strata to prod with `STRATA_AUTH_MODE=required` + the env above.

Every step works against the existing TiKV-default lab without code changes — opt-in knobs preserve the current zero-config dev experience.

## Goals

- Close all six P0 HTTP-surface gaps from the 2026-05-25 audit.
- Every new knob is opt-in (empty default = current behavior preserved).
- No regression on existing smoke / CI / s3-tests / e2e suites.
- Wire all ~18 new STRATA_* env vars through the ralph/toml-parity contract (Config struct + envMap + strata.toml.example + /reference/env-vars.md).
- Ship two new operator docs (`/operate/tls-termination.md`, `/best-practices/production-hardening.md`).
- Counter family `strata_ingress_rate_limit_refused_total{reason}` observable in Grafana.

## User Stories

### US-001: HTTP server timeouts + ROADMAP entry creation
**Description:** As an SRE, I want the gateway HTTP server to enforce per-connection timeouts so a slowloris attack or stuck client cannot exhaust the connection pool. As a maintainer, I want the ROADMAP entry for this cycle created up-front so the close-flip protocol from prior cycles is preserved.

**Acceptance Criteria:**
- [ ] `internal/serverapp/serverapp.go::Run` sets `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout`, `MaxHeaderBytes` on the `http.Server` struct (current `&http.Server{Addr, Handler}` at line 281).
- [ ] Defaults: `ReadHeaderTimeout=10s`, `ReadTimeout=60s`, `WriteTimeout=30m` (large slow PUT over cellular / transcontinental safe; AWS S3 default is unbounded; 30m is a conservative middle ground), `IdleTimeout=120s`, `MaxHeaderBytes=1<<20` (1 MiB).
- [ ] Each value overridable via env: `STRATA_HTTP_READ_HEADER_TIMEOUT`, `STRATA_HTTP_READ_TIMEOUT`, `STRATA_HTTP_WRITE_TIMEOUT`, `STRATA_HTTP_IDLE_TIMEOUT`, `STRATA_HTTP_MAX_HEADER_BYTES`. Value `0` accepted = disabled (matches Go `http.Server` zero-value semantic; useful for dev-mode).
- [ ] Wired through `Config.HTTP.*` substruct via koanf; `envMap` registry updated; `internal/config/exempt_env_vars.go` NOT extended (every knob has TOML parity).
- [ ] `deploy/strata.toml.example` `[http]` section added with all 5 keys + one-line comment per knob.
- [ ] `/reference/env-vars.md` `TOML key` column populated for all 5 rows.
- [ ] Range validation: timeouts ≥ 0 (negative reject; 0 = disabled per Go semantic). Upper bound `STRATA_HTTP_WRITE_TIMEOUT` 24h; `STRATA_HTTP_MAX_HEADER_BYTES` 16<<20 (16 MiB).
- [ ] New unit test `internal/serverapp/timeouts_test.go` verifies (a) defaults set when envs unset, (b) custom envs override, (c) `STRATA_HTTP_WRITE_TIMEOUT=0` accepted = disabled, (d) negative env rejected at boot with fail-fast error.
- [ ] **Slowloris chaos test** `internal/serverapp/timeouts_chaos_test.go` (Go-native, not shell): opens raw TCP via `net.Dial`, writes one byte every 100ms past `ReadHeaderTimeout`, asserts connection dropped within `ReadHeaderTimeout + 5s` slack.
- [ ] **ROADMAP entry created** in this US's commit under `## Correctness & consistency`:
  ```
  - **P0 — Cycle A: harden-gateway (HTTP timeouts + built-in TLS + backend mTLS + trusted proxies + admin listener split + ingress rate limit).** In progress on `ralph/harden-gateway`. Closes 6 P0 gaps from the 2026-05-25 prod-readiness audit. Flipped Done on cycle close.
  ```
  (Entry will be flipped to `~~Done~~` in US-010.)
- [ ] Typecheck + `go vet ./...` + `make test-race` pass.

### US-002a: Built-in TLS listener (single-cert + min-version + cipher profile)
**Description:** As an operator, I want Strata to terminate TLS itself with operator-supplied certificates so I can deploy without an external TLS sidecar.

**Acceptance Criteria:**
- [ ] `internal/serverapp/serverapp.go` branches between `ListenAndServe()` (default, empty cert envs) and `ListenAndServeTLS(cert, key)` (cert + key files set).
- [ ] `STRATA_TLS_CERT_FILE` + `STRATA_TLS_KEY_FILE` paths read at boot; cert loaded once via `tls.LoadX509KeyPair`.
- [ ] `STRATA_TLS_MIN_VERSION` env: default `TLS1.2`, accepted values `TLS1.2` | `TLS1.3`; rejection on other values at boot.
- [ ] `STRATA_TLS_CIPHER_PROFILE` env (enum, NOT free-form cipher list — prevents accidental weak-cipher selection). Accepted values:
  - `mozilla-modern` (default): TLS 1.3 only ciphers (TLS_AES_128_GCM_SHA256, TLS_AES_256_GCM_SHA384, TLS_CHACHA20_POLY1305_SHA256).
  - `mozilla-intermediate`: TLS 1.2 + 1.3 AEAD ciphers only (per [Mozilla SSL Config Generator](https://ssl-config.mozilla.org/) Intermediate profile).
  - `go-default`: current Go safe set via `tls.CipherSuites()` (no insecure suites).
  - Unknown profile fail-fast at boot.
- [ ] When `STRATA_TLS_MIN_VERSION=TLS1.3`, `STRATA_TLS_CIPHER_PROFILE` is doc-noted as informational only (Go's `tls` package does not honor cipher list for TLS 1.3).
- [ ] All envs wired through `Config.TLS.*` substruct + envMap + TOML example + reference page.
- [ ] New `internal/serverapp/tls_test.go` verifies (a) HTTPS handshake against in-memory test cert, (b) `STRATA_TLS_MIN_VERSION=TLS1.3` rejects TLS 1.2 client, (c) invalid cipher profile fails at boot, (d) `mozilla-modern` profile rejects RC4 client even when client offers it.
- [ ] Smoke script `scripts/smoke-tls.sh` runs Strata with self-signed cert, validates HTTPS handshake + HTTP/2 negotiation + cert content via openssl s_client.
- [ ] `make smoke` (TiKV-default lab, TLS not configured) still passes — opt-in default preserved.
- [ ] Typecheck + `make test-race` pass.

### US-002b: TLS SNI multi-cert + hot-reload (k8s-aware)
**Description:** As an operator running multi-tenant Strata, I want SNI-driven multi-certificate selection and certificate hot-reload so I can rotate certs via cert-manager without restarting the gateway.

**Acceptance Criteria:**
- [ ] `STRATA_TLS_CERT_DIR` env (mutually exclusive with single-cert envs — fail-fast at boot if both set). Directory walked at boot for `*.crt` + matching `*.key` pairs; cert SAN/CN extracted; `tls.Config.GetCertificate` callback dispatches via SNI ServerName lookup.
- [ ] **Cert store backed by `atomic.Pointer[map[string]*tls.Certificate]`** (NOT RWMutex — read-side is hot per-handshake). Reload-side writes new pointer atomically; old pointer remains valid for in-flight handshakes (Go GC reclaims when no references).
- [ ] **Hot-reload via fsnotify** on the cert dir AND single-cert paths from US-002a.
- [ ] **k8s ConfigMap / Secret atomic-symlink-swap aware**: Kubelet swaps target via symlink rename, so fsnotify on the file itself misses the modify event. Watch shape:
  - For single-cert: watch the *parent directory* for `RENAME` + `CREATE` events on the symlink basename.
  - For `STRATA_TLS_CERT_DIR`: watch the directory recursively; trigger reload on any `CREATE` / `RENAME` / `WRITE` event for `*.crt` or `*.key`.
- [ ] **Periodic reconciliation fallback** every 60s (fsnotify watchers can drop events under load): re-stat all cert files; if mtime / inode changed, reload. Knob `STRATA_TLS_RELOAD_INTERVAL` (default 60s, range [10s, 1h], 0 = disabled).
- [ ] Optional client-cert verification: `STRATA_TLS_CLIENT_CA_FILE` (PEM) — when set, `ClientAuth = RequireAndVerifyClientCert` enabled. Used for backend-to-backend (admin endpoint mTLS routed through US-005).
- [ ] All envs wired through `Config.TLS.*` substruct + envMap + TOML example + reference page.
- [ ] New `internal/serverapp/tls_reload_test.go` verifies (a) SNI multi-cert dispatch picks correct cert per ServerName, (b) atomic swap on cert file modify continues in-flight handshakes with old cert, new handshakes get new cert, (c) k8s symlink-swap scenario (rename + symlink-target change) triggers reload, (d) periodic reconciliation catches missed fsnotify event (simulated by skipping fsnotify watcher entirely).
- [ ] Integration test `internal/serverapp/tls_k8s_mount_test.go` (build tag `integration`): docker-driven test that mounts a directory mimicking the kubelet Secret-mount layout (`..data` symlink + atomic rotation) and verifies hot-reload picks up cert change.
- [ ] Typecheck + `make test-race` pass.

### US-003a: Cassandra mTLS
**Description:** As a security-conscious operator, I want Strata to authenticate to Cassandra via mutual TLS so a network intruder cannot impersonate the gateway.

**Acceptance Criteria:**
- [ ] `internal/meta/cassandra/store.go` `gocql.ClusterConfig` builder extended to wire `gocql.SslOptions` from env: `STRATA_CASSANDRA_TLS_CA_FILE`, `STRATA_CASSANDRA_TLS_CERT_FILE`, `STRATA_CASSANDRA_TLS_KEY_FILE`, `STRATA_CASSANDRA_TLS_SKIP_VERIFY` (default false; range bool).
- [ ] `STRATA_CASSANDRA_TLS_SKIP_VERIFY=true` logs single WARN at boot ("Cassandra TLS verification disabled — never set in production") + bumps gauge `strata_backend_tls_skip_verify{backend="cassandra"}=1`.
- [ ] All four envs wired through `Config.Meta.Cassandra.TLS` substruct + envMap + TOML example + reference page.
- [ ] Integration test `internal/meta/cassandra/tls_integration_test.go` (build tag `integration`, testcontainers-based): provisions self-signed CA + Cassandra server cert + Strata client cert at test time; verifies connection succeeds with valid certs + fails with wrong CA + fails with no client cert (when server requires).
- [ ] Empty TLS envs → no SslOptions = current plain-TCP behavior preserved.
- [ ] Typecheck + `make test-integration` + `make test-race` pass.

### US-003b: TiKV mTLS
**Description:** As a security-conscious operator, I want Strata to authenticate to TiKV via mutual TLS so a network intruder cannot impersonate the gateway.

**Acceptance Criteria:**
- [ ] `internal/meta/tikv/store.go` + `internal/meta/tikv/kv_tikv.go` config builder extended to populate `tikv-client-go` `config.Security` struct: `STRATA_TIKV_TLS_CA_FILE`, `STRATA_TIKV_TLS_CERT_FILE`, `STRATA_TIKV_TLS_KEY_FILE`, `STRATA_TIKV_TLS_SKIP_VERIFY`.
- [ ] PD endpoint scheme respect: when TLS envs set, PD endpoint URLs accept `https://` scheme (current `internal/meta/tikv/pdclient.go` may need TLS-aware http.Client).
- [ ] `STRATA_TIKV_TLS_SKIP_VERIFY=true` logs WARN + bumps gauge `strata_backend_tls_skip_verify{backend="tikv"}=1`.
- [ ] All four envs wired through `Config.Meta.TiKV.TLS` substruct + envMap + TOML example + reference page.
- [ ] Integration test `internal/meta/tikv/tls_integration_test.go` (build tag `integration`): provisions TLS-enabled PD + TiKV containers via testcontainers; verifies handshake.
- [ ] Empty TLS envs → no Security config = current plain-gRPC behavior preserved.
- [ ] Typecheck + `make test-integration` + `make test-race` pass.

### US-003c: S3-upstream mTLS + RADOS cephx doc-only note
**Description:** As an operator using S3-pass-through, I want Strata to authenticate to the upstream S3 endpoint via mutual TLS — globally and per-cluster.

**Acceptance Criteria:**
- [ ] `internal/data/s3/backend.go::connFor` extended: per-cluster `*awss3.Client` swaps `aws.Config.HTTPClient` with a `*http.Client` whose `Transport` carries a `*tls.Config` built from CA + client cert env.
- [ ] **Global default** envs: `STRATA_S3_TLS_CA_FILE`, `STRATA_S3_TLS_CERT_FILE`, `STRATA_S3_TLS_KEY_FILE`, `STRATA_S3_TLS_SKIP_VERIFY` apply when no per-cluster TLS override is present.
- [ ] **Per-cluster override** via existing `STRATA_S3_CLUSTERS` JSON schema (ralph/s3-multi-cluster contract): new optional `tls: {ca_file, cert_file, key_file, skip_verify}` field per cluster spec. **Resolution rule: per-cluster wins; global as fallback** (any single per-cluster TLS key replaces the global block entirely for that cluster — no merge to avoid surprise).
- [ ] Per-cluster `tls` field is additive (omitting it preserves backwards-compat; existing JSON parses unchanged).
- [ ] `STRATA_S3_TLS_SKIP_VERIFY=true` (any layer) logs WARN + bumps gauge `strata_backend_tls_skip_verify{backend="s3",cluster="<id>"}=1`.
- [ ] All envs wired through `Config.Data.S3.TLS` substruct + envMap + TOML example + reference page.
- [ ] **RADOS doc-only**: new section in `/operate/tls-termination.md` titled "RADOS uses cephx, not TLS" explaining cephx is the production-supported mutual-auth path; mTLS layer is N/A; no code change for RADOS.
- [ ] Unit test `internal/data/s3/tls_test.go` wraps `httptest.NewTLSServer` and verifies (a) global TLS config honored, (b) per-cluster override beats global, (c) skip_verify warn-and-bump fires.
- [ ] Typecheck + `make test-race` pass.

### US-004: Trusted-proxies-aware forwarded-header parsing
**Description:** As a security-conscious operator, I want the gateway to ignore `X-Forwarded-*` headers from untrusted sources so a malicious client cannot spoof the `Secure` cookie flag or the source IP in audit logs.

**Acceptance Criteria:**
- [ ] New `internal/serverapp/trusted_proxies.go` package with `TrustedProxies` type carrying a parsed `[]net.IPNet` slice + `Contains(remoteAddr string) bool` method.
- [ ] Parsed at boot from `STRATA_TRUSTED_PROXIES` (comma-separated CIDR list, e.g. `10.0.0.0/8,192.168.0.0/16`). Invalid CIDR → fail-fast at boot with explicit error message naming the bad entry.
- [ ] Default empty → forwarded headers NEVER trusted (cookies require actual TLS termination on the Strata listener; audit log uses `r.RemoteAddr` directly).
- [ ] Replace **all** blind-trust call sites (audit-grep at US-004 implementation; today's grep finds exactly 2):
  - `internal/adminapi/auth.go:100` (`X-Forwarded-Proto` → secure cookie flag).
  - `internal/s3api/notification.go:112` (`X-Forwarded-For` → notification event source-ip field).
  - Any other `r.Header.Get("X-Forwarded-*")` or `r.Header.Get("X-Real-IP")` discovered via `grep -rEn 'X-Forwarded|X-Real-IP' internal/`.
- [ ] When `remoteAddr` matches a trusted CIDR: parse the forwarded header per RFC 7239 left-to-right discipline (first untrusted hop is the original client).
- [ ] When `remoteAddr` does NOT match: ignore the header entirely; fall back to `r.RemoteAddr`.
- [ ] `Config.TrustedProxies` field wired through env + TOML + reference page.
- [ ] New unit test `internal/serverapp/trusted_proxies_test.go` covers (a) empty trusted-list ignores forwarded headers, (b) trusted CIDR honors them, (c) untrusted source IP falls back to RemoteAddr, (d) malformed CIDR fails at boot, (e) IPv4 + IPv6 CIDR mix.
- [ ] **README `## Breaking changes` section CREATED** (file currently has no such section; insert between `## Quickstart` and the next section). Note: "X-Forwarded-Proto secure-cookie behavior changed in this release — operators behind a load balancer or ingress must set `STRATA_TRUSTED_PROXIES` to the proxy's source CIDR. Default empty = forwarded headers ignored (safe for direct exposure)." Per pre-launch policy, hard cutover acceptable.
- [ ] Typecheck + `make test-race` pass.

### US-005: Admin endpoint on a separate listener
**Description:** As an SRE, I want to bind the admin / console / metrics endpoints to a separate listener (loopback or RFC1918 only) so a public S3 client cannot reach the admin API.

**Acceptance Criteria:**
- [ ] New `STRATA_ADMIN_LISTEN` env (empty default = backwards-compat single-port shape preserved per pre-launch policy).
- [ ] When set (e.g. `127.0.0.1:9001` or `:9001`), `internal/serverapp/serverapp.go::Run` builds a SECOND `http.Server` bound to `STRATA_ADMIN_LISTEN`.
- [ ] Endpoints relocated to the admin listener when split: `/admin/v1/*`, `/console/*`, `/metrics`, `/healthz`, `/readyz`. S3 surface (`/*` catch-all) stays on the primary `cfg.Listen`.
- [ ] Admin listener gets its OWN timeout block from US-001 family: `STRATA_ADMIN_HTTP_READ_HEADER_TIMEOUT`, `STRATA_ADMIN_HTTP_READ_TIMEOUT`, `STRATA_ADMIN_HTTP_WRITE_TIMEOUT`, `STRATA_ADMIN_HTTP_IDLE_TIMEOUT`, `STRATA_ADMIN_HTTP_MAX_HEADER_BYTES`. Defaults match US-001 except `WriteTimeout=2m` (no large multipart on admin).
- [ ] Optional separate TLS for admin: `STRATA_ADMIN_TLS_CERT_FILE`, `STRATA_ADMIN_TLS_KEY_FILE`, `STRATA_ADMIN_TLS_CLIENT_CA_FILE`. When unset on the admin listener, admin runs plain HTTP (typical loopback shape).
- [ ] When `STRATA_ADMIN_TLS_CLIENT_CA_FILE` set, admin listener requires mTLS for every connection (operator-issued client certs for the SRE team).
- [ ] Both listeners share the same OTel tracer + audit middleware + structured logger + access-log shape — middleware instances themselves shared (NOT re-wrapped); `mux` per listener but `Handler` chain identical.
- [ ] Graceful shutdown drains BOTH listeners on SIGTERM within `cfg.ShutdownWait` via `sync.WaitGroup`.
- [ ] All envs wired through `Config.AdminListen.*` substruct + envMap + TOML example + reference page.
- [ ] New integration test `internal/serverapp/admin_listener_test.go` verifies (a) S3 request to admin listener → 404, (b) `/admin/v1/clusters` to main listener → 404, (c) both routes work when `STRATA_ADMIN_LISTEN` unset (backwards-compat), (d) admin listener bound to 127.0.0.1 rejects non-loopback connection, (e) SIGTERM drains both listeners within timeout.
- [ ] Smoke script update: `scripts/smoke-tikv-default-lab.sh` keeps current single-port shape green; new `scripts/smoke-admin-split.sh` exercises the split shape.
- [ ] `/operate/tls-termination.md` recommends `STRATA_ADMIN_LISTEN=127.0.0.1:9001` for prod deploys (rationale: defense-in-depth, even if NetworkPolicy already restricts).
- [ ] Typecheck + `make test-race` pass.

### US-006: Per-IP + per-access-key ingress rate limiter
**Description:** As an operator, I want to rate-limit requests per source IP and per access key so a runaway client cannot exhaust gateway resources.

**Acceptance Criteria:**
- [ ] New `internal/serverapp/rate_limit.go` package with `Limiter` type wrapping `golang.org/x/time/rate.Limiter` per (key|IP) entry, backed by a fixed-size LRU via `github.com/hashicorp/golang-lru/v2` (Apache 2.0; ~300 LOC vetted dep).
- [ ] **`go.mod` direct require added** for both `golang.org/x/time/rate` (currently transitive only) and `github.com/hashicorp/golang-lru/v2`. `go mod tidy` clean.
- [ ] Per-access-key limiter keyed on `auth.FromContext(ctx).AccessKey`; empty (anonymous mode) skips this limiter.
- [ ] Per-IP limiter keyed on `r.RemoteAddr` (when no trusted proxy) OR resolved client IP from US-004 (when trusted-proxy CIDR matches).
- [ ] `STRATA_RATE_LIMIT_PER_KEY` env (req/sec, default 0 = disabled, range [0, 100000]).
- [ ] `STRATA_RATE_LIMIT_PER_IP` env (req/sec, default 0 = disabled, range [0, 100000]).
- [ ] `STRATA_RATE_LIMIT_BURST` env (token-bucket burst capacity, default `2 × limit`).
- [ ] `STRATA_RATE_LIMIT_CACHE_SIZE` env (LRU cap, default 100000, range [1000, 10000000]).
- [ ] Middleware order: rate limiter runs AFTER auth middleware (so per-key dimension has the resolved AccessKey) but BEFORE audit (so refused requests don't fill the audit log). Wire site is `serverapp.go:279` mux handler chain.
- [ ] On refusal: HTTP 429 + `<Code>SlowDown</Code><Message>Rate limit exceeded</Message>` body (AWS S3 contract) + `Retry-After: 1` header + counter `strata_ingress_rate_limit_refused_total{reason="key"|"ip"}` increments.
- [ ] LRU eviction on full: oldest entry evicted (forgets its token-bucket state — acceptable; conservatively the evicted client gets a full bucket again).
- [ ] Both limiters apply to the S3 hot path only; admin/console/metrics/healthz/readyz endpoints (whether on the main listener via single-port shape or on the admin listener) bypass rate limiting.
- [ ] All envs wired through `Config.RateLimit.*` substruct + envMap + TOML example + reference page.
- [ ] New unit test `internal/serverapp/rate_limit_test.go` verifies (a) disabled (0) is no-op, (b) per-IP fires at threshold, (c) per-key fires at threshold, (d) burst absorbs short spikes, (e) LRU eviction works at cap.
- [ ] Smoke script `scripts/smoke-rate-limit.sh` runs Strata with `STRATA_RATE_LIMIT_PER_IP=5` + `hey -c 10 -n 100`, verifies ≥80% of requests return 429.
- [ ] Typecheck + `make test-race` pass.

### US-007: Smoke + docs + ROADMAP flip Done
**Description:** As an operator, I want end-to-end validation that all six hardening features work together + new operator-facing docs so I can confidently roll Strata to prod.

**Acceptance Criteria:**
- [ ] New `scripts/smoke-harden-gateway.sh` smoke script drives ALL six features together against the TiKV-default lab:
  - Boot Strata with HTTPS + admin-split + trusted-proxies + rate limit + mTLS to TiKV.
  - Probe HTTPS endpoint via `curl --cacert`.
  - Probe slowloris-blocked via the Go test binary from US-001 chaos test (run via `go test -run TestSlowloris ./internal/serverapp/`; NOT via `nc` shell — BusyBox `nc` semantics differ across distros).
  - Probe rate-limit fires via `hey -c 10 -n 100` → assert HTTP 429 + `<Code>SlowDown</Code>` + `Retry-After: 1`.
  - Probe admin listener bound to loopback only via `docker exec` from a peer container → assert connection refused on the admin port from non-loopback.
  - Probe X-Forwarded-Proto trust honor → spoof from untrusted IP (expected: ignored) + from trusted CIDR (expected: honored).
  - Probe backend mTLS via admin `/admin/v1/storage/meta` returns healthy meta backend.
- [ ] New doc page `/operate/tls-termination.md` (4 deploy shapes described above + cert provisioning recipes for cert-manager + Vault PKI + openssl + per-backend mTLS section + RADOS cephx note from US-003c).
- [ ] New doc page `/best-practices/production-hardening.md` (12-line checklist; each line links to runbook section).
- [ ] `make docs-build` green (no broken refs; mermaid renders for the deploy-shape diagrams).
- [ ] `/reference/env-vars.md` shows all new STRATA_* rows (~18 new entries from US-001 through US-006) with TOML-key column populated.
- [ ] `deploy/strata.toml.example` carries all new sections (`[http]`, `[tls]`, `[admin_listen]`, `[admin_tls]`, `[admin_http]`, `[rate_limit]`, `[trusted_proxies]`, `[meta.cassandra.tls]`, `[meta.tikv.tls]`, `[data.s3.tls]`).
- [ ] Drift-lint test `internal/config/env_toml_parity_test.go` still green (all new envs accounted for).
- [ ] **ROADMAP entry FLIPPED to Done** with closing-SHA backfill, matching the standard 12-cycle close protocol:
  ```
  ~~**P0 — Cycle A: harden-gateway (HTTP timeouts + built-in TLS + backend mTLS + trusted proxies + admin listener split + ingress rate limit).**~~ — **Done.** Shipped via the `ralph/harden-gateway` cycle (US-001..US-007). Closes 6 P0 gaps from the 2026-05-25 prod-readiness audit. ~18 new STRATA_* env vars wired through Config + TOML + reference page. Two new operator docs: [/operate/tls-termination](docs/site/content/operate/tls-termination.md) + [/best-practices/production-hardening](docs/site/content/best-practices/production-hardening.md). Smoke harness `make smoke-harden-gateway` exercises every feature end-to-end against the TiKV-default lab. README "Breaking changes" section created (X-Forwarded-Proto trust behavior change). (commit `<SHA>`)
  ```
- [ ] ALL existing smokes still pass: `make smoke`, `scripts/smoke-signed.sh`, `scripts/smoke-tikv-default-lab.sh`, every `scripts/smoke-*.sh` shipped.
- [ ] All existing CI jobs still green (web-build, lint-build, unit, integration-cassandra, docker-build, e2e, e2e-full, e2e-ui, ci-tikv, ci-scylla).
- [ ] Delete `tasks/prd-harden-gateway.md` in the closing commit per memory rule [Pre-launch no deploys] (PRD lifecycle — markdown is disposable; Ralph snapshot under `scripts/ralph/archive/` becomes canonical).
- [ ] Typecheck + `make test-race` + `make docs-build` pass.

## Functional Requirements

### Timeouts (US-001)
- FR-1: `internal/serverapp/serverapp.go::Run` must set 5 timeout fields on every constructed `http.Server` (primary listener + admin listener from US-005).
- FR-2: Defaults: `ReadHeaderTimeout=10s`, `ReadTimeout=60s`, `WriteTimeout=30m`, `IdleTimeout=120s`, `MaxHeaderBytes=1<<20`.
- FR-3: Each value overridable via env (US-001 env list). Negative reject; 0 accepted = disabled per Go semantic.
- FR-4: Slowloris chaos test (Go-native, not shell) verifies timeout actually drops slow client.

### TLS listener (US-002a + US-002b)
- FR-5: When `STRATA_TLS_CERT_FILE` + `STRATA_TLS_KEY_FILE` set, gateway listens via TLS.
- FR-6: When `STRATA_TLS_CERT_DIR` set (mutually exclusive with single-cert envs), SNI dispatcher walks the directory at boot.
- FR-7: Cert store backed by `atomic.Pointer[map[string]*tls.Certificate]` for lock-free read hot-path.
- FR-8: Cert hot-reload via fsnotify + periodic reconciliation (60s default); k8s ConfigMap / Secret atomic-symlink-swap supported via parent-directory watch.
- FR-9: `STRATA_TLS_MIN_VERSION` accepts `TLS1.2` (default) | `TLS1.3` only.
- FR-10: `STRATA_TLS_CIPHER_PROFILE` enum: `mozilla-modern` (default) | `mozilla-intermediate` | `go-default`. Free-form override REJECTED to prevent weak-cipher selection.
- FR-11: `STRATA_TLS_CLIENT_CA_FILE` enables `ClientAuth = RequireAndVerifyClientCert` when set.

### Backend mTLS (US-003a, US-003b, US-003c)
- FR-12: Cassandra accepts `STRATA_CASSANDRA_TLS_{CA,CERT,KEY}_FILE` + `STRATA_CASSANDRA_TLS_SKIP_VERIFY`; wires `gocql.SslOptions`.
- FR-13: TiKV accepts `STRATA_TIKV_TLS_{CA,CERT,KEY}_FILE` + `STRATA_TIKV_TLS_SKIP_VERIFY`; wires `config.Security`.
- FR-14: S3-upstream accepts global `STRATA_S3_TLS_*` envs + per-cluster `tls: {...}` override in `STRATA_S3_CLUSTERS` JSON. Per-cluster wins, global as fallback (no merge).
- FR-15: `STRATA_*_TLS_SKIP_VERIFY=true` logs WARN at boot + bumps gauge `strata_backend_tls_skip_verify{backend}`.
- FR-16: RADOS path documented as cephx-native; no code change for RADOS mTLS.

### Trusted proxies (US-004)
- FR-17: `STRATA_TRUSTED_PROXIES` parses CIDR list at boot; invalid CIDR fails fast with naming error.
- FR-18: Default empty → all forwarded headers (`X-Forwarded-Proto` / `X-Forwarded-For` / `X-Forwarded-Host` / `X-Real-IP`) ignored.
- FR-19: When `r.RemoteAddr` matches a trusted CIDR, forwarded headers parsed per RFC 7239 left-to-right discipline.
- FR-20: README `## Breaking changes` section created (does not exist today) documenting the trust-behavior change.

### Admin listener (US-005)
- FR-21: `STRATA_ADMIN_LISTEN` empty default preserves single-port shape; non-empty value spawns second `http.Server`.
- FR-22: When set, `/admin/v1/*`, `/console/*`, `/metrics`, `/healthz`, `/readyz` relocate to admin listener.
- FR-23: Admin listener accepts independent timeout envs (`STRATA_ADMIN_HTTP_*`) and TLS envs (`STRATA_ADMIN_TLS_*`).
- FR-24: Graceful shutdown drains both listeners within `cfg.ShutdownWait` via `sync.WaitGroup`.

### Rate limiter (US-006)
- FR-25: `STRATA_RATE_LIMIT_PER_KEY` + `STRATA_RATE_LIMIT_PER_IP` default 0 (disabled).
- FR-26: When > 0, refusals return HTTP 429 + AWS-shape `<Code>SlowDown</Code>` body + `Retry-After: 1`.
- FR-27: Backed by LRU-bounded `map[string]*rate.Limiter` (cap configurable via `STRATA_RATE_LIMIT_CACHE_SIZE`, default 100k).
- FR-28: Counter `strata_ingress_rate_limit_refused_total{reason="key"|"ip"}` bumps per refusal.
- FR-29: Admin / console / metrics / healthz / readyz endpoints bypass rate limiting (regardless of single-port vs split-listener shape).
- FR-30: `go.mod` carries direct requires for `golang.org/x/time/rate` + `github.com/hashicorp/golang-lru/v2` post-cycle; `go mod tidy` clean.

### Smoke + docs (US-007)
- FR-31: `scripts/smoke-harden-gateway.sh` exercises every hardening feature end-to-end against the TiKV-default lab.
- FR-32: Two new operator doc pages shipped (`tls-termination.md` + `production-hardening.md`).
- FR-33: ROADMAP entry created in US-001 prep commit (open state) and flipped Done in US-007 closing commit with SHA backfill.

## Non-Goals (Out of Scope)

- **Distroless container image.** Migration off `quay.io/ceph/ceph:v19.2.3` requires a separate ceph-bootstrap dependency analysis spike (~2 days); tracked as Cycle A2.
- **govulncheck / dependabot / trivy in CI.** Owned by Cycle C ("supply-chain-security").
- **PodDisruptionBudget / NetworkPolicy / HPA in Helm chart.** Owned by Cycle D ("k8s-ha-completion").
- **Alert rules in `deploy/prometheus/`.** Owned by Cycle B ("prod-observability").
- **pprof endpoint.** Owned by Cycle B.
- **CHANGELOG / SECURITY.md / CONTRIBUTING.md.** Owned by Cycle H ("release-engineering").
- **SLSA / cosign image signing.** Owned by Cycle C.
- **WORM audit log sink.** Owned by Cycle J ("compliance-audit-log").
- **Policy engine expansion (StringLike, IpAddress, NumericLessThan, …).** Owned by Cycle E ("policy-engine-v2").
- **Multi-region active-active.** Out of scope; remains a P2 ROADMAP item.
- **Configuration hot-reload beyond TLS certs.** Other envs still require restart; TLS-only hot-reload because cert rotation is the operationally-frequent case.
- **Admin endpoint rate limiting.** Loopback bind = defense-in-depth; admin rate limit deferred to Cycle B if observability metrics show need.
- **Free-form cipher suite override.** Replaced by `STRATA_TLS_CIPHER_PROFILE` enum to prevent weak-cipher downgrade vector.

## Design Considerations

- **Opt-in default everywhere.** Every new env is empty / zero by default; existing labs and smoke scripts work unchanged.
- **Env naming consistency.** `STRATA_<DOMAIN>_<KNOB>` pattern; each maps to a `Config.<Domain>.<Knob>` field per ralph/toml-parity contract.
- **Breaking change explicit.** US-004 changes default forwarded-header semantics from "blind trust" to "ignored unless trusted CIDR matches". README `## Breaking changes` section created (does not exist today); pre-launch policy allows hard cutover.
- **Admin listener backwards-compat.** Empty `STRATA_ADMIN_LISTEN` preserves current single-port shape — no existing CI / lab needs a config change.
- **Smoke parity.** Every existing smoke script must still pass without changes. New smoke scripts (`smoke-tls.sh`, `smoke-admin-split.sh`, `smoke-rate-limit.sh`, `smoke-harden-gateway.sh`) added as additive coverage.
- **Doc shape.** Two new pages live under `/operate/` and `/best-practices/` — slot into the existing CockroachDB-shape sectioned tree from ralph/readme-docs-rewrite.
- **Cipher profile enum, NOT free-form.** Free-form `STRATA_TLS_CIPHER_SUITES` would allow operators to accidentally select weak ciphers (Go's `tls` package only fail-fasts on unknown names, not on weak ones). Enum profile locks them into known-safe sets.
- **Cert reload via atomic.Pointer, not RWMutex.** `tls.Config.GetCertificate` callback fires per-handshake under high concurrency; RWMutex contention measurable. Atomic pointer swap is lock-free read.

## Technical Considerations

- **fsnotify v1.9.0 already in `go.sum`** (transitive via koanf). No new top-level require needed.
- **`github.com/hashicorp/golang-lru/v2`** new dep (Apache 2.0, ~300 LOC, widely vetted). Add to `go.mod` direct requires.
- **`golang.org/x/time/rate`** today transitive only (via `internal/rebalance/throttle.go`); promote to direct require after US-006 moves limiter into `internal/serverapp/`.
- **gocql SslOptions** native struct; no new dep.
- **tikv-client-go config.Security** native struct; no new dep.
- **aws-sdk-go-v2 TLS transport** native via `aws.Config.HTTPClient` swap; no new dep.
- **k8s ConfigMap / Secret mount semantics**: kubelet uses atomic symlink swap (`..data` symlink → versioned subdirectory). fsnotify watching the cert file itself misses MODIFY; must watch parent directory for RENAME / CREATE events on the symlink. Periodic reconciliation (60s) is the fallback when fsnotify drops events under load.
- **Pre-launch hard cutover.** Per memory rule [Pre-launch no deploys], breaking-change envs accepted; document in README + close PRD.
- **Drift-lint test.** `internal/config/env_toml_parity_test.go` from ralph/toml-parity hard-fails CI on any new STRATA_* env missing Config wiring. Every new env in this cycle must pass.
- **HTTP/2 with TLS.** Go's `http.Server` auto-negotiates HTTP/2 when serving TLS; verify in US-002a smoke that `curl --http2` succeeds.
- **Listener interleaving.** Two listeners run as two goroutines sharing the same middleware-wrapped `http.Handler` (NOT re-wrapped per listener — single instance, two muxes); graceful shutdown coordinates via `sync.WaitGroup`.
- **OTel + audit middleware on admin listener.** Re-use the existing wrap stack from the main listener; don't fork the middleware chain.
- **Rate limiter middleware order matters.** AFTER auth (so AccessKey available), BEFORE audit (so 429 doesn't pollute audit log). Document in `internal/serverapp/serverapp.go` comment block.
- **WriteTimeout 30m default vs slow networks.** 5 GiB ÷ 30m ≈ 2.8 MB/s minimum sustained throughput — well below cellular floor. Operators on slower paths set 0 (disabled) at deployment time.

## Success Metrics

- All 10 user stories complete (passes=true in `scripts/ralph/prd.json`).
- ROADMAP `Cycle A: harden-gateway` entry created in US-001 + flipped Done in US-007 with SHA backfill.
- `make smoke-harden-gateway` green on the TiKV-default lab.
- All existing smokes + CI jobs still green (no regression).
- `make docs-build` green; 2 new doc pages render.
- `/reference/env-vars.md` carries ~18 new STRATA_* rows with TOML-key column populated.
- `internal/config/env_toml_parity_test.go` green.
- `go mod tidy` clean post-cycle; both `golang.org/x/time/rate` + `github.com/hashicorp/golang-lru/v2` direct requires.
- An operator following `/operate/tls-termination.md` can roll a hardened production deployment from zero in ≤30 minutes.

## Open Questions

- **Cipher suite default for `STRATA_TLS_MIN_VERSION=TLS1.3`.** TLS 1.3 cipher suites are not user-configurable in Go's `tls` package — `STRATA_TLS_CIPHER_PROFILE` only applies to TLS 1.2. Documented in US-002a AC + `/operate/tls-termination.md`.
- **Rate-limit cache LRU vs window-based.** Current design = LRU eviction. Alternative = time-window TTL eviction (entries expire after 5 min idle). LRU simpler; revisit in Cycle B if eviction churn becomes a metric concern.
- **Admin listener access without auth.** When admin runs on `127.0.0.1:9001` plain HTTP, is the existing `auth.MultiStore`-backed admin login still required? Yes — keep current auth chain; loopback binding is defense-in-depth, not authn replacement.
- **k8s symlink reconciliation interval.** 60s default is a guess; if operators report missed reload events, drop to 30s. `STRATA_TLS_RELOAD_INTERVAL` env exposes the knob.
