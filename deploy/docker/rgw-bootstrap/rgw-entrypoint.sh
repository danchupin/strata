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

# Realm/zonegroup/zone bootstrap (idempotent — get-first, create-if-absent).
if ! radosgw-admin realm get --rgw-realm="${REALM}" >/dev/null 2>&1; then
  echo "rgw-entrypoint: creating realm ${REALM}"
  radosgw-admin realm create --rgw-realm="${REALM}" --default
fi

if ! radosgw-admin zonegroup get --rgw-zonegroup="${ZONEGROUP}" >/dev/null 2>&1; then
  echo "rgw-entrypoint: creating zonegroup ${ZONEGROUP}"
  radosgw-admin zonegroup create --rgw-zonegroup="${ZONEGROUP}" --rgw-realm="${REALM}" --master --default
fi

if ! radosgw-admin zone get --rgw-zone="${ZONE}" >/dev/null 2>&1; then
  echo "rgw-entrypoint: creating zone ${ZONE}"
  radosgw-admin zone create --rgw-zone="${ZONE}" --rgw-zonegroup="${ZONEGROUP}" --rgw-realm="${REALM}" --master --default
fi

echo "rgw-entrypoint: period update --commit"
radosgw-admin period update --commit --rgw-realm="${REALM}" >/dev/null

# Bench S3 user — `radosgw-admin user create` errors if uid exists, so guard
# by `user info` first (PRD allows `|| true` on duplicate; explicit check is
# cleaner so a real failure still surfaces).
if ! radosgw-admin user info --uid=bench >/dev/null 2>&1; then
  echo "rgw-entrypoint: creating bench user"
  radosgw-admin user create --uid=bench --display-name="Bench" >/dev/null
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
USER_JSON="$(radosgw-admin user info --uid=bench)"
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
