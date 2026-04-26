# S3 compatibility suite

This directory wires Strata up against [Ceph's `s3-tests`][1] — the de-facto
compatibility test suite for any S3-like object gateway. Running it gives an
honest, numeric compatibility score against a broad spectrum of S3 behaviour.

[1]: https://github.com/ceph/s3-tests

## What it covers

The suite has hundreds of tests over bucket ops, object ops, ACLs, versioning,
multipart, object lock, lifecycle, CORS, website hosting, replication, and
more. Many of these cover S3 surface we do not yet implement — those will
appear as failures in the report and map back to P-items in `ROADMAP.md`.

## Prereqs

- Full Strata stack running (`make up-all`), gateway with `auth=required`.
- Python 3.9+, `git`, `pip`.
- Credentials mounted into the gateway via `STRATA_STATIC_CREDENTIALS`. The
  runner prints the exact string to use.

## Run

```bash
# Pick two + one tenant creds that the suite expects.
export MAIN_AK=testMainAK MAIN_SK=testMainSK
export ALT_AK=testAltAK   ALT_SK=testAltSK
export TENANT_AK=testTenantAK TENANT_SK=testTenantSK

# Start the gateway with those creds.
STRATA_AUTH_MODE=required \
  STRATA_STATIC_CREDENTIALS="$MAIN_AK:$MAIN_SK:main-owner,$ALT_AK:$ALT_SK:alt-owner,$TENANT_AK:$TENANT_SK:tenant-owner" \
  docker compose -f deploy/docker/docker-compose.yml up -d --force-recreate gateway

# Run the default subset (what Strata actually implements today).
scripts/s3-tests/run.sh

# Or run the full suite.
S3_TESTS_FILTER=all scripts/s3-tests/run.sh

# Or pick a specific test pattern.
S3_TESTS_FILTER=test_multipart scripts/s3-tests/run.sh
```

## Output

- `report/pytest.log` — full pytest output.
- `report/junit.xml` — machine-readable results, parsed at the end into a
  one-line summary.
- A `pass rate: X%` line on stdout that is the number you cite when claiming
  "Strata is N% S3-compatible".

## Baseline

History below — newest run on top. The default subset filter is the
`DEFAULT_FILTER` in `run.sh`:
`test_bucket_create or test_bucket_list or test_object_write or
test_object_read or test_object_delete or test_multipart or
test_versioning_obj or test_bucket_list_versions`.

### 2026-04-26 — commit `b6aca17`

After US-001..US-027 shipped, fixtures that previously errored at
collection now resolve (IAM users, owners, checksums, bucket policy,
ACLs, SSE, object lock, website, replication, tagging, region, MFA
delete, anonymous identity, multipart idempotency). Default subset:

```
tests=176  passed=114  failed=62  errors=0  skipped=0
pass rate: 64.8%
```

Executable subset grew from 19 → 176 (>9×) as the ROADMAP P-items
closed. Headline pass rate climbed from 58% → 64.8% on a much larger
sample. **No regressions vs the original 19-test subset:** the 11 that
passed before still pass, and the 8 `test_headers.py` failures are the
same deliberate SigV2 gaps.

**Remaining failure clusters** (62 total):

- **`test_headers.py` — 8 failures.** SigV2 / bad-auth edge cases. Same
  as the original baseline: deliberately not implemented.
- **List-objects shape — ~22 failures**
  (`test_bucket_list_marker_*`, `test_bucket_list_return_data*`,
  `test_bucket_listv2_fetchowner_*`, `test_bucket_list_delimiter_prefix*`,
  `test_bucket_list_encoding_basic`, `test_bucket_list_maxkeys_*`,
  `test_bucket_list_unordered`, `test_bucket_listv2_continuationtoken*`,
  `test_bucket_list_objects_anonymous`,
  `test_bucket_listv2_objects_anonymous`). Missing fields (`<Owner>` per
  entry, `<Marker>`/`<NextMarker>` echo on v1 responses, `EncodingType`)
  and pagination/encoding edge cases. ListObjects is currently V2-shape
  only; v1 is served through the same handler.
- **Multipart edge cases — ~15 failures** (`test_multipart_upload`,
  `test_multipart_upload_resend_part`, `test_multipart_upload_size_too_small`,
  `test_multipart_*_get_part`, `test_multipart_copy_*`,
  `test_multipart_use_cksum_helper_*`, `test_multipart_put_object_if_*`,
  `test_multipart_resend_first_finishes_last`,
  `test_multipart_checksum_sha256`). User metadata
  (`x-amz-meta-*`) supplied to `InitiateMultipartUpload` is not
  preserved on the resulting object; multipart copy needs richer
  source-range handling; `GetPart` needs `partNumber` query support;
  conditional `If-Match`/`If-None-Match` on multipart Complete needs
  hooking through the LWT path.
- **Versioning — 5 failures**
  (`test_versioning_obj_plain_null_version_*`,
  `test_versioning_obj_suspend_versions`,
  `test_versioning_obj_suspended_copy`). Suspend/null-version semantics:
  on suspend, an unversioned overwrite should remove all "null"-versioned
  ancestors atomically.
- **Object header echo — 2 failures**
  (`test_object_write_cache_control`, `test_object_write_expires`).
  `Cache-Control` and `Expires` request headers are not persisted on PUT
  and not echoed on GET/HEAD.
- **Misc — 5 failures** (`test_bucket_create_exists`,
  `test_bucket_create_exists_nonowner`,
  `test_object_delete_key_bucket_gone`, `test_object_read_unreadable`,
  `test_bucket_list_many`). CreateBucket idempotency on owner-equality,
  DELETE on missing-bucket error code, unreadable-key 400 path.

### 2026-04-12 — initial baseline

```
tests=177  passed=11  failed=8  errors=158  skipped=0
pass rate: 6.2%
```

Of the 177 collected, only 19 actually executed (setup succeeded); the
remaining 158 failed at collection because their fixtures required
not-yet-implemented features (IAM, bucket ownership, checksums, etc.).
**Of the 19 that ran, 11 passed → 58% pass rate on the executable
subset.** The 8 real FAILs were in `test_headers.py` covering SigV2 /
bad-auth edge cases we deliberately don't implement.

Full suite (no filter), original baseline:

```
tests=1046  passed=3  failed=55  errors=983  skipped=5
```

The large `errors` bucket reflected how much of S3 we did not implement
yet — ACLs, CORS, IAM, bucket policies, website hosting, S3 Select,
replication, checksums, etc. Each P-item closed in `ROADMAP.md` shifts
some fraction of those errors into either pass or fail.

## Interpreting failures

Failures fall into three buckets:

1. **Not-yet-implemented feature** (e.g. `test_bucket_acl_*`, `test_cors_*`).
   These correspond to open P-items in `ROADMAP.md`. Expected, not a bug.
2. **Wrong behaviour on something we do implement.** These are real bugs —
   file an issue or fix directly.
3. **Suite assumes RGW-specific behaviour** (e.g. specific error codes,
   admin API). These are architectural drift, not correctness failures.

The suite supports `--shard-id/--num-shards` for parallelism; useful for CI.
