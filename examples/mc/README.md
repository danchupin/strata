# mc (MinIO Client) examples

`mc` works against any S3-compatible endpoint. These scripts use the
alias `strata-examples` registered against `$STRATA_ENDPOINT`.

## Scripts

| script | what it does |
| --- | --- |
| `01-bucket-setup.sh` | `mc mb` / `mc ls` / `mc rb` |
| `02-multipart.sh` | 70 MiB upload (mc auto-multiparts above ~64 MiB), md5 round-trip |
| `03-presign.sh` | `mc share download` -> curl |
| `04-lifecycle.sh` | `mc ilm import` reads `lifecycle.json` |
| `05-replication.sh` | `mc replicate import` reads `replication.json` (JSON config preserved verbatim) |
| `06-sse-s3.sh` | `mc cp --enc-s3` then `mc stat --json` to read the echoed algorithm |
| `07-rotate-key.sh` | mc admin user APIs are MinIO-specific; this script falls through to `aws-cli/07-rotate-key.sh` |

## Notes

- `mc replicate add` requires admin endpoints to register a remote target.
  Strata does not implement those, so we use `mc replicate import` which
  is a raw `PutBucketReplication` proxy.
- `mc admin user` commands target a MinIO server with admin access; they
  are not part of S3. Use the AWS-shaped IAM verbs (the `aws-cli`
  examples) to manage Strata principals.
