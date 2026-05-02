# internal/crypto/master

Master-key sourcing for SSE-S3. Each provider yields `(key32, keyID)` and is
selected by `FromEnv()` based on which env var is set.

## Provider precedence (highest first)

1. `STRATA_SSE_MASTER_KEYS=<id>:<hex64>[,<id>:<hex64>...]` → `RotationProvider` (active = first)
2. `STRATA_SSE_MASTER_KEY_VAULT=<addr>:<transit-export-path>` → `VaultProvider`
3. `STRATA_SSE_MASTER_KEY_FILE=<path>` → `FileProvider` (mtime hot-reload)
4. `STRATA_SSE_MASTER_KEY=<hex64>` → `EnvProvider`

Tests that call `FromEnv()` MUST `t.Setenv` ALL precedence vars (including
`EnvMasterKeys`) to a known value, otherwise an ambient process env on a dev
machine flips precedence and tests pass/fail intermittently.

## Provider vs Resolver

- `Provider.Resolve(ctx)` returns the **active** key + id (used for wrap).
- `Resolver.ResolveByID(ctx, keyID)` looks up a specific historical key
  (used for unwrap when `objects.sse_key_id` may differ from the active id).
- `RotationProvider` implements both. `EnvProvider` / `FileProvider` /
  `VaultProvider` only implement `Provider`.
- Server code uses the `master.ResolveByID(ctx, p, keyID)` helper, which
  type-asserts to `Resolver` and falls back to `Resolve` (matching id, or empty
  id for legacy rows) for single-key providers. **Always call this helper for
  unwrap paths**; never call `Resolve` directly when you already know the id
  the row was wrapped under.

## Adding a new provider

1. Implement `Provider` (and optionally `Resolver` if you support multiple
   key versions).
2. Add an env var const + `New<Foo>FromEnv()` that returns `ErrNoConfig` if
   unset.
3. Wire into `FromEnv()` precedence chain.
4. Add tests that `t.Setenv` ALL precedence vars to "" first.
5. If your provider has TTL/refresh logic, inject a `Now func() time.Time`
   for deterministic tests (see `VaultConfig.Now`).
6. If your provider can fail non-fatally (e.g. refresh failure with cached
   fallback), inject a `*slog.Logger` via the config struct so tests can
   capture WARN logs.

## Master key rotation flow (US-006)

- Operators set `STRATA_SSE_MASTER_KEYS=newID:newHex,oldID:oldHex,...` —
  newID becomes the wrap key for new PUTs; old ids stay unwrap-only so
  pre-rotation objects keep working.
- `cmd/strata-rewrap` walks every bucket and rewraps DEKs from old keys to
  the active key (idempotent on already-current rows). Per-bucket completion
  is recorded in `meta.RewrapProgress` so re-runs skip done buckets unless
  the active id changed.
- Rewrap is **layered above the data backend** — only the wrapped DEK on
  `objects.sse_key` changes. Chunk ciphertext on disk is untouched (the DEK
  itself is unchanged; only its wrap key rotates).
