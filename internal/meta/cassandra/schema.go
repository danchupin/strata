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
}

var alterStatements = []string{
	`ALTER TABLE objects ADD retain_mode text`,
	`ALTER TABLE buckets ADD acl text`,
	// US-010: backend multipart pass-through tracking. Strata's multipart
	// session is mapped 1:1 onto the backend's own multipart upload; the
	// backend SDK upload-id is recorded on the parent row and the backend's
	// per-part ETag on each child row.
	`ALTER TABLE multipart_uploads ADD backend_upload_id text`,
	`ALTER TABLE multipart_parts ADD backend_etag text`,
	// US-016: per-bucket toggle for backend presigned-URL passthrough.
	// Default false (NULL on legacy rows decodes as false); when true,
	// authenticated presigned GETs at this bucket get a 307 redirect to a
	// backend-credentialled URL.
	`ALTER TABLE buckets ADD backend_presign boolean`,
	// US-s3-tests-90 US-003: FlexibleChecksum carries on multipart. Algo
	// declared at Initiate, per-part digest captured at UploadPart, type
	// (COMPOSITE/FULL_OBJECT) preserved across the upload so the
	// CompleteMultipartUploadResult and HEAD/GET responses can echo the
	// composite checksum the client expects.
	`ALTER TABLE multipart_uploads ADD checksum_algorithm text`,
	`ALTER TABLE multipart_uploads ADD checksum_type text`,
	`ALTER TABLE multipart_parts ADD checksum_value text`,
	`ALTER TABLE multipart_parts ADD checksum_algorithm text`,
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
