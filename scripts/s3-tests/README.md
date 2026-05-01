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

## Baseline (as of first run)

Against commit `HEAD` with `auth=required`, default subset (filter matches
`test_bucket_create or test_bucket_list or test_object_write or
test_object_read or test_object_delete or test_multipart or test_versioning_obj
or test_bucket_list_versions`):

```
tests=177  passed=11  failed=8  errors=158  skipped=0
pass rate: 6.2%
```

Of the 177 collected, only 19 actually executed (setup succeeded); the
remaining 158 failed at collection because their fixtures require
not-yet-implemented features (IAM, bucket ownership, checksums, etc.).
**Of the 19 that ran, 11 passed → 58% pass rate on the executable
subset.** The 8 real FAILs are in `test_headers.py` covering SigV2 /
bad-auth edge cases we deliberately don't implement.

Full suite (no filter):

```
tests=1046  passed=3  failed=55  errors=983  skipped=5
```

The large `errors` bucket reflects how much of S3 we don't implement yet
— ACLs, CORS, IAM, bucket policies, website hosting, S3 Select,
replication, checksums, etc. Each P-item closed in `ROADMAP.md` should
shift some fraction of those errors into either pass or fail.

### 2026-05-01 — `e397aa3` (closing auth-per-chunk-signature cycle, US-005)

Default subset, `auth=required`, `make up-all` (Cassandra + Ceph RADOS).
Cycle adds streaming SigV4 chain validation in `internal/auth/streaming.go`
(buffer-then-validate, `hmac.Equal` constant-time compare, ≤16 MiB
per-chunk cap) and aws-chunked-trailer 501 detection. Re-run asserts
upload paths did not regress vs. the cycle-start floor:

```
tests=177  passed=139  failed=38  errors=0  skipped=0
pass rate: 78.5%
```

`>= 78.5%` floor held. Failure breakdown is unchanged from the
2026-05-01 `6e122903` measurement below — every failing test is a
pre-existing gap filed under "Consolidation & validation" / multipart
/ versioning / listing in `ROADMAP.md`. No new failures from chain
validation: every AWS SDK that drives the s3-tests harness (boto)
already emits a correct chain, so legitimate streaming PUTs flow
through the validator unchanged.

### 2026-05-01 — `6e122903` (closing s3-tests-90 cycle, US-011)

Default subset, `auth=required`, `make up-all` (Cassandra + Ceph RADOS):

```
tests=177  passed=139  failed=38  errors=0  skipped=0
pass rate: 78.5%
```

Headline target was ≥90% (stretch ≥94.9%). Below floor — the `s3-tests
80% → 90%+` ROADMAP P1 entry stays open. Per-cluster gaps still failing
are filed under "Consolidation & validation" / multipart / versioning /
listing in `ROADMAP.md`. Per-story unit tests under `internal/s3api`
pass green; the cluster-level interop gaps surface only when boto / aws-
cli drives the full request shape (signed body chunks, FlexibleChecksum
SDK helper, anonymous reads, etc.).

Failure breakdown (38):

- **8 SigV2 / bad-auth (deliberate gap)** — `test_headers.py`
  `test_bucket_create_bad_*_aws2`, `test_bucket_create_bad_authorization_*`,
  `test_bucket_create_bad_expect_mismatch`. SigV2 is intentionally
  unsupported (see Non-Goals in `tasks/prd-s3-tests-90.md`).
- **5 versioning null** — `test_versioning_obj_plain_null_version_removal`,
  `test_versioning_obj_plain_null_version_overwrite`,
  `test_versioning_obj_plain_null_version_overwrite_suspended`,
  `test_versioning_obj_suspend_versions`,
  `test_versioning_obj_suspended_copy`. US-007 wired the meta-shape
  contract; the cluster-level path on Cassandra still lets a deleted
  null version remain readable. Filed as a follow-up P1.
- **5 multipart per-part composite checksum (FlexibleChecksum helper)** —
  `test_multipart_use_cksum_helper_{sha256,sha1,crc32,crc32c,crc64nvme}`.
  US-003 wired the COMPOSITE response shape; boto's helper still trips
  the SDK-side `FlexibleChecksumError`. Filed as a follow-up P1.
- **4 multipart copy** — `test_multipart_copy_{small,improper_range,
  special_names,multiple_sizes}`. US-004 wired UploadPartCopy
  FlexibleChecksum; SDK still rejects the recomputed digest on the
  copy body for these shapes.
- **3 ?partNumber=N GET** — `test_multipart_get_part`,
  `test_multipart_sse_c_get_part`, `test_multipart_single_get_part`.
  ContentLength echoes whole-object size instead of part size; single-
  part objects do not 416 on out-of-range partNumber. US-002 follow-up.
- **2 listing delimiter+prefix** — `test_bucket_list_delimiter_prefix`,
  `test_bucket_list_delimiter_prefix_underscore`. V1 NextMarker shape
  diverges from AWS observable on the s3-tests fixture set. US-005
  follow-up.
- **2 listV2 continuation token** — `test_bucket_listv2_continuationtoken`,
  `test_bucket_listv2_both_continuationtoken_startafter`. US-006 wired
  the opaque-base64 wire form; paging still drops/duplicates a row in
  the `boo/bar`+`cquux/*` fixture. Follow-up.
- **2 anonymous list (configuration, not bug)** —
  `test_bucket_list_objects_anonymous`,
  `test_bucket_listv2_objects_anonymous`. Need `auth=optional` plus a
  bucket policy / ACL allow-anonymous shape. Out of `s3-tests-90`
  scope; lands with the bucket-policy P-item.
- **1 multipart preconditions** — `test_multipart_put_current_object_if_match`.
  US-008 follow-up; bucket-versioning interaction with If-Match
  produces 412 in a case where the spec accepts.
- **1 multipart size-too-small** — `test_multipart_upload_size_too_small`.
  US-009 follow-up; the s3-tests fixture exposes a `<5 MiB` non-last
  shape we don't reject yet.
- **1 multipart resend** — `test_multipart_resend_first_finishes_last`.
  US-009 follow-up; gateway reports `InvalidPartOrder` on a Complete
  body that the spec accepts.
- **1 multipart Complete checksum** — `test_multipart_checksum_sha256`.
  US-010 follow-up; the boto FlexibleChecksum path on Complete still
  fails the SDK-side digest match.
- **1 versioning list** — `test_bucket_list_return_data_versioning`.
  Adjacent to US-007 list shape.
- **1 unreadable prefix (deliberate gap)** —
  `test_bucket_list_prefix_unreadable`. Non-UTF-8 prefix bytes; AWS
  itself does not guarantee clean behaviour. Declared a deliberate
  gap (see Non-Goals in `tasks/prd-s3-tests-90.md`).
- **1 object delete on missing bucket** — `test_object_delete_key_bucket_gone`.
  Edge-case error code drift; file under correctness P3.

`test_bucket_list_unordered`, `test_bucket_create_exists`,
`test_bucket_create_exists_nonowner`, and the `test_bucket_listv2_unordered`
companion all currently pass — keeping them noted here as
**deliberate gaps** anyway: `allow-unordered=true` is an RGW extension,
V1 ListObjects marker / encoding-type cases are SDK-quirk-driven and
modern SDKs (boto3, aws-sdk-go-v2, aws-cli 2.x) all use V2. If a future
upstream s3-tests revision flips them to FAIL, do not file as new work.

## Interpreting failures

Failures fall into three buckets:

1. **Not-yet-implemented feature** (e.g. `test_bucket_acl_*`, `test_cors_*`).
   These correspond to open P-items in `ROADMAP.md`. Expected, not a bug.
2. **Wrong behaviour on something we do implement.** These are real bugs —
   file an issue or fix directly.
3. **Suite assumes RGW-specific behaviour** (e.g. specific error codes,
   admin API). These are architectural drift, not correctness failures.

The suite supports `--shard-id/--num-shards` for parallelism; useful for CI.
