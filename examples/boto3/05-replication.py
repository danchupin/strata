"""Configure cross-bucket replication. Setup only (replicator worker actually runs the queue)."""
from _common import s3_client, suffix

s3 = s3_client()
sx = suffix()
src = f"ex-rsrc-{sx}"
dst = f"ex-rdst-{sx}"

s3.create_bucket(Bucket=src)
s3.create_bucket(Bucket=dst)

s3.put_bucket_versioning(Bucket=src, VersioningConfiguration={"Status": "Enabled"})

cfg = {
    "Role": "arn:aws:iam::123456789012:role/strata-replicator",
    "Rules": [
        {
            "ID": "logs",
            "Priority": 1,
            "Status": "Enabled",
            "Filter": {"Prefix": "logs/"},
            "DeleteMarkerReplication": {"Status": "Disabled"},
            "Destination": {"Bucket": f"arn:aws:s3:::{dst}"},
        }
    ],
}

s3.put_bucket_replication(Bucket=src, ReplicationConfiguration=cfg)
got = s3.get_bucket_replication(Bucket=src)["ReplicationConfiguration"]
assert got["Rules"][0]["Destination"]["Bucket"] == f"arn:aws:s3:::{dst}", got
print(f"replicated {src} -> {dst}")

s3.delete_bucket_replication(Bucket=src)
s3.delete_bucket(Bucket=src)
s3.delete_bucket(Bucket=dst)
print("OK")
