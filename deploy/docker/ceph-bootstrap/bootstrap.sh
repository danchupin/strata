#!/bin/bash
set -euo pipefail

MON_NAME="${MON_NAME:-ceph}"
MON_DATA="/var/lib/ceph/mon/ceph-${MON_NAME}"
OSD_ID="${OSD_ID:-0}"
OSD_DATA="/var/lib/ceph/osd/ceph-${OSD_ID}"
MGR_NAME="${MGR_NAME:-x}"
MGR_DATA="/var/lib/ceph/mgr/ceph-${MGR_NAME}"
MEMSTORE_BYTES="${MEMSTORE_BYTES:-4294967296}"
STRATA_POOL="${STRATA_POOL:-strata.rgw.buckets.data}"
STRATA_EXTRA_POOLS="${STRATA_EXTRA_POOLS:-}"

mkdir -p /etc/ceph /var/lib/ceph/{mon,osd,mgr,tmp}
chown -R ceph:ceph /var/lib/ceph /etc/ceph

MON_IP="$(hostname -i | awk '{print $1}')"

if [ ! -f /etc/ceph/ceph.conf ]; then
  FSID="$(uuidgen)"
  cat >/etc/ceph/ceph.conf <<EOF
[global]
fsid = ${FSID}
mon_initial_members = ${MON_NAME}
mon_host = ${MON_IP}
public_network = 0.0.0.0/0
cluster_network = 0.0.0.0/0

auth_cluster_required = cephx
auth_service_required = cephx
auth_client_required = cephx

osd_pool_default_size = 1
osd_pool_default_min_size = 1
osd_pool_default_pg_num = 8
osd_pool_default_pgp_num = 8
osd_crush_chooseleaf_type = 0

mon_allow_pool_size_one = true
mon_warn_on_pool_no_redundancy = false
mon_osd_full_ratio = 0.95
mon_osd_nearfull_ratio = 0.90

osd_objectstore = memstore
memstore_device_bytes = ${MEMSTORE_BYTES}

[mon.${MON_NAME}]
mon_addr = ${MON_IP}:6789
host = ${MON_NAME}
EOF
  chown ceph:ceph /etc/ceph/ceph.conf
fi

if [ ! -f "${MON_DATA}/keyring" ]; then
  echo "bootstrap: mon keyring"
  ceph-authtool --create-keyring /var/lib/ceph/tmp/mon-keyring --gen-key -n mon. --cap mon 'allow *'
  ceph-authtool --create-keyring /etc/ceph/ceph.client.admin.keyring --gen-key -n client.admin \
      --cap mon 'allow *' --cap osd 'allow *' --cap mds 'allow *' --cap mgr 'allow *'
  chown ceph:ceph /etc/ceph/ceph.client.admin.keyring
  chmod 0644 /etc/ceph/ceph.client.admin.keyring
  ceph-authtool /var/lib/ceph/tmp/mon-keyring --import-keyring /etc/ceph/ceph.client.admin.keyring

  FSID="$(awk -F'= ' '/^fsid/ {print $2}' /etc/ceph/ceph.conf)"
  monmaptool --create --add "${MON_NAME}" "${MON_IP}" --fsid "${FSID}" /var/lib/ceph/tmp/monmap

  mkdir -p "${MON_DATA}"
  chown -R ceph:ceph "${MON_DATA}" /var/lib/ceph/tmp
  sudo -u ceph ceph-mon --mkfs -i "${MON_NAME}" \
      --monmap /var/lib/ceph/tmp/monmap \
      --keyring /var/lib/ceph/tmp/mon-keyring
fi

echo "bootstrap: starting mon"
sudo -u ceph ceph-mon -i "${MON_NAME}" -f --public-addr "${MON_IP}" &
MON_PID=$!

for i in $(seq 1 60); do
  if ceph -s >/dev/null 2>&1; then
    echo "bootstrap: mon responsive after ${i}s"
    break
  fi
  sleep 1
done

if ! ceph osd dump 2>/dev/null | grep -qE "^osd\.${OSD_ID} "; then
  echo "bootstrap: creating osd.${OSD_ID}"
  OSD_UUID="$(uuidgen)"
  NEW_OSD_ID="$(ceph osd new "${OSD_UUID}")"
  if [ "${NEW_OSD_ID}" != "${OSD_ID}" ]; then
    OSD_ID="${NEW_OSD_ID}"
    OSD_DATA="/var/lib/ceph/osd/ceph-${OSD_ID}"
  fi
  mkdir -p "${OSD_DATA}"
  chown -R ceph:ceph "${OSD_DATA}"

  sudo -u ceph ceph-authtool --create-keyring "${OSD_DATA}/keyring" --gen-key -n "osd.${OSD_ID}" \
      --cap mon 'allow profile osd' --cap osd 'allow *' --cap mgr 'allow profile osd'
  ceph auth import -i "${OSD_DATA}/keyring"

  sudo -u ceph ceph-osd -i "${OSD_ID}" --mkfs --osd-uuid "${OSD_UUID}"

  ceph osd crush add-bucket "${MON_NAME}" host || true
  ceph osd crush move "${MON_NAME}" root=default || true
  ceph osd crush add "osd.${OSD_ID}" 1.0 "host=${MON_NAME}" || true
fi

echo "bootstrap: starting osd.${OSD_ID}"
sudo -u ceph ceph-osd -i "${OSD_ID}" -f &
OSD_PID=$!

if [ ! -f "${MGR_DATA}/keyring" ]; then
  echo "bootstrap: creating mgr.${MGR_NAME}"
  mkdir -p "${MGR_DATA}"
  chown -R ceph:ceph "${MGR_DATA}"
  ceph auth get-or-create "mgr.${MGR_NAME}" mon 'allow profile mgr' osd 'allow *' mds 'allow *' \
      > "${MGR_DATA}/keyring"
  chown ceph:ceph "${MGR_DATA}/keyring"
fi

echo "bootstrap: starting mgr"
sudo -u ceph ceph-mgr -i "${MGR_NAME}" -f &
MGR_PID=$!

for i in $(seq 1 60); do
  h="$(ceph health 2>/dev/null || true)"
  case "${h}" in
    HEALTH_OK*|HEALTH_WARN*)
      echo "bootstrap: cluster ${h} after ${i}s"
      break
      ;;
  esac
  sleep 2
done

for pool in ${STRATA_POOL} ${STRATA_EXTRA_POOLS}; do
  if [ -z "${pool}" ]; then continue; fi
  if ! ceph osd pool ls 2>/dev/null | grep -q "^${pool}\$"; then
    ceph osd pool create "${pool}" 8 8 replicated
    ceph osd pool application enable "${pool}" rgw
    echo "bootstrap: pool ${pool} created"
  fi
done

# Quiet two single-mon lab quirks that otherwise trip HEALTH_WARN on every
# fresh boot and trigger Strata's storage-degraded banner:
#  - AUTH_INSECURE_GLOBAL_ID_RECLAIM_ALLOWED: lab single-mon default; safe to
#    flip off because clients in this stack always present new-style cephx.
#  - MON_MSGR2_NOT_ENABLED: monmap was initialised with v1 only; enable v2.
ceph config set mon auth_allow_insecure_global_id_reclaim false 2>/dev/null || true
ceph mon enable-msgr2 2>/dev/null || true

# CI memory tuning (deploy/docker/docker-compose.ci.yml). Both knobs default
# to unset on the developer stack so behaviour matches the upstream image;
# the CI override sets them explicitly to fit ubuntu-latest's 7 GB budget.
if [ -n "${OSD_MEMORY_TARGET:-}" ]; then
  echo "bootstrap: setting osd_memory_target=${OSD_MEMORY_TARGET}"
  ceph config set osd osd_memory_target "${OSD_MEMORY_TARGET}" 2>/dev/null || true
fi
if [ "${MGR_DASHBOARD_DISABLE:-0}" = "1" ]; then
  echo "bootstrap: disabling mgr dashboard module"
  ceph mgr module disable dashboard 2>/dev/null || true
fi

echo "bootstrap: cluster ready"
ceph -s

trap 'kill -TERM ${MON_PID} ${OSD_PID} ${MGR_PID} 2>/dev/null; wait' INT TERM
wait
