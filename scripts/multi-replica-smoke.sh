#!/usr/bin/env bash
# Multi-replica lab smoke harness (US-005).
#
# Drives the three failure scenarios end-to-end against a stack brought up
# by `make up-lab-tikv`:
#   (a) baseline: 2 healthy nodes, exactly one lifecycle-leader + one gc-leader chip
#   (b) docker stop strata-tikv-a -> sleep 35 -> single survivor carries BOTH chips
#   (c) docker start strata-tikv-a -> sleep 30 -> 2 healthy again
#   (d) PUT via replica-a (9001), GET via replica-b (9002), byte-equal payload
#   (e) kill the lifecycle-leader holder -> wait 35s -> chip rotates to the OTHER replica
#
# Prereqs on the host: docker, curl, jq, aws (>= 2). Compose lab-tikv profile up.
# STRATA_STATIC_CREDENTIALS exported with the same value the gateway booted with
# (script parses its first comma-separated entry as access:secret[:owner]).
#
# Exit 0 on all green; non-zero with descriptive `FAIL: <scenario>` on any failure.

set -euo pipefail

LB="${LB:-http://127.0.0.1:9999}"
A="${A:-http://127.0.0.1:9001}"
B="${B:-http://127.0.0.1:9002}"
CT_A="${CT_A:-strata-tikv-a}"
CT_B="${CT_B:-strata-tikv-b}"

# Heartbeat TTL is 30s; the chip-rotation grace is +5s to absorb the next
# lease renew tick on the surviving replica.
DEAD_GRACE="${DEAD_GRACE:-35}"
REJOIN_GRACE="${REJOIN_GRACE:-30}"

CRED="${STRATA_STATIC_CREDENTIALS:-}"
if [[ -z "$CRED" ]]; then
  echo "FAIL: STRATA_STATIC_CREDENTIALS unset (need access:secret[:owner])" >&2
  exit 2
fi
FIRST_ENTRY="${CRED%%,*}"
AK="${FIRST_ENTRY%%:*}"
REST="${FIRST_ENTRY#*:}"
SK="${REST%%:*}"
if [[ -z "$AK" || -z "$SK" || "$AK" == "$FIRST_ENTRY" ]]; then
  echo "FAIL: cannot parse access/secret from STRATA_STATIC_CREDENTIALS='$FIRST_ENTRY'" >&2
  exit 2
fi

for tool in curl jq aws docker; do
  command -v "$tool" >/dev/null 2>&1 || { echo "FAIL: missing tool: $tool" >&2; exit 2; }
done

TMP="$(mktemp -d)"
JAR="$TMP/cookies"
BUCKET="lab-mr-$(date +%s)"
CLEAN_BUCKET=0

cleanup() {
  if (( CLEAN_BUCKET )); then
    aws --endpoint-url "$LB" s3 rm "s3://$BUCKET/blob.bin" >/dev/null 2>&1 || true
    aws --endpoint-url "$LB" s3api delete-bucket --bucket "$BUCKET" >/dev/null 2>&1 || true
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }

login() {
  local body code
  body=$(printf '{"access_key":"%s","secret_key":"%s"}' "$AK" "$SK")
  code=$(curl -sS -o "$TMP/login.out" -w '%{http_code}' \
    -c "$JAR" -H 'Content-Type: application/json' \
    -X POST -d "$body" "$LB/admin/v1/auth/login")
  [[ "$code" == "200" ]] || fail "login: HTTP $code body=$(cat "$TMP/login.out")"
}

nodes_json() {
  curl -sf -b "$JAR" "$LB/admin/v1/cluster/nodes"
}

healthy_count() {
  nodes_json | jq -r '[.nodes[] | select(.status=="healthy")] | length'
}

# Echoes the IDs (one per line) of nodes that carry the named chip.
chip_holders() {
  local chip="$1"
  nodes_json | jq -r --arg c "$chip" \
    '.nodes[] | select(.leader_for | index($c)) | .id'
}

# Wait until /readyz on a given base URL returns 200 (max 60s).
wait_ready() {
  local base="$1" i=0
  while (( i < 60 )); do
    if [[ "$(curl -fsS -o /dev/null -w '%{http_code}' "$base/readyz")" == "200" ]]; then
      return 0
    fi
    sleep 1
    i=$((i+1))
  done
  fail "readyz on $base never became 200"
}

# ---------------------------------------------------------------- (a)
echo "== (a) baseline 2 healthy + lifecycle/gc chip exactly once each"
login
HC=$(healthy_count)
[[ "$HC" == "2" ]] || fail "(a) expected 2 healthy nodes, got $HC"
LCC=$(chip_holders lifecycle-leader | wc -l | awk '{print $1}')
GCC=$(chip_holders gc-leader | wc -l | awk '{print $1}')
[[ "$LCC" == "1" ]] || fail "(a) expected 1 lifecycle-leader holder, got $LCC"
[[ "$GCC" == "1" ]] || fail "(a) expected 1 gc-leader holder, got $GCC"
pass "(a) baseline"

# ---------------------------------------------------------------- (b)
echo "== (b) stop $CT_A; surviving replica carries both chips"
docker stop "$CT_A" >/dev/null
sleep "$DEAD_GRACE"
HC=$(healthy_count)
[[ "$HC" == "1" ]] || fail "(b) expected 1 healthy node after stop, got $HC"
SURVIVOR=$(nodes_json | jq -r '.nodes[] | select(.status=="healthy") | .id')
LC_HOLDER=$(chip_holders lifecycle-leader)
GC_HOLDER=$(chip_holders gc-leader)
[[ "$LC_HOLDER" == "$SURVIVOR" ]] || fail "(b) lifecycle-leader on '$LC_HOLDER', expected survivor '$SURVIVOR'"
[[ "$GC_HOLDER" == "$SURVIVOR" ]] || fail "(b) gc-leader on '$GC_HOLDER', expected survivor '$SURVIVOR'"
pass "(b) single-replica failover survivor=$SURVIVOR"

# ---------------------------------------------------------------- (c)
echo "== (c) start $CT_A; both healthy again"
docker start "$CT_A" >/dev/null
wait_ready "$A"
sleep "$REJOIN_GRACE"
HC=$(healthy_count)
[[ "$HC" == "2" ]] || fail "(c) expected 2 healthy after restart, got $HC"
pass "(c) rejoined"

# ---------------------------------------------------------------- (d)
echo "== (d) PUT via $A (9001), GET via $B (9002), byte-equal"
export AWS_ACCESS_KEY_ID="$AK"
export AWS_SECRET_ACCESS_KEY="$SK"
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-east-1}"
export AWS_EC2_METADATA_DISABLED=true
PAYLOAD="$TMP/payload.bin"
dd if=/dev/urandom of="$PAYLOAD" bs=1M count=4 2>/dev/null
ORIG_MD5=$(md5sum "$PAYLOAD" 2>/dev/null | awk '{print $1}')
[[ -n "$ORIG_MD5" ]] || ORIG_MD5=$(md5 -q "$PAYLOAD")
aws --endpoint-url "$A" s3api create-bucket --bucket "$BUCKET" >/dev/null
CLEAN_BUCKET=1
aws --endpoint-url "$A" s3 cp "$PAYLOAD" "s3://$BUCKET/blob.bin" >/dev/null
aws --endpoint-url "$B" s3 cp "s3://$BUCKET/blob.bin" "$TMP/blob.got" >/dev/null
GOT_MD5=$(md5sum "$TMP/blob.got" 2>/dev/null | awk '{print $1}')
[[ -n "$GOT_MD5" ]] || GOT_MD5=$(md5 -q "$TMP/blob.got")
[[ "$ORIG_MD5" == "$GOT_MD5" ]] || fail "(d) cross-replica md5 mismatch: $ORIG_MD5 vs $GOT_MD5"
pass "(d) cross-replica PUT/GET md5=$ORIG_MD5"

# ---------------------------------------------------------------- (e)
echo "== (e) kill lifecycle-leader holder; chip rotates to the OTHER replica"
HOLDER_ID=$(chip_holders lifecycle-leader)
[[ -n "$HOLDER_ID" ]] || fail "(e) no lifecycle-leader holder before scenario"
case "$HOLDER_ID" in
  strata-a) HOLDER_CT="$CT_A"; OTHER_ID=strata-b;;
  strata-b) HOLDER_CT="$CT_B"; OTHER_ID=strata-a;;
  *) fail "(e) unknown lifecycle-leader holder id: $HOLDER_ID";;
esac
docker kill "$HOLDER_CT" >/dev/null
sleep "$DEAD_GRACE"
NEW_HOLDER=$(chip_holders lifecycle-leader)
[[ "$NEW_HOLDER" == "$OTHER_ID" ]] || fail "(e) lifecycle-leader expected to rotate to $OTHER_ID, got '$NEW_HOLDER'"
docker start "$HOLDER_CT" >/dev/null
pass "(e) lifecycle-leader rotated $HOLDER_ID -> $OTHER_ID"

echo "== multi-replica smoke OK"
