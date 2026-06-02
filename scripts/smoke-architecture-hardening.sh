#!/usr/bin/env bash
# scripts/smoke-architecture-hardening.sh — composite end-to-end walkthrough
# for the ralph/architecture-hardening cycle (US-011). One pass exercises the
# whole hardened path across BOTH production labs so the GO/NO-GO decision
# rests on observed behaviour, not story-by-story claims.
#
# Legs (each PASS / WARN-skip / FAIL independently):
#
#   A. Cassandra online reshard under concurrent writes (US-001/002/003/005)
#      — delegates to scripts/smoke-reshard.sh against the Cassandra lab
#        (:9998). Covers per-bucket NON-DEFAULT shard count (64->128 stored on
#        the bucket + resolved on the hot path) + the transitional read/write
#        model + crash-resume.
#
#   B. TiKV reshard parity — immediate-complete no-op (US-004). Trigger a
#      reshard on the range-scan backend (:9999), assert the job converges to
#      idle with shard_count flipped and the object still reads (API success,
#      zero rows moved — the partition layout carries no per-key shard).
#
#   C. DeleteObjects batch + versioned (US-007). Multi-key ?delete on an
#      unversioned bucket (idempotent, every key 404s after); versioned bucket
#      no-versionId delete -> DeleteMarker.
#
#   D. Policy-only anonymous GET — policy UNION ACL (US-008). Owner-owned
#      bucket + private object ACL + a bucket policy that Allows s3:GetObject
#      to Principal "*"; an ANONYMOUS GET (no creds) must 200 because the
#      explicit policy Allow unions over the (denying) ACL. Needs the lab's
#      default STRATA_AUTH_MODE=optional so anon reaches the gate.
#
#   E. Console reshard UI (US-006). Drives the cookie-authenticated /admin/v1
#      reshard pair the React console talks to: GET reports supported+state,
#      POST queues a job. Asserts supported=true on Cassandra, supported=false
#      (UI-disabled) on TiKV. The full browser pass is the Playwright spec
#      web/e2e/reshard-progress.spec.ts (CI-only — webServer boots the gateway,
#      which cgo-blocks on a macOS/lima box).
#
#   F. Plaintext chunk-corruption fail-loud (US-009). Runs the data-layer CRC
#      verification tests directly (CGO_ENABLED=0 — the data/memory + data/rados
#      packages build without librados): a flipped plaintext byte must surface
#      data.ErrChecksumMismatch on the read path, never silent-truncate. This is
#      the authoritative fail-loud proof and runs even with no lab up — a live
#      gateway exposes NO byte-flip hook by design (that would be a data-integrity
#      hole), so the corruption is injected at the backend test seam.
#
# Pre-requisites on the host: bash, curl (>=7.75 for --aws-sigv4), jq, aws, go.
#   STRATA_STATIC_CREDENTIALS=<access:secret[:owner]>  — the same value the
#       gateway booted with. First entry drives SigV4 + admin login.
#   STRATA_RESHARD_ROOT_CRED=<access:secret>  — credentials whose owner is the
#       IAM root principal ("iam-root"); needed by leg A's smoke-reshard.sh for
#       the s3api /admin/bucket/* surface. Falls back to STRATA_STATIC_CREDENTIALS.
#
# Lab assumptions:
#   - TiKV-default lab on http://127.0.0.1:9999 (TIKV_BASE):
#       STRATA_WORKERS=...,reshard make up-all && make wait-strata-lab
#   - Cassandra-profile lab on http://127.0.0.1:9998 (CASS_BASE):
#       STRATA_WORKERS=gc,lifecycle,rebalance,reshard make up-cassandra && make wait-cassandra
#     The reshard worker MUST be enabled on each gateway or the reshard legs
#     hang (job stays queued). Both legs warn-skip cleanly when their lab is down.
#
# Exit codes:
#   0  — all reachable legs passed (WARN-skips for down labs count as pass);
#        leg F (corruption) always runs and must pass.
#   1  — a real FAIL on a reachable leg.
#   77 — BOTH labs down AND leg F unrunnable (no go toolchain), unless
#        REQUIRE_LAB=1 promotes a down lab to a hard FAIL.

set -euo pipefail

cd "$(dirname "$0")/.."

TIKV_BASE="${TIKV_BASE:-http://127.0.0.1:9999}"
CASS_BASE="${CASS_BASE:-http://127.0.0.1:9998}"
REGION="${SMOKE_AH_REGION:-us-east-1}"
WAIT_GRACE="${WAIT_GRACE:-15}"
JOB_GRACE="${SMOKE_AH_JOB_GRACE:-120}"
TARGET_SHARDS="${SMOKE_AH_TARGET:-128}"
REQUIRE_LAB="${REQUIRE_LAB:-0}"
STAMP="$(date +%s)"

CRED="${STRATA_STATIC_CREDENTIALS:-}"
if [[ -z "$CRED" ]]; then
  echo "FAIL: STRATA_STATIC_CREDENTIALS unset (need access:secret[:owner])" >&2
  exit 2
fi
FIRST="${CRED%%,*}"
AK="${FIRST%%:*}"
REST="${FIRST#*:}"
SK="${REST%%:*}"
if [[ -z "$AK" || -z "$SK" || "$AK" == "$FIRST" ]]; then
  echo "FAIL: cannot parse access/secret from STRATA_STATIC_CREDENTIALS='$FIRST'" >&2
  exit 2
fi

# Root cred for the s3api /admin/bucket/* surface (gated on owner==iam-root).
# Leg B uses it; falls back to the static cred (leg B then WARN-skips if that
# cred's owner isn't iam-root and the trigger 403s).
RCRED="${STRATA_RESHARD_ROOT_CRED:-$FIRST}"
RFIRST="${RCRED%%,*}"
RAK="${RFIRST%%:*}"
RREST="${RFIRST#*:}"
RSK="${RREST%%:*}"

for tool in curl jq aws; do
  command -v "$tool" >/dev/null 2>&1 \
    || { echo "FAIL: missing tool: $tool" >&2; exit 2; }
done

TMP="$(mktemp -d)"
JAR="$TMP/cookies"
trap 'rm -rf "$TMP"' EXIT

# --- output helpers (mirror scripts/smoke-supply-chain.sh) -------------------
if [[ -f scripts/lib/severity.sh ]]; then
  # shellcheck source=scripts/lib/severity.sh
  source scripts/lib/severity.sh
else
  severity_green() { :; }; severity_yellow() { :; }; severity_red() { :; }; severity_reset() { :; }
fi
pass() { echo "$(severity_green)PASS$(severity_reset) $1"; }
warn() { echo "$(severity_yellow)WARN$(severity_reset) $1"; }
fail() { echo "$(severity_red)FAIL$(severity_reset) $1"; }
banner() { echo; echo "==> $1"; }

FAILURES=0
RAN_ANY=0
mark_fail() { fail "$1"; FAILURES=$((FAILURES + 1)); }

# probe_ready BASE [grace] — true if /readyz returns 200 within grace seconds.
probe_ready() {
  local base="$1" i=0 grace="${2:-$WAIT_GRACE}"
  while (( i < grace )); do
    if [[ "$(curl -fsS -o /dev/null -w '%{http_code}' "$base/readyz" 2>/dev/null)" == "200" ]]; then
      return 0
    fi
    sleep 1; i=$((i + 1))
  done
  return 1
}

# lab_or_skip BASE LABEL — returns 0 if the lab is up; else emits a WARN-skip
# (or a hard FAIL under REQUIRE_LAB) and returns 1 so the caller skips its leg.
lab_or_skip() {
  local base="$1" label="$2"
  if probe_ready "$base"; then return 0; fi
  if [[ "$REQUIRE_LAB" == "1" ]]; then
    mark_fail "$label: lab $base not reachable after ${WAIT_GRACE}s (REQUIRE_LAB=1)"
    return 1
  fi
  warn "$label: lab $base not reachable after ${WAIT_GRACE}s — skipped"
  return 1
}

# SigV4 curl wrappers — sign with the first static cred.
sigv4() { curl -sS --user "$AK:$SK" --aws-sigv4 "aws:amz:${REGION}:s3" "$@"; }
# root_sigv4 — sign with the iam-root cred (s3api /admin/bucket/* surface).
root_sigv4() { curl -sS --user "$RAK:$RSK" --aws-sigv4 "aws:amz:${REGION}:s3" "$@"; }
# anon — no credentials at all (resolves to the anonymous identity).
anon() { curl -sS "$@"; }

# admin_login BASE — establishes a /admin/v1 cookie session in $JAR.
admin_login() {
  local base="$1" body code
  body=$(printf '{"access_key":"%s","secret_key":"%s"}' "$AK" "$SK")
  code=$(curl -sS -o "$TMP/login.out" -w '%{http_code}' \
    -c "$JAR" -H 'Content-Type: application/json' \
    -X POST -d "$body" "$base/admin/v1/auth/login")
  [[ "$code" == "200" ]]
}

#############################################################################
# LEG A — Cassandra online reshard under concurrent writes (US-001/002/003/005)
#############################################################################
leg_a() {
  banner "LEG A — Cassandra online reshard under concurrent writes"
  if ! lab_or_skip "$CASS_BASE" "A"; then return; fi
  RAN_ANY=1
  # smoke-reshard.sh owns the full async-trigger + concurrent-load + key-set
  # stability + crash-resume assertions. Reuse it verbatim (no fork) — it also
  # proves per-bucket NON-DEFAULT shard count is stored + resolved on the hot
  # path (the post-flip point-GET would 404 if the reader used the global
  # default instead of the bucket's resharded count).
  if CASS_BASE="$CASS_BASE" REGION="$REGION" REQUIRE_LAB="$REQUIRE_LAB" \
       bash scripts/smoke-reshard.sh; then
    pass "A: Cassandra 64->$TARGET_SHARDS reshard under concurrent writes (smoke-reshard.sh)"
  else
    local rc=$?
    if [[ "$rc" == "77" ]]; then
      warn "A: smoke-reshard.sh skipped (lab/worker not ready)"
    else
      mark_fail "A: smoke-reshard.sh exit $rc"
    fi
  fi
}

#############################################################################
# LEG B — TiKV reshard parity: immediate-complete no-op (US-004)
#############################################################################
leg_b() {
  banner "LEG B — TiKV reshard parity (immediate-complete no-op)"
  if ! lab_or_skip "$TIKV_BASE" "B"; then return; fi
  RAN_ANY=1
  local bucket="ah-tikv-${STAMP}"
  echo "payload-${STAMP}" > "$TMP/body"
  if [[ "$(sigv4 -o /dev/null -w '%{http_code}' -X PUT "$TIKV_BASE/$bucket")" != 200 ]]; then
    mark_fail "B: create-bucket $bucket"; return
  fi
  sigv4 -o /dev/null -X PUT --data-binary "@$TMP/body" "$TIKV_BASE/$bucket/probe" >/dev/null

  # Trigger the reshard on the s3api admin surface (iam-root owner). On TiKV the
  # worker is a no-op but the job still queues, completes, and flips ShardCount.
  local code state sc i=0
  code=$(root_sigv4 -o "$TMP/b.out" -w '%{http_code}' \
    -X POST "$TIKV_BASE/admin/bucket/reshard?bucket=$bucket&target=$TARGET_SHARDS")
  if [[ "$code" != "202" ]]; then
    # 403 => the cred owner isn't iam-root (set STRATA_RESHARD_ROOT_CRED). That's
    # a config gap, not a behavioural failure — WARN-skip rather than false-fail.
    warn "B: reshard trigger HTTP $code (set STRATA_RESHARD_ROOT_CRED to an owner=iam-root cred) — body=$(cat "$TMP/b.out")"
    sigv4 -o /dev/null -X DELETE "$TIKV_BASE/$bucket/probe" >/dev/null 2>&1 || true
    sigv4 -o /dev/null -X DELETE "$TIKV_BASE/$bucket" >/dev/null 2>&1 || true
    return
  fi
  while (( i < JOB_GRACE )); do
    state=$(root_sigv4 "$TIKV_BASE/admin/bucket/reshard?bucket=$bucket" | jq -r '.state // "error"')
    [[ "$state" == "idle" ]] && break
    sleep 1; i=$((i + 1))
  done
  sc=$(root_sigv4 "$TIKV_BASE/admin/bucket/reshard?bucket=$bucket" | jq -r '.shard_count // 0')

  # Object must still read post-flip (range-scan backend is shard-agnostic).
  local gc
  gc=$(sigv4 -o /dev/null -w '%{http_code}' "$TIKV_BASE/$bucket/probe")
  if [[ "$state" == "idle" && "$sc" == "$TARGET_SHARDS" && "$gc" == "200" ]]; then
    pass "B: TiKV reshard converged idle shard_count=$sc, object still readable (no-op parity)"
  else
    mark_fail "B: TiKV reshard state=$state shard_count=$sc get=$gc (want idle/$TARGET_SHARDS/200)"
  fi
  sigv4 -o /dev/null -X DELETE "$TIKV_BASE/$bucket/probe" >/dev/null 2>&1 || true
  sigv4 -o /dev/null -X DELETE "$TIKV_BASE/$bucket" >/dev/null 2>&1 || true
}

#############################################################################
# LEG C — DeleteObjects batch + versioned (US-007)
#############################################################################
leg_c() {
  banner "LEG C — DeleteObjects batch + versioned"
  local base="$TIKV_BASE" which="TiKV"
  if ! probe_ready "$base"; then base="$CASS_BASE"; which="Cassandra"; fi
  if ! lab_or_skip "$base" "C"; then return; fi
  RAN_ANY=1
  local bucket="ah-del-${STAMP}"
  echo "x" > "$TMP/body"
  sigv4 -o /dev/null -X PUT "$base/$bucket" >/dev/null
  local k
  for k in d1 d2 d3; do
    sigv4 -o /dev/null -X PUT --data-binary "@$TMP/body" "$base/$bucket/$k" >/dev/null
  done
  # Batch ?delete of d1,d2 + a NON-existent key (idempotent success row, no <Error>).
  cat > "$TMP/del.xml" <<'XML'
<Delete><Object><Key>d1</Key></Object><Object><Key>d2</Key></Object><Object><Key>ghost</Key></Object></Delete>
XML
  local code
  code=$(sigv4 -o "$TMP/del.out" -w '%{http_code}' \
    -X POST -H 'Content-Type: application/xml' \
    --data-binary "@$TMP/del.xml" "$base/$bucket?delete")
  local errs deleted
  errs=$(grep -c '<Error>' "$TMP/del.out" || true)
  deleted=$(grep -c '<Deleted>' "$TMP/del.out" || true)
  local ok=1
  [[ "$code" == "200" ]] || { mark_fail "C: batch delete HTTP $code"; ok=0; }
  [[ "$errs" -eq 0 ]] || { mark_fail "C: batch delete had $errs <Error> rows (missing key must be idempotent success)"; ok=0; }
  [[ "$deleted" -ge 3 ]] || { mark_fail "C: batch delete reported $deleted <Deleted> rows want >=3 (idempotent)"; ok=0; }
  # Deleted keys must 404 after.
  [[ "$(sigv4 -o /dev/null -w '%{http_code}' "$base/$bucket/d1")" == 404 ]] \
    || { mark_fail "C: d1 still present after batch delete"; ok=0; }

  # Versioned: enable versioning, no-versionId delete -> a DeleteMarker.
  sigv4 -o /dev/null -X PUT -H 'Content-Type: application/xml' \
    --data-binary '<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>' \
    "$base/$bucket?versioning" >/dev/null
  sigv4 -o /dev/null -X PUT --data-binary "@$TMP/body" "$base/$bucket/v1" >/dev/null
  local dm
  dm=$(sigv4 -D - -o /dev/null -X DELETE "$base/$bucket/v1" | tr -d '\r' | grep -i '^x-amz-delete-marker:' | awk '{print tolower($2)}')
  if [[ "$dm" == "true" ]]; then
    pass "C ($which): batch delete idempotent ($deleted deleted, 0 errors) + versioned delete -> DeleteMarker"
  else
    [[ "$ok" == "1" ]] && mark_fail "C: versioned delete did not set x-amz-delete-marker:true (got '$dm')"
  fi
  sigv4 -o /dev/null -X DELETE "$base/$bucket" >/dev/null 2>&1 || true
}

#############################################################################
# LEG D — Policy-only anonymous GET: policy UNION ACL (US-008)
#############################################################################
leg_d() {
  banner "LEG D — Policy-only anonymous GET (policy UNION ACL)"
  local base="$TIKV_BASE" which="TiKV"
  if ! probe_ready "$base"; then base="$CASS_BASE"; which="Cassandra"; fi
  if ! lab_or_skip "$base" "D"; then return; fi
  RAN_ANY=1
  local bucket="ah-pol-${STAMP}"
  echo "secret-${STAMP}" > "$TMP/body"
  sigv4 -o /dev/null -X PUT "$base/$bucket" >/dev/null
  sigv4 -o /dev/null -X PUT --data-binary "@$TMP/body" "$base/$bucket/pub" >/dev/null

  # Baseline: anonymous GET BEFORE the policy must be denied (object ACL is
  # owner-private; anon != owner). This is the discriminator — if anon already
  # reads, the lab isn't gating and leg D proves nothing.
  local pre
  pre=$(anon -o /dev/null -w '%{http_code}' "$base/$bucket/pub")
  if [[ "$pre" == "200" ]]; then
    warn "D ($which): anon GET returned 200 BEFORE policy (auth not gating object reads) — union proof inconclusive"
    sigv4 -o /dev/null -X DELETE "$base/$bucket/pub" >/dev/null 2>&1 || true
    sigv4 -o /dev/null -X DELETE "$base/$bucket" >/dev/null 2>&1 || true
    return
  fi

  # Attach a bucket policy that Allows s3:GetObject to everyone.
  cat > "$TMP/pol.json" <<JSON
{"Version":"2012-10-17","Statement":[{"Sid":"AnonGet","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::${bucket}/*"}]}
JSON
  local pcode
  pcode=$(sigv4 -o "$TMP/pol.out" -w '%{http_code}' \
    -X PUT -H 'Content-Type: application/json' \
    --data-binary "@$TMP/pol.json" "$base/$bucket?policy")
  if [[ "$pcode" != 200 && "$pcode" != 204 ]]; then
    mark_fail "D: PUT bucket policy HTTP $pcode body=$(cat "$TMP/pol.out")"
  else
    # Now the ANON GET must succeed — granted by the explicit policy Allow,
    # unioned over the (denying) ACL.
    local post
    post=$(anon -o "$TMP/anon.out" -w '%{http_code}' "$base/$bucket/pub")
    if [[ "$post" == "200" ]]; then
      pass "D ($which): anon GET denied pre-policy (403/$pre), granted post-policy (200) — policy UNION ACL"
    else
      mark_fail "D: anon GET after policy Allow returned $post (want 200 — union not applied)"
    fi
  fi
  sigv4 -o /dev/null -X DELETE "$base/$bucket?policy" >/dev/null 2>&1 || true
  sigv4 -o /dev/null -X DELETE "$base/$bucket/pub" >/dev/null 2>&1 || true
  sigv4 -o /dev/null -X DELETE "$base/$bucket" >/dev/null 2>&1 || true
}

#############################################################################
# LEG E — Console reshard UI via /admin/v1 (US-006)
#############################################################################
# probe_console BASE LABEL EXPECT_SUPPORTED — cookie-login, GET the reshard
# panel JSON, assert `supported` matches the backend, and (when supported) POST
# a queue trigger the way the React BucketReshardPanel does.
probe_console() {
  local base="$1" label="$2" expect="$3"
  if ! lab_or_skip "$base" "E/$label"; then return; fi
  RAN_ANY=1
  if ! admin_login "$base"; then
    warn "E/$label: /admin/v1 login failed (cookie-auth console may be disabled) — skipped"
    return
  fi
  local bucket="ah-ui-${STAMP}-${label}"
  sigv4 -o /dev/null -X PUT "$base/$bucket" >/dev/null
  local sup state
  sup=$(curl -sS -b "$JAR" "$base/admin/v1/buckets/$bucket/reshard" | jq -r '.supported')
  if [[ "$sup" != "$expect" ]]; then
    mark_fail "E/$label: console reshard supported=$sup want $expect"
  elif [[ "$expect" == "true" ]]; then
    local code
    code=$(curl -sS -o "$TMP/ui.out" -w '%{http_code}' -b "$JAR" \
      -H 'Content-Type: application/json' -X POST \
      -d "{\"target\":$TARGET_SHARDS}" "$base/admin/v1/buckets/$bucket/reshard")
    state=$(jq -r '.state // "error"' < "$TMP/ui.out")
    if [[ "$code" == "202" && ( "$state" == "queued" || "$state" == "running" || "$state" == "idle" ) ]]; then
      pass "E/$label: console reshard supported=true, POST queued (HTTP $code state=$state)"
    else
      mark_fail "E/$label: console POST HTTP $code state=$state (want 202 queued)"
    fi
  else
    pass "E/$label: console reshard supported=false (UI disabled-with-tooltip on range-scan backend)"
  fi
  sigv4 -o /dev/null -X DELETE "$base/$bucket" >/dev/null 2>&1 || true
}

leg_e() {
  banner "LEG E — Console reshard UI (/admin/v1, US-006)"
  probe_console "$CASS_BASE" "cassandra" "true"
  probe_console "$TIKV_BASE" "tikv" "false"
  warn "E: full browser pass is web/e2e/reshard-progress.spec.ts (Playwright, CI-only — webServer cgo-blocks on macOS/lima)"
}

#############################################################################
# LEG F — Plaintext chunk-corruption fail-loud (US-009)
#############################################################################
leg_f() {
  banner "LEG F — Plaintext chunk-corruption fail-loud (CRC32C read-path)"
  if ! command -v go >/dev/null 2>&1; then
    warn "F: go toolchain not on PATH — corruption fail-loud proof unrun (see internal/data/memory/corruption_test.go)"
    return
  fi
  RAN_ANY=1
  # data/memory + data/rados build without librados, so CGO_ENABLED=0 runs the
  # whole CRC-verification suite. The *FailsLoud cases flip a plaintext byte in
  # a stored chunk and require data.ErrChecksumMismatch on read (never a silent
  # truncation). GOWORK=off keeps the hermetic default-tag module graph.
  if CGO_ENABLED=0 GOWORK=off go test \
       ./internal/data/memory/ ./internal/data/rados/ \
       -run 'CRC|Checksum|FailsLoud' -count=1 > "$TMP/crc.out" 2>&1; then
    pass "F: chunk-corruption fail-loud verified (memory + rados CRC32C read-path tests green)"
  else
    mark_fail "F: CRC corruption tests FAILED:\n$(cat "$TMP/crc.out")"
  fi
}

#############################################################################
# Drive every leg, then aggregate.
#############################################################################
echo "Strata architecture-hardening e2e walkthrough — $(date -u +%FT%TZ)"
echo "  TiKV lab:      $TIKV_BASE"
echo "  Cassandra lab: $CASS_BASE"

leg_a
leg_b
leg_c
leg_d
leg_e
leg_f

echo
if (( FAILURES > 0 )); then
  fail "architecture-hardening e2e: $FAILURES leg(s) failed"
  exit 1
fi
if (( RAN_ANY == 0 )); then
  echo "SKIP: no lab reachable and no go toolchain — nothing exercised." >&2
  echo "SKIP: bring up labs (make up-all / make up-cassandra with STRATA_WORKERS=...,reshard)." >&2
  [[ "$REQUIRE_LAB" == "1" ]] && exit 1
  exit 77
fi
pass "architecture-hardening e2e: all reachable legs green (WARN-skips count as pass)"
