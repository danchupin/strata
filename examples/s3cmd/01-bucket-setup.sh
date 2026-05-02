#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/_config.sh"
trap 'rm -f "$S3CFG"' EXIT

BUCKET="ex-bkt-$(strata_suffix)"
$S3CMD mb "s3://$BUCKET"
$S3CMD ls | grep -q "$BUCKET"
$S3CMD info "s3://$BUCKET" >/dev/null
$S3CMD rb "s3://$BUCKET"
echo "OK"
