#!/usr/bin/env bash
# Smoke-tests that Grafana provisioning loaded the Strata dashboard via the
# /api/dashboards/uid/<uid> endpoint. Assumes `make up-all` is running.
set -euo pipefail

GRAFANA_URL="${GRAFANA_URL:-http://localhost:3000}"
GRAFANA_USER="${GRAFANA_USER:-admin}"
GRAFANA_PASS="${GRAFANA_PASS:-admin}"
DASHBOARD_UID="${DASHBOARD_UID:-strata-overview}"

echo "waiting for grafana at $GRAFANA_URL..."
for i in $(seq 1 60); do
  if curl -fsS "$GRAFANA_URL/api/health" >/dev/null 2>&1; then
    break
  fi
  sleep 2
done

echo "fetching dashboard $DASHBOARD_UID..."
body=$(curl -fsS -u "$GRAFANA_USER:$GRAFANA_PASS" \
  "$GRAFANA_URL/api/dashboards/uid/$DASHBOARD_UID")

# Validate JSON parses + carries our title.
echo "$body" | python3 -c '
import json, sys
d = json.load(sys.stdin)
title = d.get("dashboard", {}).get("title")
assert title == "Strata Overview", f"unexpected title: {title!r}"
panels = d.get("dashboard", {}).get("panels", [])
assert len(panels) > 0, "no panels in dashboard"
print(f"OK: {title} with {len(panels)} panels")
'

# Sanity-check the datasource is wired.
echo "checking prometheus datasource..."
ds=$(curl -fsS -u "$GRAFANA_USER:$GRAFANA_PASS" \
  "$GRAFANA_URL/api/datasources/name/Prometheus")
echo "$ds" | python3 -c '
import json, sys
d = json.load(sys.stdin)
assert d.get("type") == "prometheus", d
print(f"OK: {d.get(\"name\")} -> {d.get(\"url\")}")
'

echo "grafana smoke OK"
