-- Strata metadata schema (draft, Phase 0).
-- Final DDL is owned by internal/meta/schema.go (auto-migration on gateway startup).

CREATE KEYSPACE IF NOT EXISTS strata
    WITH replication = {
        'class': 'NetworkTopologyStrategy',
        'dc1': 3,
        'dc2': 3
    }
    AND durable_writes = true;

USE strata;

CREATE TYPE IF NOT EXISTS objectlock_cfg (
    enabled      boolean,
    mode         text,   -- 'GOVERNANCE' | 'COMPLIANCE'
    retain_days  int
);

CREATE TABLE IF NOT EXISTS buckets (
    name           text PRIMARY KEY,
    id             uuid,
    owner_id       text,
    created_at     timestamp,
    versioning     text,
    default_class  text,
    shard_count    int,
    objectlock     frozen<objectlock_cfg>,
    tags           map<text, text>,
    policy         text
);

CREATE TABLE IF NOT EXISTS objects (
    bucket_id         uuid,
    shard             int,
    key               text,
    version_id        timeuuid,
    is_latest         boolean,
    is_delete_marker  boolean,
    size              bigint,
    etag              text,
    content_type      text,
    storage_class     text,
    mtime             timestamp,
    manifest          blob,
    user_meta         map<text, text>,
    tags              map<text, text>,
    retain_until      timestamp,
    legal_hold        boolean,
    PRIMARY KEY ((bucket_id, shard), key, version_id)
) WITH CLUSTERING ORDER BY (key ASC, version_id DESC)
  AND compaction = {
      'class': 'LeveledCompactionStrategy',
      'tombstone_threshold': '0.1',
      'unchecked_tombstone_compaction': 'true'
  };

CREATE TABLE IF NOT EXISTS object_shard_lookup (
    bucket_id uuid,
    key       text,
    shard     int,
    PRIMARY KEY ((bucket_id, key))
);

CREATE TABLE IF NOT EXISTS multipart_uploads (
    bucket_id      uuid,
    upload_id      timeuuid,
    key            text,
    status         text,         -- 'uploading' | 'completing' | 'aborted'
    storage_class  text,
    content_type   text,
    initiated_at   timestamp,
    PRIMARY KEY ((bucket_id), upload_id)
);

CREATE TABLE IF NOT EXISTS multipart_parts (
    bucket_id    uuid,
    upload_id    timeuuid,
    part_number  int,
    etag         text,
    size         bigint,
    mtime        timestamp,
    manifest     blob,
    PRIMARY KEY ((bucket_id, upload_id), part_number)
);

CREATE TABLE IF NOT EXISTS bucket_lifecycle (
    bucket_id uuid PRIMARY KEY,
    rules     blob
);

CREATE TABLE IF NOT EXISTS transition_queue (
    dc            text,
    bucket_id     uuid,
    key           text,
    version_id    timeuuid,
    target_class  text,
    enqueued_at   timestamp,
    PRIMARY KEY ((dc), bucket_id, key, version_id)
);

CREATE TABLE IF NOT EXISTS gc_queue (
    region       text,
    enqueued_at  timestamp,
    oid          text,
    pool         text,
    cluster      text,
    PRIMARY KEY ((region), enqueued_at, oid)
);

CREATE TABLE IF NOT EXISTS access_keys (
    access_key_id  text PRIMARY KEY,
    secret_key     text,
    owner_id       text,
    created_at     timestamp,
    disabled       boolean
);
