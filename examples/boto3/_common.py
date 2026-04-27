"""Shared helpers for the boto3 examples."""
import os
import time

import boto3
from botocore.config import Config

ENDPOINT = os.environ.get("STRATA_ENDPOINT", "http://127.0.0.1:9999")
REGION = os.environ.get("STRATA_REGION", "us-east-1")
ACCESS_KEY = os.environ.get("STRATA_ACCESS_KEY", "adminAccessKey")
SECRET_KEY = os.environ.get("STRATA_SECRET_KEY", "adminSecretKey")


def s3_client(access_key: str | None = None, secret_key: str | None = None):
    return boto3.client(
        "s3",
        endpoint_url=ENDPOINT,
        region_name=REGION,
        aws_access_key_id=access_key or ACCESS_KEY,
        aws_secret_access_key=secret_key or SECRET_KEY,
        config=Config(signature_version="s3v4", s3={"addressing_style": "path"}),
    )


def iam_client():
    return boto3.client(
        "iam",
        endpoint_url=ENDPOINT,
        region_name=REGION,
        aws_access_key_id=ACCESS_KEY,
        aws_secret_access_key=SECRET_KEY,
    )


def suffix() -> str:
    return str(time.time_ns())[-9:]
