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
	`CREATE TABLE IF NOT EXISTS worker_locks (
		name   text PRIMARY KEY,
		holder text
	)`,
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
		PRIMARY KEY ((bucket_id, day), event_id)
	) WITH CLUSTERING ORDER BY (event_id DESC)`,
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
