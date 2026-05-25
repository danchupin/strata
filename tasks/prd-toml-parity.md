# PRD: Full env+TOML config parity

## Introduction

Final tactical cycle to complete user vision "leave only globals in
ROADMAP". P2 entry (line 385) closes; post-cycle ROADMAP holds 4
globals + 1 P3 latent only.

`internal/config/config.go` already uses koanf with `toml` + `env` +
`structs` providers; `STRATA_CONFIG_FILE` points at a TOML file;
`deploy/strata.toml.example` lives in-tree. **Gap**: many env knobs
added across last ~10 cycles ship env-only — not wired through
`Config struct`, not documented in `strata.toml.example`, not loadable
via TOML.

Reference page `docs/site/content/reference/env-vars.md` lists 122
STRATA_* vars; only ~30-40 (verify via audit script) have TOML parity
today. Examples of env-only knobs: `STRATA_GC_*`, `STRATA_REBALANCE_*`,
`STRATA_USAGE_ROLLUP_*`, `STRATA_KMS_*`, `STRATA_DEK_CACHE_TTL`,
`STRATA_KEY_MAX_AGE`, `STRATA_BUCKET_STATS_SHARDS`,
`STRATA_MANIFEST_FORMAT`, `STRATA_OTEL_*`, `STRATA_AUDIT_RETENTION`.

This cycle audits the gap, wires every missing knob through `Config
struct`, refreshes `strata.toml.example`, adds a drift-proof lint test
that hard-fails CI on missing TOML wire, and updates the reference
page with a `TOML key` column per row.

**Pre-launch product** per [Pre-launch no deploys] memory — hard
cutover OK if any knob renamed (no operator config migration).

Branch: `ralph/toml-parity`. Starts from `main`. **7 stories.**

## Goals

- Every `STRATA_*` env var readable from main module reachable via
  TOML key with identical semantics (env still overrides via koanf
  precedence).
- Single `Config struct` is source of truth — workers / auth / KMS /
  observability all consume `cfg.<Section>.<Knob>` instead of
  `os.Getenv("STRATA_*")`.
- `deploy/strata.toml.example` documents every field with one-line
  comment + commented-out default (`# STRATA_X = "default"` shape).
- Drift-proof `internal/config/env_toml_parity_test.go` hard-fails
  CI on any new `STRATA_*` env read added without TOML wiring (or
  added to the exemption list).
- Reference page `docs/site/content/reference/env-vars.md` gains
  `TOML key` column across all 122 rows.
- Knob validation symmetric — TOML loads through same clamp/range
  checks as env reads (`koanf.UnmarshalConf` returns the same
  errors).

## User Journey

Three personas:

- **Operator deploying via Helm chart with `values.yaml`.** Today:
  Helm chart renders ConfigMap with selected env vars, but most
  tuning knobs aren't covered. After cycle: full TOML rendered into
  ConfigMap, mounted at `STRATA_CONFIG_FILE`, every knob settable
  without env-var explosion in pod spec.
- **Operator running `strata server` from CLI with TOML.** Today:
  half the knobs need env vars in addition to TOML — confusing dual
  surface. After cycle: TOML covers 100% of operator-tunable knobs;
  env vars are override-only (CI overrides, ad-hoc debug).
- **Contributor adding a new `STRATA_*` env var in a future cycle.**
  Today: easy to forget TOML wiring; debt accumulates. After cycle:
  drift lint hard-fails the PR until TOML field + struct tag +
  `strata.toml.example` row + reference page row land together.

## User Stories

### US-001: Audit script + gap-list

**Description:** As a contributor, I want an audit script that
generates the gap-list jsonl programmatically so subsequent stories
work from a deterministic contract + the same script powers the
drift-lint in US-006.

**Acceptance Criteria:**
- [ ] New script `scripts/audit-env-toml-parity.sh`:
      (a) Greps STRATA_* env reads in main module:
          `grep -rhEo 'STRATA_[A-Z_][A-Z0-9_]+' cmd/ internal/ | sort -u`.
      (b) **Parses `envMap` central registry** in
          `internal/config/config.go` (verified to exist —
          ~13 entries today as `"STRATA_X": "toml.key"` pairs at
          module level). `envMap` is THE central env→TOML mapping
          ledger; every wired env var MUST be in it.
      (c) Parses `Config struct` + sub-structs (`Config`,
          `CassandraConfig`, `TiKVConfig`, `RADOSConfig`,
          `AuthConfig`, `LifecycleConfig`, `GCConfig` exist today;
          this cycle adds `WorkersConfig` / `KMSConfig` / `OTelConfig`
          / `LoggingConfig` / `RebalanceConfig` / `UsageRollupConfig`
          / `ManifestRewriterConfig` / `AuditExportConfig` /
          `QuotaReconcileConfig`) koanf tags via reflect or AST,
          emits `(field_path, koanf_tag, type)` jsonl.
      (d) Reads `deploy/strata.toml.example` — parses via
          `github.com/pelletier/go-toml` (or koanf TOML parser) +
          emits set of TOML keys present.
      (e) Joins four streams: env-var → envMap entry (or missing)
          → expected-TOML-key (per naming convention from option
          2A) → struct-field → example-key. Emits
          `scripts/audit-results/env-toml-parity-<date>.jsonl`
          with rows: `{env_var, in_env_map: bool,
          expected_toml_key, struct_field, present_in_struct: bool,
          present_in_example: bool}`.
      (f) Prints summary table: total env vars (expected 122 +
          additions) / in envMap / mapped to struct / unmapped /
          example-coverage-pct.
- [ ] **TOML key naming convention** (option 2A): `STRATA_<SECTION>_<KNOB>`
      → `<section>.<knob>` (lowercase). One underscore splits
      section; remaining underscores stay snake_case inside knob.
      Examples: `STRATA_GC_INTERVAL` → `gc.interval`;
      `STRATA_RADOS_HEALTH_OID` → `rados.health_oid`;
      `STRATA_USAGE_ROLLUP_SAMPLES_PER_DAY` →
      `usage_rollup.samples_per_day`.
- [ ] **Hard-coded exemption list** (option 3A) in
      `internal/config/exempt_env_vars.go`: `STRATA_CONFIG_FILE`
      (bootstrap — points AT the TOML file, can't itself be IN it),
      `STRATA_BENCH_*` (test/bench-only), `STRATA_REGEN_TRAILER_FIXTURES`
      (test-only). Audit script + drift lint consume the list.
- [ ] **Output gap-list** committed: `tasks/toml-parity-gaps.md`
      generated by piping audit jsonl through a sed/awk pipeline
      into a markdown table. Sorted by section. Reviewed by US-001
      author for false positives — flagged exemptions added to the
      list above.
- [ ] No code changes outside `scripts/`, new audit helper, exempt
      list. Audit script becomes the contract for US-002..US-005.
- [ ] `make audit-toml-parity` Makefile target wraps the script.
- [ ] `go vet ./...` passes; tests pass (no new tests required —
      contract test added in US-006).
- [ ] Typecheck passes; tests pass.

### US-002: Wire workers env knobs through Config struct

**Description:** As an operator, I want every worker knob
(GC / lifecycle / rebalance / usage-rollup / manifest-rewriter /
audit-export / quota-reconcile) settable via TOML.

**Acceptance Criteria:**
- [ ] **Workers section in Config struct**:
      `Config.Workers.<Worker>` substruct per worker. Each substruct
      carries fields per knob from audit-gap-list — example:
      `Workers.GC.Interval duration` (TOML key `workers.gc.interval`),
      `Workers.GC.Grace duration`, `Workers.GC.BatchSize int`,
      `Workers.GC.Concurrency int`, `Workers.GC.Shards int`.
      Similar for rebalance / lifecycle / usage_rollup /
      manifest_rewriter / audit_export / quota_reconcile.
- [ ] **koanf struct tags** on every new field
      (`koanf:"interval"`, etc.) so YAML/TOML/env precedence works.
- [ ] **Worker Build sites refactored** to consume Config field —
      `cmd/strata/workers/gc.go`, `rebalance.go`, `lifecycle.go`,
      `usage_rollup.go` (verified `os.Getenv` reads here), etc.
      Replace `os.Getenv("STRATA_GC_INTERVAL")` with
      `deps.Cfg.Workers.GC.Interval`.
- [ ] **`envMap` registry updated** in `internal/config/config.go`
      with new entries per knob (e.g. `"STRATA_GC_BATCH_SIZE":
      "gc.batch_size"`). Every refactored env read MUST have a
      corresponding envMap entry.
- [ ] **Backward-compat**: existing env vars STILL work (koanf env
      provider has higher precedence than TOML by default — operator
      can override TOML defaults via env on CI / debug). Audit
      script verifies env override path post-refactor.
- [ ] **Smoke**: `make run-memory` boots cleanly with **TOML-only**
      config (`STRATA_CONFIG_FILE=/tmp/test.toml` pointing at a
      file with all worker fields populated; zero `STRATA_*` env
      vars set). Workers come up with the TOML values.
- [ ] **Range validation** — TOML loads through same clamp checks
      as env reads. E.g. `STRATA_GC_SHARDS` clamps to [1, 1024];
      TOML value of 5000 gets the same clamp + WARN log line.
- [ ] Contract test added: `internal/config/workers_test.go`
      verifies each worker substruct round-trips TOML → struct →
      back to expected values (covers all knobs from US-001 audit).
- [ ] `go vet ./...` + `go test -race ./internal/config/...
      ./cmd/strata/workers/...` pass.
- [ ] Typecheck passes; tests pass.

### US-003: Wire auth + KMS + crypto env knobs

**Description:** As an operator, I want auth + KMS knobs settable
via TOML so the per-bucket signing key config (from
ralph/auth-dx-trailer-lima) lives in one place.

**Acceptance Criteria:**
- [ ] **Auth + KMS sections in Config struct**:
      `Config.Auth.Mode string` (TOML `auth.mode`),
      `Config.Auth.StaticCredentials string` (`auth.static_credentials`),
      `Config.Auth.STSDuration duration` (`auth.sts_duration`),
      `Config.Auth.KeyMaxAge duration` (`auth.key_max_age` —
      default `90d` from ralph/auth-dx-trailer-lima US-002).
      `Config.KMS.Adapter string` (`kms.adapter` — selects
      vault/aws/local_hsm per existing `FromEnv` precedence),
      `Config.KMS.DEKCacheTTL duration` (`kms.dek_cache_ttl`,
      default 5m from ralph/auth-dx-trailer-lima US-001),
      `Config.KMS.AWS.Region string`, `Config.KMS.AWS.Endpoint string`,
      `Config.KMS.AWS.RoleARN string`,
      `Config.KMS.Vault.Address string`, `Config.KMS.Vault.Token
      string`, `Config.KMS.Vault.RoleID string`,
      `Config.KMS.Vault.SecretID string`, `Config.KMS.Vault.Mount
      string`.
- [ ] **Existing `internal/crypto/kms/FromEnv` refactor**: accept
      `Config.KMS` substruct instead of reading
      `os.Getenv("STRATA_KMS_*")` directly. Keep env override path
      via koanf precedence.
- [ ] **`envMap` registry updated** with all auth + KMS entries
      (e.g. `"STRATA_KMS_ADAPTER": "kms.adapter"`,
      `"STRATA_DEK_CACHE_TTL": "kms.dek_cache_ttl"`).
- [ ] **`internal/serverapp/bucket_signing.go`** (verified
      `os.Getenv` reads here) — refactor to consume `cfg.KMS` +
      `cfg.Auth`.
- [ ] **Auth middleware refactor** in `internal/auth/`: consume
      `cfg.Auth.KeyMaxAge` for per-bucket key expiry check;
      `cfg.KMS.DEKCacheTTL` for DEK cache.
- [ ] **Range validation** — TOML loads clamp to same ranges as
      env (`auth.key_max_age` ∈ [1d, 365d];
      `kms.dek_cache_ttl` ∈ [30s, 1h]).
- [ ] **Smoke**: `make run-memory` with TOML-only KMS config (vault
      adapter + address) boots cleanly; per-bucket signing key flow
      works end-to-end against in-process LocalHSMProvider stub.
- [ ] Contract test in `internal/config/auth_kms_test.go`.
- [ ] `go vet ./...` + `go test -race ./internal/config/...
      ./internal/auth/... ./internal/crypto/kms/...` pass.
- [ ] Typecheck passes; tests pass.

### US-004: Wire observability env knobs

**Description:** As an operator running with Prometheus + OTel + audit
log retention, I want every observability knob settable via TOML.

**Acceptance Criteria:**
- [ ] **Observability section in Config struct**:
      `Config.OTel.ExporterEndpoint string`
      (`otel.exporter_endpoint` — already env via
      `OTEL_EXPORTER_OTLP_ENDPOINT`; verify TOML key shape since
      env doesn't carry STRATA_ prefix — keep `otel.exporter_endpoint`
      as TOML, env override via `STRATA_OTEL_EXPORTER_ENDPOINT`
      OR continue accepting `OTEL_EXPORTER_OTLP_ENDPOINT` as
      bootstrap-env per current koanf wiring).
      `Config.OTel.SampleRatio float64` (`otel.sample_ratio`),
      `Config.OTel.Ringbuf bool` (`otel.ringbuf`),
      `Config.OTel.RingbufBytes int` (`otel.ringbuf_bytes`),
      `Config.AuditLog.Retention duration` (`audit_log.retention`),
      `Config.AuditExport.After duration` (`audit_export.after`),
      `Config.Logging.Level string` (`logging.level`),
      `Config.Logging.Format string` (`logging.format` — slog
      JSON / text).
- [ ] **Existing `internal/otel/Init` refactor**: consume
      `cfg.OTel` instead of reading
      `os.Getenv("STRATA_OTEL_*")` directly.
- [ ] **Existing `internal/logging.Setup` refactor**: consume
      `cfg.Logging` instead of `os.Getenv("STRATA_LOG_LEVEL")`.
- [ ] **`envMap` registry updated** with all observability entries.
- [ ] **Range validation** — sample_ratio ∈ [0.0, 1.0];
      ringbuf_bytes ∈ [1MiB, 1GiB].
- [ ] Contract test in `internal/config/observability_test.go`.
- [ ] `go vet ./...` + `go test -race ./internal/config/...
      ./internal/otel/... ./internal/logging/...` pass.
- [ ] Typecheck passes; tests pass.

### US-005: Misc env knobs sweep

**Description:** As a maintainer, I want every remaining gap from
the US-001 audit not covered by US-002/003/004 wired in this story
so the audit script outputs 100% mapped post-cycle.

**Acceptance Criteria:**
- [ ] **Sweep covers** (from US-001 audit gap-list — verify before
      coding):
      `STRATA_BUCKET_STATS_SHARDS` → `bucket_stats.shards`,
      `STRATA_MANIFEST_FORMAT` → `manifest.format`,
      `STRATA_HEARTBEAT_INTERVAL` → `heartbeat.interval`
      (verified `os.Getenv` read at `internal/heartbeat/heartbeat.go`),
      `STRATA_RADOS_HEALTH_OID` → `rados.health_oid`,
      `STRATA_RADOS_CLUSTERS` → `rados.clusters` (**ALREADY IN
      envMap**, verified — no action),
      `STRATA_RADOS_CLASSES` → `rados.classes` (**ALREADY IN
      envMap**, verified — no action),
      `STRATA_RADOS_POOL_SIZE` → `rados.pool_size` (verified
      `os.Getenv` read at `internal/data/rados/pool_env.go`),
      `STRATA_RADOS_BATCH_OPS` → `rados.batch_ops` (verified
      `os.Getenv` read at `internal/data/rados/ops_env.go`),
      `STRATA_DRAIN_*` → `drain.*`,
      any other STRATA_* env reads the audit found.
- [ ] **Validation symmetry preserved** per knob.
- [ ] **Audit re-run** at end of story shows 100% mapped (only
      exempt list entries unmapped — `STRATA_CONFIG_FILE`,
      `STRATA_BENCH_*`, `STRATA_REGEN_TRAILER_FIXTURES`).
- [ ] Contract test in `internal/config/misc_test.go`.
- [ ] `go vet ./...` + `go test -race ./internal/config/...` pass.
- [ ] Typecheck passes; tests pass.

### US-006: strata.toml.example refresh + drift-lint test + reference page TOML column

**Description:** As a maintainer, I want the example TOML file
complete + a Go test that hard-fails the build on any future env
var added without TOML wiring + the reference page updated with
TOML keys.

**Acceptance Criteria:**
- [ ] **Refresh `deploy/strata.toml.example`** with every field
      from Config struct. Each field carries:
      - Inline comment with default value + range (if applicable):
        `# Default: 1h. Range: [1m, 24h].`
      - Commented-out `# key = value` line showing default.
      - Section grouping matches Config struct sections (workers /
        auth / kms / otel / etc.) — TOML sections in same order.
- [ ] **New test `internal/config/env_toml_parity_test.go`**
      (default build tag — runs under standard `go test`):
      (a) Runs the audit script (via `os/exec` or in-process Go
          equivalent) — reuses logic from
          `scripts/audit-env-toml-parity.sh`.
      (b) Asserts every env var (NOT in exemption list) has matching
          koanf tag in Config struct.
      (c) Asserts every Config struct field has matching commented
          line in `deploy/strata.toml.example`.
      (d) Asserts every env var (NOT exempt) has a row in
          `docs/site/content/reference/env-vars.md` with non-empty
          `TOML key` column.
- [ ] **Reference page `TOML key` column** added to
      `docs/site/content/reference/env-vars.md`:
      Markdown table gains 6th column (after `Variable | Default |
      Range | Consuming layer | Notes` from
      ralph/readme-docs-rewrite US-002). Backfill across all 122
      rows. Exempt rows show `—` (em-dash) in the TOML key column.
- [ ] **Drift lint exemption list** behavior: env vars in
      `internal/config/exempt_env_vars.go` skip the parity check;
      anything else fails the test with clear message
      `STRATA_NEW_KNOB has no Config struct tag — add field or add to
      exempt list`.
- [ ] **Hard CI gate**: test runs under `make test` default tag;
      green required for merge.
- [ ] `make docs-build` green (reference page renders with new
      column).
- [ ] `make vet` + `go test -race ./...` pass.
- [ ] Typecheck passes; tests pass.

### US-007: Smoke + ROADMAP close-flip + PRD removal

**Description:** As a future-maintainer, I want the TOML parity
cycle verified end-to-end + ROADMAP entry flipped + PRD removed.

**Acceptance Criteria:**
- [ ] Run `make smoke` against TiKV-default lab → green.
- [ ] Run `make smoke-signed` → green.
- [ ] Run `make smoke-tikv-default-lab` → 4/4 scenarios pass.
- [ ] Run full `go test -race ./...` (default tag) → green;
      capture duration in progress.txt.
- [ ] Run `make test-integration` → green.
- [ ] Run `make vet` + `make docs-build` → green.
- [ ] **TOML-only boot smoke**: `STRATA_CONFIG_FILE=/tmp/full.toml
      make run-memory` (where `/tmp/full.toml` is generated from
      `deploy/strata.toml.example` by uncommenting every line
      with default values + setting `STRATA_META_BACKEND=memory`)
      → server boots + serves `/healthz` 200 + serves
      `aws s3 ls` empty list → kill — passes proves TOML
      covers the boot path end-to-end.
- [ ] **Audit script run** confirms 100% mapped (only exempt list
      unmapped).
- [ ] **Drift lint test** `env_toml_parity_test.go` passes.
- [ ] **ROADMAP close-flip × 1** on line 385 (P2 — Full config
      coverage in `strata.toml`). Summary references US-001..US-006
      + audit script artifact + drift lint test + reference page
      TOML column. Carries `(commit pending)` placeholder; SHA
      backfill on `main` as fast-follow commit.
- [ ] `tasks/prd-toml-parity.md` REMOVED via `git rm`.
- [ ] `tasks/toml-parity-gaps.md` (US-001 artifact) REMOVED — was
      the cycle contract; cycle done, drift lint is the ongoing
      enforcement.
- [ ] `scripts/ralph/progress.txt` carries one US-007 block
      summarising smoke + audit final state + TOML-only boot
      outcome.
- [ ] Typecheck passes; tests pass.

## Functional Requirements

- FR-1: Every `STRATA_*` env var (except exempt list) MUST have a
  matching koanf-tagged field in `Config struct`.
- FR-2: TOML key naming MUST follow `STRATA_<SECTION>_<KNOB>` →
  `<section>.<knob>` (lowercase; single split on first underscore
  after STRATA_; remaining `_` stay as snake_case inside knob).
- FR-3: Knob validation (clamp, range) MUST apply symmetrically to
  TOML + env loads (single validation path in Config struct
  unmarshal hook).
- FR-4: `deploy/strata.toml.example` MUST contain a commented-out
  line + range/default doc for every Config struct field.
- FR-5: `internal/config/env_toml_parity_test.go` MUST hard-fail
  CI on any env var (non-exempt) without TOML wire.
- FR-6: `docs/site/content/reference/env-vars.md` MUST gain `TOML
  key` column across all 122 rows.
- FR-7: Audit script `scripts/audit-env-toml-parity.sh` MUST be
  re-runnable + produce deterministic jsonl output suitable for
  drift detection.
- FR-8: Exemption list MUST live in
  `internal/config/exempt_env_vars.go` (hard-coded, no per-site
  markers).
- FR-9: TOML-only boot (zero `STRATA_*` env vars set; only
  `STRATA_CONFIG_FILE`) MUST work for memory + Cassandra default
  labs.
- FR-10: ROADMAP MUST close P2 entry on line 385 in US-007
  commit; no new entries surfaced.

## Non-Goals

- No removal of env var support — env stays as override channel
  (CI / ad-hoc debug); precedence env > TOML > defaults
  preserved.
- No new env vars added in this cycle (audit + wire existing only).
- No knob renames (any rename would break operator surface; hard
  cutover only if pre-launch demands; verify per knob in US-001
  audit review).
- No per-site `// toml:skip` magic comments (option 3B from
  scoping rejected — hard-coded exemption list).
- No deep-nesting TOML key shape (option 2B rejected;
  `<section>.<knob>` keeps section count small + reads cleanly).
- No removal of `STRATA_CONFIG_FILE` (bootstrap env — points AT
  the TOML).
- No translation of `deploy/strata.toml.example` to other deploy
  formats (Helm values.yaml already covered separately;
  Kubernetes ConfigMap consumes the rendered TOML).

## Design Considerations

- **TOML naming convention**: `<section>.<knob>` keeps the
  section tree shallow (3-4 sections vs 8+ if every `_` becomes
  `.`). Operator's `vi` autocomplete works at section level.
- **Audit script is the contract**: US-001 produces the
  authoritative list of gaps; US-002..US-005 implement each gap;
  US-006 turns the script into a CI gate so future cycles can't
  introduce drift.
- **Exemption list discipline**: hard-coded in code, not in
  config — operator can't accidentally exempt a knob via TOML
  edit. Adding to exempt list requires a code commit + review.
- **Env precedence preserved**: koanf default precedence is
  env > TOML > defaults; we don't flip it. Operator's CI override
  via env keeps working.
- **No knob renames** (default): every existing `STRATA_*` name
  stays. TOML key is derived; if naming convention forces a
  rename, flag in US-001 audit + decide per knob.

## Technical Considerations

- **koanf precedence**: `internal/config/config.go` already loads
  TOML first, then env overlay. Verify the env overlay still
  works after refactor (US-002..US-005) — TestLoadEmptyEnvDoesNotOverrideTOMLValue
  from existing config_test.go pins the regression.
- **Range validation symmetry**: TOML unmarshal into typed Go
  struct → koanf provides type coercion. Apply clamp via post-
  unmarshal hook `Config.Validate()` so it runs once regardless
  of TOML vs env source.
- **Audit script output stability**: jsonl rows sorted by
  env_var name for deterministic diff across runs.
- **Test fixture size**: 122 env vars × multi-line TOML examples
  = ~400-500 line `strata.toml.example`. Operator-friendly with
  section comments; not unwieldy.
- **CI cost**: `env_toml_parity_test.go` runs under `make test`
  default tag — negligible runtime (~50ms grep + AST walk).
- **Backward-compat**: env still highest precedence — CI envs
  that set STRATA_* continue working; existing tests that
  `t.Setenv("STRATA_X", "y")` continue working without TOML
  setup.

## Success Metrics

- Audit script final run: 100% env-var coverage (only exempt
  list unmapped).
- `deploy/strata.toml.example` covers every Config field with
  commented default + range.
- `env_toml_parity_test.go` green on main; hard-fails on
  intentional regression (verify by temp-removing one field, then
  reverting).
- `docs/site/content/reference/env-vars.md` shows TOML key
  column populated for 122 rows.
- TOML-only boot works for memory + Cassandra labs.
- 1 ROADMAP P2 entry closes in this cycle; no new entries.
- Cycle ships in 7 stories.
- **Post-cycle ROADMAP holds 5 entries**: 4 globals + 1 P3 latent
  (smoke-tikv burst-count). User vision "leave only globals"
  effectively complete (1 latent is operator-surfaced
  test-script tweak, not feature work).

## Open Questions

- Helm chart `values.yaml` ↔ TOML sync — does the Helm chart from
  ralph/dx-lab US-003 render a ConfigMap with the new TOML? If
  yes, refresh values.yaml in US-005 sweep; if no (Helm chart
  still env-only), park as follow-up. Resolve in US-001 audit by
  diffing values.yaml against new Config struct.
- Knob removal vs rename: any knob that currently has misleading
  name (e.g. `STRATA_GC_SHARDS` actually means fan-out parallelism
  not data sharding) — keep name or rename? Default: keep (pre-
  launch but operator-facing names already in docs); revisit per
  knob in US-001 review if naming clearly wrong.
- `STRATA_LOG_FORMAT` — currently in code? Or only `STRATA_LOG_LEVEL`?
  Audit reveals — if not in code, parked as future P3 knob, not
  this cycle.
