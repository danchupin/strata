#!/usr/bin/env bash
# `s3cmd setlifecycle FILE s3://BUCKET` uploads an XML config.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/_config.sh"
trap 'rm -f "$S3CFG"' EXIT

BUCKET="ex-lc-$(strata_suffix)"
$S3CMD mb "s3://$BUCKET"

$S3CMD setlifecycle "$HERE/lifecycle.xml" "s3://$BUCKET"
$S3CMD getlifecycle "s3://$BUCKET" | grep -q "expire-logs-after-30d"
$S3CMD dellifecycle "s3://$BUCKET"

$S3CMD rb "s3://$BUCKET"
echo "OK"
