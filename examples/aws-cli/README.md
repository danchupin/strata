# aws-cli examples

Each script is self-contained. Source `examples/lib/common.sh` for the
endpoint URL / credentials. Defaults work against `examples/smoke.sh`'s
freshly-booted in-memory gateway:

| var | default |
| --- | --- |
| `STRATA_ENDPOINT` | `http://127.0.0.1:9999` |
| `STRATA_REGION` | `us-east-1` |
| `STRATA_ACCESS_KEY` | `adminAccessKey` |
| `STRATA_SECRET_KEY` | `adminSecretKey` |

Override any of these to target a different deployment.

## Scripts

| script | what it does |
| --- | --- |
| `01-bucket-setup.sh` | create-bucket / list / head / delete |
| `02-multipart.sh` | 25 MiB upload via aws-cli's automatic multipart, md5 round-trip |
| `03-presign.sh` | `aws s3 presign` GET URL, fetched via plain curl with no creds |
| `04-lifecycle.sh` | put + get + delete a lifecycle config (`lifecycle.json`) |
| `05-replication.sh` | enable versioning, put + get + delete a replication config (`replication.json`) |
| `06-sse-s3.sh` | PUT with `--server-side-encryption AES256`, HEAD echoes the algo, GET round-trips plaintext |
| `07-rotate-key.sh` | create-user, create-access-key, RotateAccessKey via curl --aws-sigv4, verify old rejected + new works |

## Running

```
# Boot a fresh in-memory gateway and run every example end-to-end:
bash examples/smoke.sh

# Or, if a Strata gateway is already running with the credentials shown above:
bash examples/aws-cli/01-bucket-setup.sh
```

## Notes

- `RotateAccessKey` is a Strata-only verb. aws-cli does not know it, so the
  script calls `?Action=RotateAccessKey` directly via `curl --aws-sigv4`.
- SSE-S3 requires the gateway to have a master key configured
  (`STRATA_SSE_MASTER_KEY`). `examples/smoke.sh` generates one at boot.
- Replication PUT requires versioning enabled on the source bucket. The
  `strata-replicator` worker drains the queue against the destination
  endpoint configured in its env.
