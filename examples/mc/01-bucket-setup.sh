#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/_alias.sh"

BUCKET="ex-bkt-$(strata_suffix)"
mc mb "$MC_ALIAS/$BUCKET"
mc ls "$MC_ALIAS" | grep -q "$BUCKET"
mc rb "$MC_ALIAS/$BUCKET"
echo "OK"
