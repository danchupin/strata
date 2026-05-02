#!/usr/bin/env bash
# `mc replicate add` requires registering a remote target via admin APIs that
# Strata does not implement. Use `mc replicate import` instead, which sends a
# raw PutBucketReplication payload that Strata accepts.
#
# `mc replicate ls` calls admin endpoints to enrich each rule with remote
# target metadata, which Strata also doesn't implement, so we use aws-cli
# (or curl) to verify and clean up the replication config.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/_alias.sh"

SUFFIX="$(strata_suffix)"
SRC="ex-rsrc-$SUFFIX"
DST="ex-rdst-$SUFFIX"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

mc mb "$MC_ALIAS/$SRC"
mc mb "$MC_ALIAS/$DST"
mc version enable "$MC_ALIAS/$SRC"

sed "s|__DEST__|$DST|" "$HERE/replication.json" > "$TMP/repl.json"
mc replicate import "$MC_ALIAS/$SRC" < "$TMP/repl.json"

# Verify via aws-cli (mc admin endpoints are MinIO-only).
aws_strata s3api get-bucket-replication --bucket "$SRC" \
    --query 'ReplicationConfiguration.Rules[0].Destination.Bucket' --output text \
    | grep -q "$DST"

aws_strata s3api delete-bucket-replication --bucket "$SRC"
mc rb "$MC_ALIAS/$SRC"
mc rb "$MC_ALIAS/$DST"
echo "OK"
