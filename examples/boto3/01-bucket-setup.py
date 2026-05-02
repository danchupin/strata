"""Create a bucket, list, head, delete."""
from _common import s3_client, suffix

s3 = s3_client()
bucket = f"ex-bkt-{suffix()}"

s3.create_bucket(Bucket=bucket)
print(f"created {bucket}")

names = [b["Name"] for b in s3.list_buckets()["Buckets"]]
assert bucket in names, names

s3.head_bucket(Bucket=bucket)
print("head ok")

s3.delete_bucket(Bucket=bucket)
print("OK")
