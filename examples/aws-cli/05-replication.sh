#!/usr/bin/env bash
# Configure cross-bucket replication. Replication PUT requires versioning
# enabled on the source bucket. The destination is referenced by ARN; the
# `replicator` worker (run via `strata server --workers=replicator`) drains
# the queue.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/../lib/common.sh"

SUFFIX="$(strata_suffix)"
SRC="ex-rsrc-$SUFFIX"
DST="ex-rdst-$SUFFIX"

aws_strata s3api create-bucket --bucket "$SRC" >/dev/null
aws_strata s3api create-bucket --bucket "$DST" >/dev/null

echo "== aws-cli: enable versioning on source"
aws_strata s3api put-bucket-versioning --bucket "$SRC" \
    --versioning-configuration Status=Enabled

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
sed "s|__DEST__|$DST|" "$HERE/replication.json" > "$TMP/repl.json"

echo "== aws-cli: put-bucket-replication"
aws_strata s3api put-bucket-replication --bucket "$SRC" \
    --replication-configuration "file://$TMP/repl.json"

echo "== aws-cli: get-bucket-replication"
aws_strata s3api get-bucket-replication --bucket "$SRC"

echo "== aws-cli: delete-bucket-replication"
aws_strata s3api delete-bucket-replication --bucket "$SRC"

aws_strata s3api delete-bucket --bucket "$SRC"
aws_strata s3api delete-bucket --bucket "$DST"
echo "OK"
