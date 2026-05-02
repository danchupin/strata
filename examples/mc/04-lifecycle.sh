#!/usr/bin/env bash
# `mc ilm` is the lifecycle CLI. Use `import` for the canonical XML/JSON shape.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/_alias.sh"

BUCKET="ex-lc-$(strata_suffix)"
mc mb "$MC_ALIAS/$BUCKET"

mc ilm import "$MC_ALIAS/$BUCKET" < "$HERE/lifecycle.json"
mc ilm ls "$MC_ALIAS/$BUCKET" | grep -q "expire-logs-after-30d"
mc ilm rule remove --id expire-logs-after-30d "$MC_ALIAS/$BUCKET"

mc rb "$MC_ALIAS/$BUCKET"
echo "OK"
