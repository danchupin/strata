package cassandra

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gocql/gocql"
)

var tableDDL = []string{
	`CREATE TYPE IF NOT EXISTS objectlock_cfg (
		enabled     boolean,
		mode        text,
		retain_days int
	)`,
	`CREATE TABLE IF NOT EXISTS buckets (
		name          text PRIMARY KEY,
		id            uuid,
		owner_id      text,
		created_at    timestamp,
		versioning    text,
		default_class text,
		shard_count   int,
		objectlock    frozen<objectlock_cfg>,
		tags          map<text, text>,
		policy        text,
		acl           text
	)`,
	`CREATE TABLE IF NOT EXISTS objects (
		bucket_id        uuid,
		shard            int,
		key              text,
		version_id       timeuuid,
		is_latest        boolean,
		is_delete_marker boolean,
		size             bigint,
		etag             text,
		content_type     text,
		storage_class    text,
		mtime            timestamp,
		manifest         blob,
		user_meta        map<text, text>,
		tags             map<text, text>,
		retain_until     timestamp,
		legal_hold       boolean,
		PRIMARY KEY ((bucket_id, shard), key, version_id)
	) WITH CLUSTERING ORDER BY (key ASC, version_id DESC)
	  AND compaction = {
	    'class': 'LeveledCompactionStrategy',
	    'tombstone_threshold': '0.1',
	    'unchecked_tombstone_compaction': 'true'
	  }`,
	`CREATE TABLE IF NOT EXISTS multipart_uploads (
		bucket_id     uuid,
		upload_id     timeuuid,
		key           text,
		status        text,
		storage_class text,
		content_type  text,
		initiated_at  timestamp,
		PRIMARY KEY ((bucket_id), upload_id)
	)`,
	`CREATE TABLE IF NOT EXISTS multipart_parts (
		bucket_id   uuid,
		upload_id   timeuuid,
		part_number int,
		etag        text,
		size        bigint,
		mtime       timestamp,
		manifest    blob,
		PRIMARY KEY ((bucket_id, upload_id), part_number)
	)`,
	`CREATE TABLE IF NOT EXISTS bucket_lifecycle (
		bucket_id uuid PRIMARY KEY,
		rules     blob
	)`,
	`CREATE TABLE IF NOT EXISTS bucket_cors (
		bucket_id uuid PRIMARY KEY,
		rules     blob
	)`,
	`CREATE TABLE IF NOT EXISTS bucket_policy (
		bucket_id uuid PRIMARY KEY,
		document  blob
	)`,
	`CREATE TABLE IF NOT EXISTS bucket_public_access_block (
		bucket_id uuid PRIMARY KEY,
		config    blob
	)`,
	`CREATE TABLE IF NOT EXISTS bucket_ownership_controls (
		bucket_id uuid PRIMARY KEY,
		config    blob
	)`,
	`CREATE TABLE IF NOT EXISTS bucket_acl_grants (
		bucket_id uuid PRIMARY KEY,
		grants    blob
	)`,
	`CREATE TABLE IF NOT EXISTS gc_queue (
		region       text,
		enqueued_at  timestamp,
		oid          text,
		pool         text,
		cluster      text,
		namespace    text,
		PRIMARY KEY ((region), enqueued_at, oid)
	)`,
	// gc_entries_v2 partitions the GC queue across 1024 logical shards
	// (US-002 / Phase 2 sharded leader-election). The on-disk shard fan-out
	// is fixed at 1024; runtime readers (STRATA_GC_SHARDS) take a subset of
	// these partitions via `shard_id % shardCount == myShard`. Dual-write
	// window: writers stamp both gc_queue (legacy) and gc_entries_v2 until
	// STRATA_GC_DUAL_WRITE=off is flipped at the operator's discretion.
	`CREATE TABLE IF NOT EXISTS gc_entries_v2 (
		region       text,
		shard_id     int,
		enqueued_at  timestamp,
		oid          text,
		pool         text,
		cluster      text,
		namespace    text,
		PRIMARY KEY ((region, shard_id), enqueued_at, oid)
	)`,
	`CREATE TABLE IF NOT EXISTS worker_locks (
		name   text PRIMARY KEY,
		holder text
	)`,
	`CREATE TABLE IF NOT EXISTS cluster_nodes (
		node_id        text PRIMARY KEY,
		address        text,
		version        text,
		started_at     timestamp,
		workers        set<text>,
		leader_for     set<text>,
		last_heartbeat timestamp
	) WITH default_time_to_live = 30`,
	`CREATE TABLE IF NOT EXISTS access_keys (
		access_key text PRIMARY KEY,
		secret_key text,
		owner      text,
		disabled   boolean,
		created_at timestamp
	)`,
	`CREATE TABLE IF NOT EXISTS iam_users (
		user_name  text PRIMARY KEY,
		user_id    text,
		path       text,
		created_at timestamp
	)`,
	`CREATE TABLE IF NOT EXISTS iam_access_keys_by_user (
		user_name  text,
		access_key text,
		PRIMARY KEY (user_name, access_key)
	)`,
	`CREATE TABLE IF NOT EXISTS bucket_encryption (
		bucket_id uuid PRIMARY KEY,
		config    blob
	)`,
	`CREATE TABLE IF NOT EXISTS bucket_object_lock (
		bucket_id uuid PRIMARY KEY,
		config    blob
	)`,
	`CREATE TABLE IF NOT EXISTS bucket_notification (
		bucket_id uuid PRIMARY KEY,
		config    blob
	)`,
	`CREATE TABLE IF NOT EXISTS bucket_website (
		bucket_id uuid PRIMARY KEY,
		config    blob
	)`,
	`CREATE TABLE IF NOT EXISTS bucket_replication (
		bucket_id uuid PRIMARY KEY,
		config    blob
	)`,
	`CREATE TABLE IF NOT EXISTS bucket_logging (
		bucket_id uuid PRIMARY KEY,
		config    blob
	)`,
	`CREATE TABLE IF NOT EXISTS bucket_tagging (
		bucket_id uuid PRIMARY KEY,
		config    blob
	)`,
	`CREATE TABLE IF NOT EXISTS bucket_quota (
		bucket_id uuid PRIMARY KEY,
		config    blob
	)`,
	`CREATE TABLE IF NOT EXISTS bucket_placement (
		bucket_id uuid PRIMARY KEY,
		policy    blob
	)`,
	`CREATE TABLE IF NOT EXISTS cluster_state (
		cluster_id text PRIMARY KEY,
		state      text
	)`,
	`CREATE TABLE IF NOT EXISTS user_quota (
		user_name text PRIMARY KEY,
		config    blob
	)`,
	`CREATE TABLE IF NOT EXISTS bucket_stats (
		bucket_id    uuid PRIMARY KEY,
		used_bytes   bigint,
		used_objects bigint,
		updated_at   timestamp
	)`,
	`CREATE TABLE IF NOT EXISTS usage_aggregates (
		bucket_id        uuid,
		storage_class    text,
		day              date,
		byte_seconds     bigint,
		object_count_avg bigint,
		object_count_max bigint,
		computed_at      timestamp,
		PRIMARY KEY ((bucket_id, storage_class), day)
	) WITH CLUSTERING ORDER BY (day ASC)`,
	`CREATE TABLE IF NOT EXISTS usage_aggregates_classes (
		bucket_id     uuid,
		storage_class text,
		PRIMARY KEY (bucket_id, storage_class)
	)`,
	`CREATE TABLE IF NOT EXISTS multipart_completions (
		bucket_id    uuid,
		upload_id    timeuuid,
		key          text,
		etag         text,
		version_id   text,
		body         blob,
		headers      map<text, text>,
		completed_at timestamp,
		PRIMARY KEY ((bucket_id), upload_id)
	)`,
	`CREATE TABLE IF NOT EXISTS rewrap_progress (
		bucket_id  uuid PRIMARY KEY,
		target_id  text,
		last_key   text,
		complete   boolean,
		updated_at timestamp
	)`,
	`CREATE TABLE IF NOT EXISTS notify_queue (
		bucket_id    uuid,
		hour         timestamp,
		event_id     timeuuid,
		bucket_name  text,
		object_key   text,
		event_name   text,
		event_time   timestamp,
		config_id    text,
		target_type  text,
		target_arn   text,
		payload      blob,
		PRIMARY KEY ((bucket_id, hour), event_id)
	)`,
	`CREATE TABLE IF NOT EXISTS replication_queue (
		bucket_id          uuid,
		day                timestamp,
		event_id           timeuuid,
		bucket_name        text,
		object_key         text,
		version_id         text,
		event_name         text,
		event_time         timestamp,
		rule_id            text,
		destination_bucket text,
		storage_class      text,
		PRIMARY KEY ((bucket_id, day), event_id)
	)`,
	`CREATE TABLE IF NOT EXISTS access_log_buffer (
		bucket_id      uuid,
		hour           timestamp,
		event_id       timeuuid,
		ts             timestamp,
		request_id     text,
		principal      text,
		source_ip      text,
		op             text,
		object_key     text,
		status         int,
		bytes_sent     bigint,
		object_size    bigint,
		total_time_ms  int,
		turn_around_ms int,
		referrer       text,
		user_agent     text,
		version_id     text,
		PRIMARY KEY ((bucket_id, hour), event_id)
	)`,
	`CREATE TABLE IF NOT EXISTS audit_log (
		bucket_id    uuid,
		day          timestamp,
		event_id     timeuuid,
		ts           timestamp,
		principal    text,
		action       text,
		resource     text,
		result       text,
		request_id   text,
		source_ip    text,
		bucket_name  text,
		user_agent   text,
		PRIMARY KEY ((bucket_id, day), event_id)
	) WITH CLUSTERING ORDER BY (event_id DESC)`,
	`CREATE TABLE IF NOT EXISTS bucket_inventory_configs (
		bucket_id uuid,
		config_id text,
		config    blob,
		PRIMARY KEY ((bucket_id), config_id)
	)`,
	`CREATE TABLE IF NOT EXISTS access_points (
		name                text PRIMARY KEY,
		bucket_id           uuid,
		bucket              text,
		alias               text,
		network_origin      text,
		vpc_id              text,
		policy              blob,
		public_access_block blob,
		created_at          timestamp
	)`,
	`CREATE TABLE IF NOT EXISTS reshard_jobs (
		bucket_id   uuid PRIMARY KEY,
		bucket_name text,
		source      int,
		target      int,
		last_key    text,
		done        boolean,
		created_at  timestamp,
		updated_at  timestamp
	)`,
	`CREATE TABLE IF NOT EXISTS admin_jobs (
		id           text PRIMARY KEY,
		kind         text,
		bucket       text,
		state        text,
		message      text,
		deleted      bigint,
		started_at   timestamp,
		updated_at   timestamp,
		finished_at  timestamp
	)`,
	`CREATE TABLE IF NOT EXISTS iam_managed_policies (
		arn         text PRIMARY KEY,
		name        text,
		path        text,
		description text,
		document    blob,
		created_at  timestamp,
		updated_at  timestamp
	)`,
	`CREATE TABLE IF NOT EXISTS iam_user_policies (
		user_name   text,
		policy_arn  text,
		attached_at timestamp,
		PRIMARY KEY (user_name, policy_arn)
	)`,
	`CREATE TABLE IF NOT EXISTS iam_policy_attachments (
		policy_arn  text,
		user_name   text,
		attached_at timestamp,
		PRIMARY KEY (policy_arn, user_name)
	)`,
	`CREATE TABLE IF NOT EXISTS notify_dlq (
		bucket_id    uuid,
		day          timestamp,
		event_id     timeuuid,
		bucket_name  text,
		object_key   text,
		event_name   text,
		event_time   timestamp,
		config_id    text,
		target_type  text,
		target_arn   text,
		payload      blob,
		attempts     int,
		reason       text,
		enqueued_at  timestamp,
		PRIMARY KEY ((bucket_id, day), event_id)
	)`,
}

var alterStatements = []string{
	`ALTER TABLE objects ADD retain_mode text`,
	`ALTER TABLE buckets ADD acl text`,
	`ALTER TABLE objects ADD grants blob`,
	`ALTER TABLE access_keys ADD user_name text`,
	`ALTER TABLE objects ADD checksums map<text, text>`,
	`ALTER TABLE multipart_parts ADD checksums map<text, text>`,
	`ALTER TABLE objects ADD sse text`,
	`ALTER TABLE multipart_uploads ADD sse text`,
	`ALTER TABLE objects ADD ssec_key_md5 text`,
	`ALTER TABLE objects ADD restore_status text`,
	`ALTER TABLE buckets ADD object_lock_enabled boolean`,
	`ALTER TABLE buckets ADD region text`,
	`ALTER TABLE buckets ADD mfa_delete text`,
	`ALTER TABLE objects ADD cache_control text`,
	`ALTER TABLE objects ADD expires text`,
	`ALTER TABLE objects ADD parts_count int`,
	`ALTER TABLE multipart_uploads ADD user_meta map<text, text>`,
	`ALTER TABLE multipart_uploads ADD cache_control text`,
	`ALTER TABLE multipart_uploads ADD expires text`,
	`ALTER TABLE multipart_uploads ADD checksum_algorithm text`,
	`ALTER TABLE objects ADD sse_key blob`,
	`ALTER TABLE objects ADD sse_key_id text`,
	`ALTER TABLE multipart_uploads ADD sse_key blob`,
	`ALTER TABLE multipart_uploads ADD sse_key_id text`,
	`ALTER TABLE objects ADD replication_status text`,
	`ALTER TABLE replication_queue ADD destination_endpoint text`,
	`ALTER TABLE objects ADD part_sizes list<bigint>`,
	`ALTER TABLE objects ADD checksum_type text`,
	`ALTER TABLE multipart_uploads ADD checksum_type text`,
	`ALTER TABLE objects ADD is_null boolean`,
	`ALTER TABLE gc_queue ADD cluster text`,
	`ALTER TABLE gc_queue ADD namespace text`,
	`ALTER TABLE buckets ADD shard_count_target int`,
	`ALTER TABLE buckets ADD backend_presign boolean`,
	`ALTER TABLE audit_log ADD user_agent text`,
	`ALTER TABLE audit_log ADD total_time_ms int`,
	`ALTER TABLE cluster_state ADD mode text`,
}

func isColumnAlreadyExists(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "already exists") ||
		strings.Contains(msg, "conflicts with an existing column") ||
		strings.Contains(msg, "Invalid column name")
}

func ensureKeyspace(cfg SessionConfig) error {
	s, err := connectNoKeyspace(cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	q := fmt.Sprintf(
		"CREATE KEYSPACE IF NOT EXISTS %s WITH replication = %s AND durable_writes = true",
		cfg.Keyspace, cfg.Replication,
	)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	return s.Query(q).WithContext(ctx).Exec()
}

func ensureTables(s *gocql.Session, timeout time.Duration) error {
	for _, stmt := range tableDDL {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := s.Query(stmt).WithContext(ctx).Exec()
		cancel()
		if err != nil {
			return fmt.Errorf("ddl: %w\n%s", err, stmt)
		}
	}
	for _, stmt := range alterStatements {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		err := s.Query(stmt).WithContext(ctx).Exec()
		cancel()
		if err != nil && !isColumnAlreadyExists(err) {
			return fmt.Errorf("alter: %w\n%s", err, stmt)
		}
	}
	return nil
}
