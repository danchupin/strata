"""PUT with SSE-S3, HEAD echoes algo, GET round-trips plaintext."""
from _common import s3_client, suffix

s3 = s3_client()
bucket = f"ex-sse-{suffix()}"
s3.create_bucket(Bucket=bucket)

body = b"encrypted secret"
s3.put_object(Bucket=bucket, Key="secret.txt", Body=body, ServerSideEncryption="AES256")

head = s3.head_object(Bucket=bucket, Key="secret.txt")
assert head.get("ServerSideEncryption") == "AES256", head
print(f"SSE={head['ServerSideEncryption']}")

got = s3.get_object(Bucket=bucket, Key="secret.txt")["Body"].read()
assert got == body, (got, body)
print("plaintext round-trip ok")

s3.delete_object(Bucket=bucket, Key="secret.txt")
s3.delete_bucket(Bucket=bucket)
print("OK")
