#!/bin/bash
set -euo pipefail

# RGW bench entrypoint — separate `ceph/ceph:v19.2.3` container that joins the
# existing strata ceph-a cluster via the shared `strata-ceph-etc` volume (ro
# mount of /etc/ceph + ceph.client.admin.keyring). Bootstraps the minimal
# realm/zonegroup/zone chain that RGW has required since Jewel, creates a
# bench S3 user idempotently, and persists access/secret creds to the
# `strata-bench-creds` volume for the bench script (US-002).
#
# Path-(b) manual chain chosen over `ceph orch apply rgw` because the stratall
# lab runs a standalone ceph daemon (no cephadm / orchestrator). Manual chain
# is also shorter for an entrypoint script.

REALM="${RGW_REALM:-default}"
ZONEGROUP="${RGW_ZONEGROUP:-default}"
ZONE="${RGW_ZONE:-default}"
RGW_NAME="${RGW_NAME:-bench}"
RGW_LISTEN_PORT="${RGW_LISTEN_PORT:-8080}"
BENCH_CREDS_DIR="${RGW_CREDS_DIR:-/etc/strata-bench}"
BENCH_CREDS_FILE="${BENCH_CREDS_DIR}/rgw-creds.env"

echo "rgw-entrypoint: realm=${REALM} zonegroup=${ZONEGROUP} zone=${ZONE} name=${RGW_NAME}"

echo "rgw-entrypoint: waiting for ceph mon..."
mon_ready=0
for i in $(seq 1 60); do
  if ceph -s --connect-timeout=2 >/dev/null 2>&1; then
    echo "rgw-entrypoint: mon reachable after ${i}s"
    mon_ready=1
    break
  fi
  sleep 1
done
if [ "${mon_ready}" -ne 1 ]; then
  echo "rgw-entrypoint: mon unreachable after 60s" >&2
  exit 1
fi

# Lab tuning to allow RGW's pool bootstrap on the single-OSD memstore lab.
# Two knobs (idempotent):
#  - mon_max_pg_per_osd=1000: Ceph default 250 is exhausted by strata's
#    pre-existing pools + RGW's ~7 new pools when autoscaler picks 32 PG
#    each. pool_create then returns EOVERFLOW (34). Lab memstore has no
#    on-disk PG bookkeeping cost so raising the cap is safe.
#  - osd_pool_default_pg_autoscale_mode=off: forces new RGW pools to use
#    pg_num=8 default instead of autoscaler-targeted 32, keeping the OSD
#    well below the cap. Lab-only knob — production runs leave autoscaler on.
echo "rgw-entrypoint: lab pool tuning (max_pg_per_osd=1000, autoscaler off)"
ceph config set global mon_max_pg_per_osd 1000 || true
ceph config set osd osd_pool_default_pg_autoscale_mode off || true

# Pre-create `.rgw.root` — librados's pool_create rejects dot-prefixed pool
# names (modern Ceph guard against accidental system-pool naming); the
# `ceph osd pool create ... --yes-i-really-mean-it` CLI path is the documented
# bypass. radosgw-admin uses librados directly, so it cannot create this pool
# itself and `realm create` fails with EOVERFLOW (34) "Numerical result out
# of range" until the pool exists. Subsequent RGW pools (`default.rgw.meta`,
# `default.rgw.log`, `default.rgw.control`, `default.rgw.buckets.*`) are NOT
# dot-prefixed so RGW creates them itself on first use.
if ! ceph osd pool ls 2>/dev/null | grep -qx '\.rgw\.root'; then
  echo "rgw-entrypoint: pre-creating .rgw.root pool"
  ceph osd pool create .rgw.root 8 8 replicated --yes-i-really-mean-it
  ceph osd pool application enable .rgw.root rgw || true
fi

# Pre-create RGW backing pools BEFORE `period update --commit` — breaks the
# chicken-and-egg deadlock observed on Ceph squid (v19.2.x):
#   radosgw-admin period commit refuses while "current period does not have
#   zone configured", and the zone -> period propagation step internally
#   calls librados pool_create on `default.rgw.{control,meta,log}` which
#   returns EOVERFLOW (34, "Numerical result out of range") in single-OSD
#   lab mode. The period commit then never lands; subsequent cycles inherit
#   the empty period and RGW refuses to serve I/O. Pre-creating the pools
#   side-steps the librados path entirely so period commit's zone -> period
#   propagation can succeed on first boot.
#
# All three are non-dot-prefixed so the plain `osd pool create` CLI accepts
# them. pg_num/pgp_num pinned to 8 + autoscaler off (via the global config
# set above) so the OSD's mon_max_pg_per_osd budget stays comfortable.
for pool in default.rgw.control default.rgw.meta default.rgw.log; do
  if ! ceph osd pool ls 2>/dev/null | grep -qx "${pool}"; then
    echo "rgw-entrypoint: pre-creating ${pool} pool"
    ceph osd pool create "${pool}" 8 8 replicated
    ceph osd pool application enable "${pool}" rgw || true
  fi
done

# US-007 pre-create RGW bucket-backing pools. Without these, the FIRST
# CreateBucket against RGW returns `<Code>InvalidRange</Code><Message></Message>`
# (botocore then crashes with `TypeError: argument of type 'NoneType' is not
# iterable` on the empty Message — surfaces to the operator as a confusing
# aws-cli stack trace, not the real "pool missing" cause). Root cause: RGW's
# default zone references `default.rgw.buckets.{index,data,non-ec}` for bucket
# placement, and the auto-create path inside RGW hits the same EOVERFLOW that
# the control/meta/log pools hit (single-OSD memstore can't allocate PGs
# transparently). Pre-creating the three bucket pools side-steps the auto-create
# path and lets the bench harness's CreateBucket succeed on cycle 1.
for pool in default.rgw.buckets.index default.rgw.buckets.data default.rgw.buckets.non-ec; do
  if ! ceph osd pool ls 2>/dev/null | grep -qx "${pool}"; then
    echo "rgw-entrypoint: pre-creating ${pool} pool"
    ceph osd pool create "${pool}" 8 8 replicated
    ceph osd pool application enable "${pool}" rgw || true
  fi
done

# radosgw-admin's first invocation against a freshly-restarted single-OSD
# memstore lab can hang on internal librados-client thread sync (observed
# stuck in `futex_wait_queue` for >60s on cycle 2+ even though `ceph -s`
# and `ceph osd pool stats .rgw.root` return immediately). Wrap every
# radosgw-admin invocation below in a 30s-timeout + 3-retry loop so the
# bootstrap recovers from the hang transparently. Idempotent semantics of
# every radosgw-admin call invoked here (get/info/create-if-absent) make
# retries safe.
radosgw_retry() {
  local attempt
  for attempt in 1 2 3; do
    if timeout 30 radosgw-admin "$@"; then
      return 0
    fi
    local rc=$?
    if [ "${rc}" -eq 124 ]; then
      echo "rgw-entrypoint: radosgw-admin $1 hung (attempt ${attempt}/3) — retrying" >&2
    elif [ "${attempt}" -lt 3 ]; then
      echo "rgw-entrypoint: radosgw-admin $1 exited ${rc} (attempt ${attempt}/3) — retrying" >&2
    fi
  done
  return 1
}
radosgw_retry_quiet() {
  local attempt
  for attempt in 1 2 3; do
    if timeout 30 radosgw-admin "$@" >/dev/null 2>&1; then
      return 0
    fi
  done
  return 1
}

# Realm/zonegroup/zone bootstrap (idempotent — get-first, create-if-absent).
if ! radosgw_retry_quiet realm get --rgw-realm="${REALM}"; then
  echo "rgw-entrypoint: creating realm ${REALM}"
  radosgw_retry realm create --rgw-realm="${REALM}" --default >/dev/null
fi

if ! radosgw_retry_quiet zonegroup get --rgw-zonegroup="${ZONEGROUP}"; then
  echo "rgw-entrypoint: creating zonegroup ${ZONEGROUP}"
  radosgw_retry zonegroup create --rgw-zonegroup="${ZONEGROUP}" --rgw-realm="${REALM}" --master --default >/dev/null
fi

if ! radosgw_retry_quiet zone get --rgw-zone="${ZONE}"; then
  echo "rgw-entrypoint: creating zone ${ZONE}"
  radosgw_retry zone create --rgw-zone="${ZONE}" --rgw-zonegroup="${ZONEGROUP}" --rgw-realm="${REALM}" --master --default >/dev/null
fi

# Period reconcile — investigation (US-005 of ralph/p1-fixes):
#
# Drift symptom on `make down && make up-all && make up-bench-rgw` × N: cycle 1
# brings up RGW cleanly, cycle 2+ hangs at `wait-rgw` because RGW daemon refuses
# to serve I/O until the period agrees with the zonegroup membership.
#
# Root cause: `radosgw-admin zone create --rgw-zonegroup=X --master --default`
# creates the zone row but does NOT always add the zone to the zonegroup's
# `zones[]` array. The `period update --commit` immediately after the zone
# create reads `zonegroup.zones[] = []` and commits a period whose zonegroup
# has no zones. On the next container boot the zone/zonegroup/realm rows still
# exist (idempotent guards skip create), but the latest committed period still
# references the empty zones[] from cycle 1 — period_update_commit on cycle 2
# is a no-op increment. RGW daemon starts pointing at zone=Z but the period's
# zonegroup has zones[] empty → no rgw_zonegroup → 503 / hang.
#
# Diff between cycle-1 and cycle-2 `radosgw-admin period get` JSON:
#   period_map.zonegroups[?(.name="default")].zones — empty on both cycles
#   (the drift is silent — period commit doesn't repopulate zones[] from
#   the zone-create side).
#
# Fix: before `period update --commit`, reconcile zonegroup → zone membership
# explicitly via `radosgw-admin zonegroup add --rgw-zone=Z`. Idempotent — no-op
# if zone is already a member. Also ensure master_zone is set so RGW can
# resolve the master endpoint for replication metadata.
echo "rgw-entrypoint: period reconcile — inspecting zonegroup ${ZONEGROUP} for zone ${ZONE}"
ZG_JSON="$(radosgw_retry zonegroup get --rgw-zonegroup="${ZONEGROUP}")"
zone_in_zg=0
if command -v jq >/dev/null 2>&1; then
  if echo "${ZG_JSON}" | jq -e --arg z "${ZONE}" '.zones[]? | select(.name == $z)' >/dev/null 2>&1; then
    zone_in_zg=1
  fi
  master_zone_id="$(echo "${ZG_JSON}" | jq -r '.master_zone // ""')"
  zone_id="$(radosgw_retry zone get --rgw-zone="${ZONE}" | jq -r '.id // ""')"
else
  if echo "${ZG_JSON}" | python3 -c 'import sys,json; d=json.load(sys.stdin); n=sys.argv[1]; sys.exit(0 if any(z.get("name")==n for z in d.get("zones",[])) else 1)' "${ZONE}" 2>/dev/null; then
    zone_in_zg=1
  fi
  master_zone_id="$(echo "${ZG_JSON}" | python3 -c 'import sys,json; d=json.load(sys.stdin); print(d.get("master_zone",""))')"
  zone_id="$(radosgw_retry zone get --rgw-zone="${ZONE}" | python3 -c 'import sys,json; print(json.load(sys.stdin).get("id",""))')"
fi

if [ "${zone_in_zg}" -ne 1 ]; then
  echo "rgw-entrypoint: period reconcile — zone ${ZONE} missing from zonegroup ${ZONEGROUP}, adding"
  radosgw_retry zonegroup add --rgw-zonegroup="${ZONEGROUP}" --rgw-zone="${ZONE}" >/dev/null
else
  echo "rgw-entrypoint: period reconcile — zone ${ZONE} already in zonegroup ${ZONEGROUP}"
fi

if [ -n "${zone_id}" ] && [ "${master_zone_id}" != "${zone_id}" ]; then
  echo "rgw-entrypoint: period reconcile — setting master_zone=${zone_id} on zonegroup ${ZONEGROUP} (was: '${master_zone_id}')"
  radosgw_retry zonegroup modify --rgw-zonegroup="${ZONEGROUP}" --rgw-zone="${ZONE}" --master --default >/dev/null
fi

# Operator visibility — final zonegroup snapshot summarising zonegroup count
# (period_map.zonegroups[]) + per-zonegroup zone count. Format keeps the line
# greppable and matches the AC contract in tasks/prd-p1-fixes / scripts/ralph
# US-005.
ZG_AFTER="$(radosgw_retry zonegroup get --rgw-zonegroup="${ZONEGROUP}")"
if command -v jq >/dev/null 2>&1; then
  zg_summary="$(echo "${ZG_AFTER}" | jq -r '"\(.name):\(.zones | length)"')"
else
  zg_summary="$(echo "${ZG_AFTER}" | python3 -c 'import sys,json; d=json.load(sys.stdin); print("%s:%d" % (d.get("name",""), len(d.get("zones",[]))))')"
fi
echo "strata-rgw-bootstrap: period reconcile — zonegroups=1, zones-per-zonegroup=[${zg_summary}]"

echo "rgw-entrypoint: period update --commit"
radosgw_retry period update --commit --rgw-realm="${REALM}" >/dev/null

# Bench S3 user — `radosgw-admin user create` errors if uid exists, so guard
# by `user info` first (PRD allows `|| true` on duplicate; explicit check is
# cleaner so a real failure still surfaces).
if ! radosgw_retry_quiet user info --uid=bench; then
  echo "rgw-entrypoint: creating bench user"
  radosgw_retry user create --uid=bench --display-name="Bench" >/dev/null
else
  echo "rgw-entrypoint: bench user already present"
fi

# RGW daemon keyring — get-or-create is idempotent (returns existing key on
# repeat runs). Lives in container tmpfs so it disappears with the container;
# regenerated on next boot from auth state in the mon.
RGW_KEYRING_DIR="/var/lib/ceph/radosgw/ceph-rgw.${RGW_NAME}"
mkdir -p "${RGW_KEYRING_DIR}"
ceph auth get-or-create "client.rgw.${RGW_NAME}" \
  mon 'allow rw' \
  osd 'allow rwx' \
  -o "${RGW_KEYRING_DIR}/keyring"
chown -R ceph:ceph "${RGW_KEYRING_DIR}"

# Parse access/secret. jq is the preferred path; python3 is the documented
# fallback (verified present in ceph/ceph:v19.2.3 image — needed by the
# orchestrator). Log which parser was chosen so operator can debug.
echo "rgw-entrypoint: parsing user creds"
USER_JSON="$(radosgw_retry user info --uid=bench)"
if command -v jq >/dev/null 2>&1; then
  echo "rgw-entrypoint: using jq for cred parsing"
  ACCESS_KEY="$(echo "${USER_JSON}" | jq -r '.keys[0].access_key')"
  SECRET_KEY="$(echo "${USER_JSON}" | jq -r '.keys[0].secret_key')"
else
  echo "rgw-entrypoint: jq absent — falling back to python3"
  ACCESS_KEY="$(echo "${USER_JSON}" | python3 -c 'import sys,json; print(json.load(sys.stdin)["keys"][0]["access_key"])')"
  SECRET_KEY="$(echo "${USER_JSON}" | python3 -c 'import sys,json; print(json.load(sys.stdin)["keys"][0]["secret_key"])')"
fi

if [ -z "${ACCESS_KEY:-}" ] || [ -z "${SECRET_KEY:-}" ]; then
  echo "rgw-entrypoint: failed to parse bench user creds" >&2
  echo "${USER_JSON}" >&2
  exit 1
fi

mkdir -p "${BENCH_CREDS_DIR}"
cat > "${BENCH_CREDS_FILE}" <<EOF
# Generated by rgw-entrypoint.sh — bench user S3 creds for the rgw container.
# Sourced by scripts/bench-rgw-comparison.sh (US-002).
# Lowercase keys match the documented parser contract (extract_rgw_creds in
# bench-rgw-comparison.sh greps ^access_key=/^secret_key=). Uppercase RGW_*
# aliases retained for callers that shell-source the file directly.
access_key=${ACCESS_KEY}
secret_key=${SECRET_KEY}
RGW_ACCESS_KEY=${ACCESS_KEY}
RGW_SECRET_KEY=${SECRET_KEY}
RGW_ENDPOINT_URL=http://localhost:9991
EOF
chmod 0644 "${BENCH_CREDS_FILE}"
echo "rgw-entrypoint: creds written to ${BENCH_CREDS_FILE}"

echo "rgw-entrypoint: starting radosgw -n client.rgw.${RGW_NAME} on port ${RGW_LISTEN_PORT}"
exec radosgw -d -n "client.rgw.${RGW_NAME}" \
  --rgw_frontends="beast endpoint=0.0.0.0:${RGW_LISTEN_PORT}" \
  --rgw_zone="${ZONE}" \
  --rgw_zonegroup="${ZONEGROUP}" \
  --rgw_realm="${REALM}"
