package cassandra

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gocql/gocql"
	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// Store is the Cassandra-backed meta.Store. It deliberately does NOT
// implement the optional meta.RangeScanStore capability (US-012): the
// objects table is partitioned by (bucket_id, shard) so a prefix scan must
// fan out across N shard partitions and heap-merge by clustering order —
// that fan-out IS the implementation in Store.ListObjects below. Hoisting
// it under a "single ordered range scan" name would just rename the same
// code. The gateway's type-assertion at the dispatch site
// (internal/s3api/server.go::listObjects) therefore falls through to
// Store.ListObjects on Cassandra; memory and TiKV provide the
// RangeScanStore-shaped fast path.
type Store struct {
	s            *gocql.Session
	defaultShard int
	keyspace     string

	// gcDualWrite gates fan-out writes to both gc_queue (legacy) and
	// gc_entries_v2 during the Phase 2 cutover (US-002). Reads always
	// drain v2 first and fall back to v1 while either side may carry
	// rows. Flipped to false after STRATA_GC_DUAL_WRITE=off and the
	// legacy table has drained.
	gcDualWrite bool

	// obs is the SlowQueryObserver attached to the gocql session. The Store
	// reuses it as the LWT-conflict sink (US-009): on applied=false from a
	// CAS the Store calls obs.RecordLWTConflict(ctx, table, bucket, shard).
	// nil when no observer was wired (memory-only tests / metrics-disabled).
	obs *SlowQueryObserver

	// bucketNames caches bucketID → name lookups so the LWT-conflict
	// emitter can stamp a human-readable `bucket=<name>` label without an
	// extra round-trip per CAS. Populated as a side-effect of GetBucket /
	// CreateBucket; misses fall back to the UUID string. Eviction is by
	// process restart — bucket renames don't happen.
	bucketNamesMu sync.RWMutex
	bucketNames   map[uuid.UUID]string

	// metaHealthMu guards the 10s in-process cache used by MetaHealth so
	// adminapi pollers (storage page TanStack Query 30s + Overview hero
	// 60s + degraded banner 30s) don't re-issue system.peers / system.local
	// queries on every hit.
	metaHealthMu     sync.Mutex
	metaHealthCache  *meta.MetaHealthReport
	metaHealthExpiry time.Time
}

type Options struct {
	DefaultShardCount int
	// GCDualWrite controls whether the GC queue is dual-written to the
	// legacy `gc_queue` table alongside `gc_entries_v2` (US-002 Phase 2
	// cutover). Defaults to GCDualWriteFromEnv() when zero-valued via
	// the option struct (callers that want a hard override pass an
	// explicit *bool via WithGCDualWrite).
	GCDualWrite *bool
}

// WithGCDualWrite is a small helper for tests / callers that want to set the
// optional pointer cleanly: `cassandra.Options{..., GCDualWrite: cassandra.WithGCDualWrite(false)}`.
func WithGCDualWrite(v bool) *bool { return &v }

func Open(cfg SessionConfig, opts Options) (*Store, error) {
	if err := ensureKeyspace(cfg); err != nil {
		return nil, err
	}
	s, err := connect(cfg)
	if err != nil {
		return nil, err
	}
	if err := ensureTables(s, cfg.Timeout); err != nil {
		s.Close()
		return nil, err
	}
	if opts.DefaultShardCount <= 0 {
		opts.DefaultShardCount = 64
	}
	gcDualWrite := GCDualWriteFromEnv()
	if opts.GCDualWrite != nil {
		gcDualWrite = *opts.GCDualWrite
	}
	store := &Store{
		s:            s,
		defaultShard: opts.DefaultShardCount,
		keyspace:     cfg.Keyspace,
		bucketNames:  make(map[uuid.UUID]string),
		gcDualWrite:  gcDualWrite,
	}
	store.obs = NewQueryObserver(cfg.Logger, time.Duration(cfg.SlowMS)*time.Millisecond, cfg.Metrics, cfg.Tracer)
	return store, nil
}

func (s *Store) Close() error {
	s.s.Close()
	return nil
}

// Session exposes the underlying gocql session for auxiliary subsystems
// (leader election, admin tooling) that need direct CQL access.
func (s *Store) Session() *gocql.Session { return s.s }

// Probe runs a lightweight `SELECT now() FROM system.local` to confirm the
// gocql session is still attached to a live coordinator. Used by the gateway
// /readyz endpoint; honours ctx cancellation.
func (s *Store) Probe(ctx context.Context) error {
	if s == nil || s.s == nil || s.s.Closed() {
		return errors.New("cassandra session closed")
	}
	var t time.Time
	return s.s.Query("SELECT now() FROM system.local").WithContext(ctx).Scan(&t)
}

func shardOf(key string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(n))
}

func (s *Store) CreateBucket(ctx context.Context, name, owner, defaultClass string) (*meta.Bucket, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	b := &meta.Bucket{
		Name:         name,
		ID:           id,
		Owner:        owner,
		CreatedAt:    time.Now().UTC(),
		DefaultClass: defaultClass,
		Versioning:   meta.VersioningDisabled,
	}
	existing := make(map[string]interface{})
	applied, err := s.s.Query(
		`INSERT INTO buckets (name, id, owner_id, created_at, versioning, default_class, shard_count, region)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?) IF NOT EXISTS`,
		name, gocqlUUID(id), owner, b.CreatedAt, meta.VersioningDisabled, defaultClass, s.defaultShard, "",
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).MapScanCAS(existing)
	if err != nil {
		return nil, err
	}
	if !applied {
		return nil, meta.ErrBucketAlreadyExists
	}
	s.cacheBucketName(id, name)
	return b, nil
}

func (s *Store) GetBucket(ctx context.Context, name string) (*meta.Bucket, error) {
	var (
		idG               gocql.UUID
		owner, class      string
		versioning, acl   string
		region            string
		mfaDelete         string
		createdAt         time.Time
		shardCount        int
		shardCountTarget  int
		objectLockEnabled bool
	)
	err := s.s.Query(
		`SELECT id, owner_id, created_at, default_class, versioning, shard_count, shard_count_target, acl, object_lock_enabled, region, mfa_delete FROM buckets WHERE name=?`,
		name,
	).WithContext(ctx).Scan(&idG, &owner, &createdAt, &class, &versioning, &shardCount, &shardCountTarget, &acl, &objectLockEnabled, &region, &mfaDelete)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrBucketNotFound
	}
	if err != nil {
		return nil, err
	}
	if versioning == "" {
		versioning = meta.VersioningDisabled
	}
	bID := uuidFromGocql(idG)
	s.cacheBucketName(bID, name)
	return &meta.Bucket{
		Name:              name,
		ID:                bID,
		Owner:             owner,
		CreatedAt:         createdAt,
		DefaultClass:      class,
		Versioning:        versioning,
		ACL:               acl,
		ObjectLockEnabled: objectLockEnabled,
		Region:            region,
		MfaDelete:         mfaDelete,
		ShardCount:        shardCount,
		TargetShardCount:  shardCountTarget,
	}, nil
}

// cacheBucketName populates the bucketID→name cache used by the LWT-conflict
// emitter (US-009). Safe to call from any path that already knows the
// (id, name) pair; misses fall back to the UUID string at lookup time.
func (s *Store) cacheBucketName(id uuid.UUID, name string) {
	if name == "" {
		return
	}
	s.bucketNamesMu.Lock()
	if s.bucketNames == nil {
		s.bucketNames = make(map[uuid.UUID]string)
	}
	s.bucketNames[id] = name
	s.bucketNamesMu.Unlock()
}

// bucketLabelForLWT returns the operator-readable bucket label used on the
// strata_cassandra_lwt_conflicts_total counter. Cache-hit returns the name;
// miss falls back to the bucket-id UUID string so the metric never silently
// drops a conflict on a cold cache.
func (s *Store) bucketLabelForLWT(id uuid.UUID) string {
	s.bucketNamesMu.RLock()
	name := s.bucketNames[id]
	s.bucketNamesMu.RUnlock()
	if name != "" {
		return name
	}
	return id.String()
}

// recordLWTConflict fans out to the SlowQueryObserver-attached metrics sink
// when set. Used by every LWT call site on the `objects` table that observes
// applied=false. No-op when the observer or its metrics sink is nil
// (memory-only tests / metrics-disabled builds).
func (s *Store) recordLWTConflict(ctx context.Context, table string, bucketID uuid.UUID, shard int) {
	if s.obs == nil || s.obs.Metrics == nil {
		return
	}
	s.obs.RecordLWTConflict(ctx, table, s.bucketLabelForLWT(bucketID), strconv.Itoa(shard))
}

func (s *Store) SetBucketVersioning(ctx context.Context, name, state string) error {
	// Use LWT (`IF EXISTS`) so the write participates in the same Paxos lineage
	// as `CreateBucket` (`INSERT ... IF NOT EXISTS`). Without this, a non-LWT
	// UPDATE can be applied to the base row but leave the Paxos state stale,
	// causing subsequent LOCAL_QUORUM reads to observe the pre-update value
	// until Paxos resynchronises.
	applied, err := s.s.Query(
		`UPDATE buckets SET versioning=? WHERE name=? IF EXISTS`,
		state, name,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(nil)
	if err != nil {
		return err
	}
	if !applied {
		return meta.ErrBucketNotFound
	}
	return nil
}

func (s *Store) SetBucketGrants(ctx context.Context, bucketID uuid.UUID, grants []meta.Grant) error {
	blob, err := encodeGrants(grants)
	if err != nil {
		return err
	}
	return s.setBucketBlob(ctx, "bucket_acl_grants", "grants", bucketID, blob)
}

func (s *Store) GetBucketGrants(ctx context.Context, bucketID uuid.UUID) ([]meta.Grant, error) {
	blob, err := s.getBucketBlob(ctx, "bucket_acl_grants", "grants", bucketID, meta.ErrNoSuchGrants)
	if err != nil {
		return nil, err
	}
	return decodeGrants(blob)
}

func (s *Store) DeleteBucketGrants(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, "bucket_acl_grants", bucketID)
}

func (s *Store) SetObjectGrants(ctx context.Context, bucketID uuid.UUID, key, versionID string, grants []meta.Grant) error {
	v, shard, err := s.resolveVersionID(ctx, bucketID, key, versionID)
	if err != nil {
		return err
	}
	blob, err := encodeGrants(grants)
	if err != nil {
		return err
	}
	return s.s.Query(
		`UPDATE objects SET grants=? WHERE bucket_id=? AND shard=? AND key=? AND version_id=?`,
		blob, gocqlUUID(bucketID), shard, key, v,
	).WithContext(ctx).Exec()
}

func (s *Store) GetObjectGrants(ctx context.Context, bucketID uuid.UUID, key, versionID string) ([]meta.Grant, error) {
	v, shard, err := s.resolveVersionID(ctx, bucketID, key, versionID)
	if err != nil {
		return nil, err
	}
	var blob []byte
	err = s.s.Query(
		`SELECT grants FROM objects WHERE bucket_id=? AND shard=? AND key=? AND version_id=?`,
		gocqlUUID(bucketID), shard, key, v,
	).WithContext(ctx).Scan(&blob)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrObjectNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(blob) == 0 {
		return nil, meta.ErrNoSuchGrants
	}
	return decodeGrants(blob)
}

func (s *Store) SetBucketACL(ctx context.Context, name, canned string) error {
	applied, err := s.s.Query(
		`UPDATE buckets SET acl=? WHERE name=? IF EXISTS`,
		canned, name,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(nil)
	if err != nil {
		return err
	}
	if !applied {
		return meta.ErrBucketNotFound
	}
	return nil
}

func (s *Store) DeleteBucket(ctx context.Context, name string) error {
	b, err := s.GetBucket(ctx, name)
	if err != nil {
		return err
	}
	empty, err := s.bucketIsEmpty(ctx, b.ID, s.defaultShard)
	if err != nil {
		return err
	}
	if !empty {
		return meta.ErrBucketNotEmpty
	}
	return s.s.Query(`DELETE FROM buckets WHERE name=?`, name).WithContext(ctx).Exec()
}

func (s *Store) bucketIsEmpty(ctx context.Context, bucketID uuid.UUID, shardCount int) (bool, error) {
	for shard := range shardCount {
		var key string
		err := s.s.Query(
			`SELECT key FROM objects WHERE bucket_id=? AND shard=? LIMIT 1`,
			gocqlUUID(bucketID), shard,
		).WithContext(ctx).Scan(&key)
		if err == nil {
			return false, nil
		}
		if !errors.Is(err, gocql.ErrNotFound) {
			return false, err
		}
	}
	return true, nil
}

func (s *Store) ListBuckets(ctx context.Context, owner string) ([]*meta.Bucket, error) {
	iter := s.s.Query(`SELECT name, id, owner_id, created_at, default_class, versioning, shard_count, shard_count_target, acl, region, mfa_delete FROM buckets`).
		WithContext(ctx).Iter()
	defer iter.Close()

	var (
		out                                                      []*meta.Bucket
		name, ownerID, class, versioning, acl, region, mfaDelete string
		idG                                                      gocql.UUID
		createdAt                                                time.Time
		shardCount, shardCountTarget                             int
	)
	for iter.Scan(&name, &idG, &ownerID, &createdAt, &class, &versioning, &shardCount, &shardCountTarget, &acl, &region, &mfaDelete) {
		if owner != "" && ownerID != owner {
			continue
		}
		if versioning == "" {
			versioning = meta.VersioningDisabled
		}
		bID := uuidFromGocql(idG)
		s.cacheBucketName(bID, name)
		out = append(out, &meta.Bucket{
			Name:             name,
			ID:               bID,
			Owner:            ownerID,
			CreatedAt:        createdAt,
			DefaultClass:     class,
			Versioning:       versioning,
			ACL:              acl,
			Region:           region,
			MfaDelete:        mfaDelete,
			ShardCount:       shardCount,
			TargetShardCount: shardCountTarget,
		})
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) PutObject(ctx context.Context, o *meta.Object, versioned bool) error {
	manifestBlob, err := encodeManifest(o.Manifest)
	if err != nil {
		return err
	}
	shard := shardOf(o.Key, s.defaultShard)
	var versionID gocql.UUID
	if !versioned {
		o.VersionID = meta.NullVersionID
		o.IsNull = true
	} else if o.IsNull {
		o.VersionID = meta.NullVersionID
	}
	if o.VersionID != "" {
		parsed, err := versionToCQL(o.VersionID)
		if err != nil {
			return err
		}
		versionID = parsed
	} else {
		versionID = gocql.TimeUUID()
		o.VersionID = versionID.String()
	}

	// Compute (deltaBytes, deltaObjects) for the bucket_stats bump that
	// follows the object write. Read prior row BEFORE issuing the DELETE
	// so the delta accounts for the replaced bytes. Delete markers
	// contribute 0 bytes and 0 to object count — only non-marker rows count.
	var prior *meta.Object
	if !versioned {
		p, err := s.scanObjectLimit1(ctx, o.BucketID, shard, o.Key)
		if err != nil && !errors.Is(err, meta.ErrObjectNotFound) {
			return err
		}
		prior = p
		if err := s.s.Query(
			`DELETE FROM objects WHERE bucket_id=? AND shard=? AND key=?`,
			gocqlUUID(o.BucketID), shard, o.Key,
		).WithContext(ctx).Exec(); err != nil {
			return err
		}
	} else if o.IsNull {
		// Suspended-mode null PUT: atomically drop any prior null-versioned
		// row (LWT IF EXISTS — applied=false is fine, no prior null row).
		nullUUID, _ := versionToCQL(meta.NullVersionID)
		p, err := s.scanObjectByVersion(ctx, o.BucketID, shard, o.Key, nullUUID)
		if err != nil && !errors.Is(err, meta.ErrObjectNotFound) {
			return err
		}
		prior = p
		if err := s.s.Query(
			`DELETE FROM objects WHERE bucket_id=? AND shard=? AND key=? AND version_id=? IF EXISTS`,
			gocqlUUID(o.BucketID), shard, o.Key, nullUUID,
		).WithContext(ctx).Exec(); err != nil {
			return err
		}
	}
	deltaBytes, deltaObjects := bucketStatsDelta(prior, o)
	var retainUntil *time.Time
	if !o.RetainUntil.IsZero() {
		t := o.RetainUntil
		retainUntil = &t
	}
	var partsCount interface{}
	if o.PartsCount > 0 {
		partsCount = o.PartsCount
	}
	var partSizes interface{}
	if len(o.PartSizes) > 0 {
		partSizes = o.PartSizes
	}
	if err := s.s.Query(
		`INSERT INTO objects (bucket_id, shard, key, version_id, is_latest, is_delete_marker,
		 size, etag, content_type, storage_class, mtime, manifest, user_meta, tags,
		 retain_until, retain_mode, legal_hold, checksums, sse, ssec_key_md5, restore_status,
		 cache_control, expires, parts_count, sse_key, sse_key_id, replication_status, part_sizes, checksum_type, is_null)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		gocqlUUID(o.BucketID), shard, o.Key, versionID, true, o.IsDeleteMarker,
		o.Size, o.ETag, o.ContentType, o.StorageClass, o.Mtime, manifestBlob, o.UserMeta, o.Tags,
		retainUntil, nilIfEmpty(o.RetainMode), o.LegalHold, o.Checksums, nilIfEmpty(o.SSE), nilIfEmpty(o.SSECKeyMD5), nilIfEmpty(o.RestoreStatus),
		nilIfEmpty(o.CacheControl), nilIfEmpty(o.Expires), partsCount, nilIfEmptyBytes(o.SSEKey), nilIfEmpty(o.SSEKeyID), nilIfEmpty(o.ReplicationStatus), partSizes, nilIfEmpty(o.ChecksumType), o.IsNull,
	).WithContext(ctx).Exec(); err != nil {
		return err
	}
	if deltaBytes != 0 || deltaObjects != 0 {
		if _, err := s.BumpBucketStats(ctx, o.BucketID, deltaBytes, deltaObjects); err != nil {
			return err
		}
	}
	return nil
}

// bucketStatsDelta returns the (deltaBytes, deltaObjects) bump that should be
// applied to bucket_stats when `next` replaces `prior` (nil prior = fresh row).
// Delete markers contribute 0 bytes and 0 to object count.
func bucketStatsDelta(prior, next *meta.Object) (int64, int64) {
	var priorBytes, priorObjects int64
	if prior != nil {
		priorBytes = prior.Size
		if !prior.IsDeleteMarker {
			priorObjects = 1
		}
	}
	var nextBytes, nextObjects int64
	if next != nil {
		nextBytes = next.Size
		if !next.IsDeleteMarker {
			nextObjects = 1
		}
	}
	return nextBytes - priorBytes, nextObjects - priorObjects
}

func nilIfEmptyBytes(b []byte) interface{} {
	if len(b) == 0 {
		return nil
	}
	return b
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func (s *Store) GetObject(ctx context.Context, bucketID uuid.UUID, key, versionID string) (*meta.Object, error) {
	versionID = meta.ResolveVersionID(versionID)
	shard := shardOf(key, s.defaultShard)
	if versionID == "" {
		// "Latest" with versioning history requires comparing the highest-
		// clustering row against the null sentinel by mtime: a Suspended PUT
		// writes a v1-zero null-sentinel row that sorts LAST in version_id DESC,
		// so the LIMIT-1 query alone returns a stale prior real-timeuuid row
		// even when the null row is the most recent write. (US-005 suspended-copy.)
		latestByVersion, errA := s.scanObjectLimit1(ctx, bucketID, shard, key)
		if errA != nil && !errors.Is(errA, meta.ErrObjectNotFound) {
			return nil, errA
		}
		nullUUID, _ := versionToCQL(meta.NullVersionID)
		nullRow, errB := s.scanObjectByVersion(ctx, bucketID, shard, key, nullUUID)
		if errB != nil && !errors.Is(errB, meta.ErrObjectNotFound) {
			return nil, errB
		}
		var winner *meta.Object
		switch {
		case latestByVersion != nil && nullRow != nil:
			if !latestByVersion.IsNull && nullRow.Mtime.After(latestByVersion.Mtime) {
				winner = nullRow
			} else {
				winner = latestByVersion
			}
		case latestByVersion != nil:
			winner = latestByVersion
		case nullRow != nil:
			winner = nullRow
		default:
			return nil, meta.ErrObjectNotFound
		}
		if winner.IsDeleteMarker {
			return nil, meta.ErrObjectNotFound
		}
		return winner, nil
	}
	vUUID, perr := versionToCQL(versionID)
	if perr != nil {
		return nil, meta.ErrObjectNotFound
	}
	return s.scanObjectByVersion(ctx, bucketID, shard, key, vUUID)
}

func (s *Store) scanObjectLimit1(ctx context.Context, bucketID uuid.UUID, shard int, key string) (*meta.Object, error) {
	q := s.s.Query(
		`SELECT version_id, is_latest, is_delete_marker, size, etag, content_type,
		        storage_class, mtime, manifest, user_meta, tags,
		        retain_until, retain_mode, legal_hold, checksums, sse, ssec_key_md5, restore_status,
		        cache_control, expires, parts_count, sse_key, sse_key_id, replication_status, part_sizes, checksum_type, is_null
		 FROM objects WHERE bucket_id=? AND shard=? AND key=? LIMIT 1`,
		gocqlUUID(bucketID), shard, key,
	).WithContext(ctx)
	return scanObjectQuery(q, bucketID, key)
}

func (s *Store) scanObjectByVersion(ctx context.Context, bucketID uuid.UUID, shard int, key string, vUUID gocql.UUID) (*meta.Object, error) {
	q := s.s.Query(
		`SELECT version_id, is_latest, is_delete_marker, size, etag, content_type,
		        storage_class, mtime, manifest, user_meta, tags,
		        retain_until, retain_mode, legal_hold, checksums, sse, ssec_key_md5, restore_status,
		        cache_control, expires, parts_count, sse_key, sse_key_id, replication_status, part_sizes, checksum_type, is_null
		 FROM objects WHERE bucket_id=? AND shard=? AND key=? AND version_id=?`,
		gocqlUUID(bucketID), shard, key, vUUID,
	).WithContext(ctx)
	return scanObjectQuery(q, bucketID, key)
}

func scanObjectQuery(q *gocql.Query, bucketID uuid.UUID, key string) (*meta.Object, error) {
	var (
		versionUUID  gocql.UUID
		isLatest     bool
		isDeleteMark bool
		size         int64
		etag, ctype  string
		class        string
		mtime        time.Time
		manifestBlob []byte
		userMeta     map[string]string
		tags         map[string]string
		retainUntil  time.Time
		retainMode   string
		legalHold    bool
		checksums    map[string]string
		sse          string
		ssecKeyMD5   string
		restore      string
		cacheControl string
		expires      string
		partsCount   int
		sseKey       []byte
		sseKeyID     string
		replication  string
		partSizes    []int64
		checksumType string
		isNull       bool
	)
	err := q.Scan(&versionUUID, &isLatest, &isDeleteMark, &size, &etag, &ctype,
		&class, &mtime, &manifestBlob, &userMeta, &tags,
		&retainUntil, &retainMode, &legalHold, &checksums, &sse, &ssecKeyMD5, &restore,
		&cacheControl, &expires, &partsCount, &sseKey, &sseKeyID, &replication, &partSizes, &checksumType, &isNull)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrObjectNotFound
	}
	if err != nil {
		return nil, err
	}
	m, err := decodeManifest(manifestBlob)
	if err != nil {
		return nil, err
	}
	return &meta.Object{
		BucketID:          bucketID,
		Key:               key,
		VersionID:         versionFromCQL(versionUUID),
		IsLatest:          isLatest,
		IsDeleteMarker:    isDeleteMark,
		IsNull:            isNull,
		Size:              size,
		ETag:              etag,
		ContentType:       ctype,
		StorageClass:      class,
		Mtime:             mtime,
		Manifest:          m,
		UserMeta:          userMeta,
		Tags:              tags,
		RetainUntil:       retainUntil,
		RetainMode:        retainMode,
		LegalHold:         legalHold,
		Checksums:         checksums,
		SSE:               sse,
		SSECKeyMD5:        ssecKeyMD5,
		SSEKey:            sseKey,
		SSEKeyID:          sseKeyID,
		RestoreStatus:     restore,
		CacheControl:      cacheControl,
		Expires:           expires,
		PartsCount:        partsCount,
		PartSizes:         partSizes,
		ReplicationStatus: replication,
		ChecksumType:      checksumType,
	}, nil
}

// DeleteObjectNullReplacement implements US-029: a Suspended-mode unversioned
// DELETE atomically removes any prior null-versioned row (LWT IF EXISTS) and
// writes a fresh null-versioned delete marker. Other (TimeUUID) versions for
// the key are preserved.
func (s *Store) DeleteObjectNullReplacement(ctx context.Context, bucketID uuid.UUID, key string) (*meta.Object, error) {
	marker := &meta.Object{
		BucketID:       bucketID,
		Key:            key,
		VersionID:      meta.NullVersionID,
		IsLatest:       true,
		IsDeleteMarker: true,
		IsNull:         true,
		Mtime:          time.Now().UTC(),
	}
	if err := s.PutObject(ctx, marker, true); err != nil {
		return nil, err
	}
	return marker, nil
}

func (s *Store) DeleteObject(ctx context.Context, bucketID uuid.UUID, key, versionID string, versioned bool) (*meta.Object, error) {
	versionID = meta.ResolveVersionID(versionID)
	shard := shardOf(key, s.defaultShard)

	if versionID != "" {
		vUUID, err := versionToCQL(versionID)
		if err != nil {
			return nil, meta.ErrObjectNotFound
		}
		o, err := s.GetObject(ctx, bucketID, key, versionID)
		if err != nil {
			return nil, err
		}
		if err := s.s.Query(
			`DELETE FROM objects WHERE bucket_id=? AND shard=? AND key=? AND version_id=?`,
			gocqlUUID(bucketID), shard, key, vUUID,
		).WithContext(ctx).Exec(); err != nil {
			return nil, err
		}
		db, dc := bucketStatsDelta(o, nil)
		if db != 0 || dc != 0 {
			if _, err := s.BumpBucketStats(ctx, bucketID, db, dc); err != nil {
				return nil, err
			}
		}
		return o, nil
	}

	if versioned {
		marker := &meta.Object{
			BucketID:       bucketID,
			Key:            key,
			VersionID:      gocql.TimeUUID().String(),
			IsLatest:       true,
			IsDeleteMarker: true,
			Mtime:          time.Now().UTC(),
		}
		if err := s.PutObject(ctx, marker, true); err != nil {
			return nil, err
		}
		return marker, nil
	}

	o, err := s.GetObject(ctx, bucketID, key, "")
	if errors.Is(err, meta.ErrObjectNotFound) {
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if err := s.s.Query(
		`DELETE FROM objects WHERE bucket_id=? AND shard=? AND key=?`,
		gocqlUUID(bucketID), shard, key,
	).WithContext(ctx).Exec(); err != nil {
		return nil, err
	}
	db, dc := bucketStatsDelta(o, nil)
	if db != 0 || dc != 0 {
		if _, err := s.BumpBucketStats(ctx, bucketID, db, dc); err != nil {
			return nil, err
		}
	}
	return o, nil
}

func (s *Store) ListObjects(ctx context.Context, bucketID uuid.UUID, opts meta.ListOptions) (*meta.ListResult, error) {
	limit := opts.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	shardCount := s.defaultShard

	cursors := make([]*shardCursor, 0, shardCount)
	var mu sync.Mutex
	var wg sync.WaitGroup
	errCh := make(chan error, shardCount)

	for shard := range shardCount {
		wg.Add(1)
		go func(shard int) {
			defer wg.Done()
			c, err := s.openShardCursor(ctx, bucketID, shard, opts.Marker, opts.Prefix, limit+1)
			if err != nil {
				errCh <- err
				return
			}
			if c != nil {
				mu.Lock()
				cursors = append(cursors, c)
				mu.Unlock()
			}
		}(shard)
	}
	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		for _, c := range cursors {
			c.close()
		}
		return nil, err
	}

	h := &cursorHeap{}
	heap.Init(h)
	for _, c := range cursors {
		if c.advance() {
			heap.Push(h, c)
		} else {
			c.close()
		}
	}

	res := &meta.ListResult{}
	seenPrefix := make(map[string]struct{})
	var lastKey string

	for h.Len() > 0 {
		top := heap.Pop(h).(*shardCursor)
		obj := top.current
		if top.advance() {
			heap.Push(h, top)
		} else {
			top.close()
		}

		if obj.Key == lastKey {
			continue
		}
		lastKey = obj.Key

		if opts.Prefix != "" && !strings.HasPrefix(obj.Key, opts.Prefix) {
			if obj.Key > opts.Prefix {
				break
			}
			continue
		}
		if opts.Delimiter != "" {
			rest := obj.Key[len(opts.Prefix):]
			if idx := strings.Index(rest, opts.Delimiter); idx >= 0 {
				pfx := opts.Prefix + rest[:idx+len(opts.Delimiter)]
				if _, ok := seenPrefix[pfx]; !ok {
					if len(res.Objects)+len(res.CommonPrefixes) >= limit {
						res.Truncated = true
						res.NextMarker = pfx
						drainHeap(h)
						return res, nil
					}
					seenPrefix[pfx] = struct{}{}
					res.CommonPrefixes = append(res.CommonPrefixes, pfx)
				}
				continue
			}
		}
		if len(res.Objects)+len(res.CommonPrefixes) >= limit {
			res.Truncated = true
			res.NextMarker = obj.Key
			drainHeap(h)
			return res, nil
		}
		res.Objects = append(res.Objects, obj)
	}
	return res, nil
}

func (s *Store) ListObjectVersions(ctx context.Context, bucketID uuid.UUID, opts meta.ListOptions) (*meta.ListVersionsResult, error) {
	limit := opts.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	shardCount := s.defaultShard

	cursors := make([]*versionCursor, 0, shardCount)
	var mu sync.Mutex
	var wg sync.WaitGroup
	errCh := make(chan error, shardCount)

	for shard := range shardCount {
		wg.Add(1)
		go func(shard int) {
			defer wg.Done()
			c, err := s.openVersionCursor(ctx, bucketID, shard, opts.Marker, limit*2)
			if err != nil {
				errCh <- err
				return
			}
			if c != nil {
				mu.Lock()
				cursors = append(cursors, c)
				mu.Unlock()
			}
		}(shard)
	}
	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		for _, c := range cursors {
			c.close()
		}
		return nil, err
	}

	h := &versionHeap{}
	heap.Init(h)
	for _, c := range cursors {
		if c.advance() {
			heap.Push(h, c)
		} else {
			c.close()
		}
	}

	res := &meta.ListVersionsResult{}
	seenPrefix := make(map[string]struct{})
	var lastKey string
	firstVersionForKey := true

	for h.Len() > 0 {
		top := heap.Pop(h).(*versionCursor)
		obj := top.current
		if top.advance() {
			heap.Push(h, top)
		} else {
			top.close()
		}

		if obj.Key != lastKey {
			firstVersionForKey = true
			lastKey = obj.Key
		}

		if opts.Prefix != "" && !strings.HasPrefix(obj.Key, opts.Prefix) {
			if obj.Key > opts.Prefix {
				break
			}
			continue
		}
		if opts.Delimiter != "" {
			rest := obj.Key[len(opts.Prefix):]
			if idx := strings.Index(rest, opts.Delimiter); idx >= 0 {
				pfx := opts.Prefix + rest[:idx+len(opts.Delimiter)]
				if _, ok := seenPrefix[pfx]; !ok {
					seenPrefix[pfx] = struct{}{}
					res.CommonPrefixes = append(res.CommonPrefixes, pfx)
				}
				continue
			}
		}

		obj.IsLatest = firstVersionForKey
		firstVersionForKey = false

		if len(res.Versions) >= limit {
			res.Truncated = true
			res.NextKeyMarker = obj.Key
			res.NextVersionID = obj.VersionID
			drainVersionHeap(h)
			return res, nil
		}
		res.Versions = append(res.Versions, obj)
	}
	return res, nil
}

func drainVersionHeap(h *versionHeap) {
	for h.Len() > 0 {
		c := heap.Pop(h).(*versionCursor)
		c.close()
	}
}

// AllObjectVersions returns every stored object version across every shard
// of the bucket as deep copies, with IsLatest set on the first (highest
// version_id) row per key. Test-only escape hatch for invariant checks
// that need the full picture without paging through ListObjectVersions
// (whose 1000-row hard cap makes it unfit for the race-harness verification
// pass).
func (s *Store) AllObjectVersions(ctx context.Context, bucketID uuid.UUID) ([]*meta.Object, error) {
	out := make([]*meta.Object, 0)
	for shard := 0; shard < s.defaultShard; shard++ {
		iter := s.s.Query(
			`SELECT key, version_id, is_delete_marker, size, etag, content_type,
			        storage_class, mtime, manifest, user_meta, is_null
			 FROM objects WHERE bucket_id=? AND shard=?`,
			gocqlUUID(bucketID), shard,
		).WithContext(ctx).PageSize(2000).Iter()
		for {
			var (
				key          string
				versionID    gocql.UUID
				isDeleteMark bool
				size         int64
				etag, ctype  string
				class        string
				mtime        time.Time
				manifestBlob []byte
				userMeta     map[string]string
				isNull       bool
			)
			if !iter.Scan(&key, &versionID, &isDeleteMark, &size, &etag, &ctype,
				&class, &mtime, &manifestBlob, &userMeta, &isNull) {
				break
			}
			m, _ := decodeManifest(manifestBlob)
			out = append(out, &meta.Object{
				BucketID:       bucketID,
				Key:            key,
				VersionID:      versionFromCQL(versionID),
				IsDeleteMarker: isDeleteMark,
				IsNull:         isNull,
				Size:           size,
				ETag:           etag,
				ContentType:    ctype,
				StorageClass:   class,
				Mtime:          mtime,
				Manifest:       m,
				UserMeta:       userMeta,
			})
		}
		if err := iter.Close(); err != nil {
			return nil, err
		}
	}
	// Sort by (key ASC, version_id DESC) so we can stamp IsLatest on the head
	// of each per-key chain. Cassandra's clustering order delivers within a
	// shard but across shards the merge order is not preserved.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].VersionID > out[j].VersionID
	})
	var lastKey string
	first := true
	for _, o := range out {
		if o.Key != lastKey {
			first = true
			lastKey = o.Key
		}
		o.IsLatest = first
		first = false
	}
	return out, nil
}

// SampleBucketShardStats issues one `SELECT key, version_id, is_delete_marker,
// size FROM objects WHERE bucket_id=? AND shard=?` per shard, picks the latest
// (highest version_id) non-delete-marker row per key, and returns the per-shard
// byte/object totals. Per-partition scan keeps cardinality bounded; no
// ALLOW FILTERING. Used by the bucketstats sampler (US-012).
func (s *Store) SampleBucketShardStats(ctx context.Context, bucketID uuid.UUID, shardCount int) (map[int]meta.ShardStat, error) {
	if shardCount <= 0 {
		shardCount = s.defaultShard
	}
	out := make(map[int]meta.ShardStat, shardCount)
	for shard := 0; shard < shardCount; shard++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		iter := s.s.Query(
			`SELECT key, version_id, is_delete_marker, size
			 FROM objects WHERE bucket_id=? AND shard=?`,
			gocqlUUID(bucketID), shard,
		).WithContext(ctx).PageSize(2000).Iter()
		var (
			key            string
			versionID      gocql.UUID
			isDeleteMarker bool
			size           int64

			lastKey         string
			haveLast        bool
			latestVersion   gocql.UUID
			latestSize      int64
			latestIsDelete  bool
		)
		flush := func() {
			if !haveLast {
				return
			}
			if !latestIsDelete {
				st := out[shard]
				st.Bytes += latestSize
				st.Objects++
				out[shard] = st
			}
			haveLast = false
		}
		for iter.Scan(&key, &versionID, &isDeleteMarker, &size) {
			if !haveLast || key != lastKey {
				flush()
				lastKey = key
				latestVersion = versionID
				latestSize = size
				latestIsDelete = isDeleteMarker
				haveLast = true
				continue
			}
			// Same key — clustering order is version_id DESC so the first row
			// is already the latest. Defensive: keep the higher version_id.
			if versionID.String() > latestVersion.String() {
				latestVersion = versionID
				latestSize = size
				latestIsDelete = isDeleteMarker
			}
		}
		flush()
		if err := iter.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type versionCursor struct {
	iter    *gocql.Iter
	current *meta.Object
	bucket  uuid.UUID
}

func (c *versionCursor) close() {
	if c.iter != nil {
		_ = c.iter.Close()
		c.iter = nil
	}
}

func (c *versionCursor) advance() bool {
	var (
		key          string
		versionID    gocql.UUID
		isDeleteMark bool
		size         int64
		etag, ctype  string
		class        string
		mtime        time.Time
		manifestBlob []byte
		userMeta     map[string]string
		isNull       bool
	)
	if !c.iter.Scan(&key, &versionID, &isDeleteMark, &size, &etag, &ctype,
		&class, &mtime, &manifestBlob, &userMeta, &isNull) {
		return false
	}
	m, _ := decodeManifest(manifestBlob)
	c.current = &meta.Object{
		BucketID:       c.bucket,
		Key:            key,
		VersionID:      versionFromCQL(versionID),
		IsDeleteMarker: isDeleteMark,
		IsNull:         isNull,
		Size:           size,
		ETag:           etag,
		ContentType:    ctype,
		StorageClass:   class,
		Mtime:          mtime,
		Manifest:       m,
		UserMeta:       userMeta,
	}
	return true
}

func (s *Store) openVersionCursor(ctx context.Context, bucketID uuid.UUID, shard int, marker string, pageSize int) (*versionCursor, error) {
	var iter *gocql.Iter
	if marker == "" {
		iter = s.s.Query(
			`SELECT key, version_id, is_delete_marker, size, etag, content_type,
			        storage_class, mtime, manifest, user_meta, is_null
			 FROM objects WHERE bucket_id=? AND shard=?`,
			gocqlUUID(bucketID), shard,
		).WithContext(ctx).PageSize(pageSize).Iter()
	} else {
		iter = s.s.Query(
			`SELECT key, version_id, is_delete_marker, size, etag, content_type,
			        storage_class, mtime, manifest, user_meta, is_null
			 FROM objects WHERE bucket_id=? AND shard=? AND key >= ?`,
			gocqlUUID(bucketID), shard, marker,
		).WithContext(ctx).PageSize(pageSize).Iter()
	}
	return &versionCursor{iter: iter, bucket: bucketID}, nil
}

type versionHeap []*versionCursor

func (h versionHeap) Len() int { return len(h) }
func (h versionHeap) Less(i, j int) bool {
	if h[i].current.Key != h[j].current.Key {
		return h[i].current.Key < h[j].current.Key
	}
	return h[i].current.VersionID > h[j].current.VersionID
}
func (h versionHeap) Swap(i, j int)   { h[i], h[j] = h[j], h[i] }
func (h *versionHeap) Push(x any)     { *h = append(*h, x.(*versionCursor)) }
func (h *versionHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func drainHeap(h *cursorHeap) {
	for h.Len() > 0 {
		c := heap.Pop(h).(*shardCursor)
		c.close()
	}
}

type shardCursor struct {
	iter    *gocql.Iter
	current *meta.Object
	bucket  uuid.UUID
	shard   int
}

func (c *shardCursor) close() {
	if c.iter != nil {
		_ = c.iter.Close()
		c.iter = nil
	}
}

func (c *shardCursor) advance() bool {
	for {
		var (
			key          string
			versionID    gocql.UUID
			isDeleteMark bool
			size         int64
			etag, ctype  string
			class        string
			mtime        time.Time
			manifestBlob []byte
			userMeta     map[string]string
		)
		if !c.iter.Scan(&key, &versionID, &isDeleteMark, &size, &etag, &ctype,
			&class, &mtime, &manifestBlob, &userMeta) {
			return false
		}
		if c.current != nil && key == c.current.Key {
			continue
		}
		if isDeleteMark {
			c.current = &meta.Object{Key: key, IsDeleteMarker: true}
			continue
		}
		m, _ := decodeManifest(manifestBlob)
		c.current = &meta.Object{
			BucketID:     c.bucket,
			Key:          key,
			VersionID:    versionFromCQL(versionID),
			IsLatest:     true,
			Size:         size,
			ETag:         etag,
			ContentType:  ctype,
			StorageClass: class,
			Mtime:        mtime,
			Manifest:     m,
			UserMeta:     userMeta,
		}
		return true
	}
}

func (s *Store) openShardCursor(ctx context.Context, bucketID uuid.UUID, shard int, marker, _ string, pageSize int) (*shardCursor, error) {
	var iter *gocql.Iter
	if marker == "" {
		iter = s.s.Query(
			`SELECT key, version_id, is_delete_marker, size, etag, content_type,
			        storage_class, mtime, manifest, user_meta
			 FROM objects WHERE bucket_id=? AND shard=?`,
			gocqlUUID(bucketID), shard,
		).WithContext(ctx).PageSize(pageSize).Iter()
	} else {
		iter = s.s.Query(
			`SELECT key, version_id, is_delete_marker, size, etag, content_type,
			        storage_class, mtime, manifest, user_meta
			 FROM objects WHERE bucket_id=? AND shard=? AND key > ?`,
			gocqlUUID(bucketID), shard, marker,
		).WithContext(ctx).PageSize(pageSize).Iter()
	}
	return &shardCursor{iter: iter, bucket: bucketID, shard: shard}, nil
}

type cursorHeap []*shardCursor

func (h cursorHeap) Len() int            { return len(h) }
func (h cursorHeap) Less(i, j int) bool  { return h[i].current.Key < h[j].current.Key }
func (h cursorHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *cursorHeap) Push(x any)         { *h = append(*h, x.(*shardCursor)) }
func (h *cursorHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func (s *Store) EnqueueChunkDeletion(ctx context.Context, region string, chunks []data.ChunkRef) error {
	if len(chunks) == 0 {
		return nil
	}
	now := time.Now().UTC()
	batch := s.s.NewBatch(gocql.LoggedBatch).WithContext(ctx)
	for _, c := range chunks {
		shardID := meta.GCShardID(c.OID)
		batch.Query(
			`INSERT INTO gc_entries_v2 (region, shard_id, enqueued_at, oid, pool, cluster, namespace)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			region, shardID, now, c.OID, c.Pool, c.Cluster, c.Namespace,
		)
		if s.gcDualWrite {
			batch.Query(
				`INSERT INTO gc_queue (region, enqueued_at, oid, pool, cluster, namespace)
				 VALUES (?, ?, ?, ?, ?, ?)`,
				region, now, c.OID, c.Pool, c.Cluster, c.Namespace,
			)
		}
	}
	return s.s.ExecuteBatch(batch)
}

func (s *Store) ListGCEntries(ctx context.Context, region string, before time.Time, limit int) ([]meta.GCEntry, error) {
	return s.ListGCEntriesShard(ctx, region, 0, 1, before, limit)
}

// ListGCEntriesShard reads the GC queue partitioned across 1024 logical
// shards (`gc_entries_v2`). The runtime caller owns every logical shard
// `s` where `s % shardCount == shardID`; the read iterates those partitions
// and stops when `limit` rows have been collected. During the dual-write
// window (s.gcDualWrite, US-002 cutover), if v2 yields fewer than `limit`
// rows the remainder is topped up from the legacy `gc_queue` partition (a
// post-fetch shard filter applies). The legacy fallback drops automatically
// once `gc_queue` is empty.
func (s *Store) ListGCEntriesShard(ctx context.Context, region string, shardID, shardCount int, before time.Time, limit int) ([]meta.GCEntry, error) {
	if shardCount <= 0 {
		shardCount = 1
	}
	if shardID < 0 || shardID >= shardCount {
		return nil, nil
	}
	if limit <= 0 {
		limit = 1000
	}
	out := make([]meta.GCEntry, 0, limit)
	seen := make(map[string]struct{}, limit)
	addEntry := func(enqueuedAt time.Time, oid, pool, cluster, namespace string, sid int) bool {
		if _, ok := seen[oid]; ok {
			return len(out) < limit
		}
		seen[oid] = struct{}{}
		out = append(out, meta.GCEntry{
			EnqueuedAt: enqueuedAt,
			ShardID:    sid,
			Chunk: data.ChunkRef{
				Cluster:   cluster,
				Pool:      pool,
				Namespace: namespace,
				OID:       oid,
			},
		})
		return len(out) < limit
	}

	for _, logicalShard := range gcOwnedLogicalShards(shardID, shardCount) {
		if len(out) >= limit {
			break
		}
		remaining := limit - len(out)
		iter := s.s.Query(
			`SELECT enqueued_at, oid, pool, cluster, namespace
			 FROM gc_entries_v2 WHERE region=? AND shard_id=? AND enqueued_at <= ? LIMIT ?`,
			region, logicalShard, before, remaining,
		).WithContext(ctx).Iter()
		var (
			enqueuedAt time.Time
			oid, pool  string
			cluster    string
			namespace  string
		)
		for iter.Scan(&enqueuedAt, &oid, &pool, &cluster, &namespace) {
			if !addEntry(enqueuedAt, oid, pool, cluster, namespace, logicalShard) {
				break
			}
		}
		if err := iter.Close(); err != nil {
			return nil, err
		}
	}
	if len(out) >= limit || !s.gcDualWrite {
		return out, nil
	}

	// Legacy `gc_queue` fallback: post-filter by shard identity, top up
	// to `limit`. Over-fetches `(limit-len(out)) * shardCount` (cap 4096)
	// because `gc_queue` is a single partition with no shard column.
	remaining := limit - len(out)
	scan := remaining * shardCount
	if scan <= 0 || scan > 4096 {
		scan = 4096
	}
	iter := s.s.Query(
		`SELECT enqueued_at, oid, pool, cluster, namespace
		 FROM gc_queue WHERE region=? AND enqueued_at <= ? LIMIT ?`,
		region, before, scan,
	).WithContext(ctx).Iter()
	var (
		enqueuedAt time.Time
		oid, pool  string
		cluster    string
		namespace  string
	)
	for iter.Scan(&enqueuedAt, &oid, &pool, &cluster, &namespace) {
		sid := meta.GCShardID(oid)
		if sid%shardCount != shardID {
			continue
		}
		if !addEntry(enqueuedAt, oid, pool, cluster, namespace, sid) {
			break
		}
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// gcOwnedLogicalShards returns the subset of [0, meta.GCShardCount) that
// the runtime shard `shardID` (of `shardCount`) owns under modulo mapping.
// shardCount=1 returns every logical shard; shardCount=meta.GCShardCount
// returns exactly one.
func gcOwnedLogicalShards(shardID, shardCount int) []int {
	out := make([]int, 0, meta.GCShardCount/shardCount+1)
	for s := shardID; s < meta.GCShardCount; s += shardCount {
		out = append(out, s)
	}
	return out
}

func (s *Store) AckGCEntry(ctx context.Context, region string, e meta.GCEntry) error {
	if err := s.s.Query(
		`DELETE FROM gc_entries_v2 WHERE region=? AND shard_id=? AND enqueued_at=? AND oid=?`,
		region, e.ShardID, e.EnqueuedAt, e.Chunk.OID,
	).WithContext(ctx).Exec(); err != nil {
		return err
	}
	if !s.gcDualWrite {
		return nil
	}
	// Drop the legacy row too while dual-write is on so the two tables
	// converge regardless of which side served the read. Idempotent —
	// no-op when the row is absent.
	return s.s.Query(
		`DELETE FROM gc_queue WHERE region=? AND enqueued_at=? AND oid=?`,
		region, e.EnqueuedAt, e.Chunk.OID,
	).WithContext(ctx).Exec()
}

func notifyHour(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC)
}

func (s *Store) EnqueueNotification(ctx context.Context, evt *meta.NotificationEvent) error {
	if evt == nil {
		return nil
	}
	if evt.EventTime.IsZero() {
		evt.EventTime = time.Now().UTC()
	}
	if evt.EventID == "" {
		evt.EventID = gocql.TimeUUID().String()
	}
	eventUUID, err := gocql.ParseUUID(evt.EventID)
	if err != nil {
		return err
	}
	hour := notifyHour(evt.EventTime)
	return s.s.Query(
		`INSERT INTO notify_queue (bucket_id, hour, event_id, bucket_name, object_key, event_name, event_time, config_id, target_type, target_arn, payload)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		gocqlUUID(evt.BucketID), hour, eventUUID, evt.Bucket, evt.Key,
		evt.EventName, evt.EventTime, evt.ConfigID, evt.TargetType, evt.TargetARN, evt.Payload,
	).WithContext(ctx).Exec()
}

func (s *Store) ListPendingNotifications(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.NotificationEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	now := time.Now().UTC()
	hour := notifyHour(now)
	out := make([]meta.NotificationEvent, 0, limit)
	// Walk the last 48 hours so the worker (US-009) and inspector tests catch
	// recently enqueued events. Older partitions tombstone via TTL once the
	// worker delivers them.
	for i := 0; i < 48 && len(out) < limit; i++ {
		partition := hour.Add(time.Duration(-i) * time.Hour)
		iter := s.s.Query(
			`SELECT event_id, bucket_name, object_key, event_name, event_time, config_id, target_type, target_arn, payload
			 FROM notify_queue WHERE bucket_id=? AND hour=? LIMIT ?`,
			gocqlUUID(bucketID), partition, limit-len(out),
		).WithContext(ctx).Iter()
		var (
			eventID                                                gocql.UUID
			bucket, key, name, configID, targetType, targetARN     string
			eventTime                                              time.Time
			payload                                                []byte
		)
		for iter.Scan(&eventID, &bucket, &key, &name, &eventTime, &configID, &targetType, &targetARN, &payload) {
			out = append(out, meta.NotificationEvent{
				BucketID:   bucketID,
				Bucket:     bucket,
				Key:        key,
				EventID:    eventID.String(),
				EventName:  name,
				EventTime:  eventTime,
				ConfigID:   configID,
				TargetType: targetType,
				TargetARN:  targetARN,
				Payload:    append([]byte(nil), payload...),
			})
			if len(out) >= limit {
				break
			}
		}
		if err := iter.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) AckNotification(ctx context.Context, evt meta.NotificationEvent) error {
	if evt.EventID == "" {
		return nil
	}
	eventUUID, err := gocql.ParseUUID(evt.EventID)
	if err != nil {
		return err
	}
	hour := notifyHour(evt.EventTime)
	return s.s.Query(
		`DELETE FROM notify_queue WHERE bucket_id=? AND hour=? AND event_id=?`,
		gocqlUUID(evt.BucketID), hour, eventUUID,
	).WithContext(ctx).Exec()
}

func notifyDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func (s *Store) EnqueueNotificationDLQ(ctx context.Context, entry *meta.NotificationDLQEntry) error {
	if entry == nil {
		return nil
	}
	if entry.EnqueuedAt.IsZero() {
		entry.EnqueuedAt = time.Now().UTC()
	}
	if entry.EventID == "" {
		entry.EventID = gocql.TimeUUID().String()
	}
	eventUUID, err := gocql.ParseUUID(entry.EventID)
	if err != nil {
		return err
	}
	day := notifyDay(entry.EnqueuedAt)
	return s.s.Query(
		`INSERT INTO notify_dlq (bucket_id, day, event_id, bucket_name, object_key, event_name, event_time, config_id, target_type, target_arn, payload, attempts, reason, enqueued_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		gocqlUUID(entry.BucketID), day, eventUUID, entry.Bucket, entry.Key,
		entry.EventName, entry.EventTime, entry.ConfigID, entry.TargetType, entry.TargetARN,
		entry.Payload, entry.Attempts, entry.Reason, entry.EnqueuedAt,
	).WithContext(ctx).Exec()
}

func (s *Store) ListNotificationDLQ(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.NotificationDLQEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	now := time.Now().UTC()
	day := notifyDay(now)
	out := make([]meta.NotificationDLQEntry, 0, limit)
	// Walk the last 30 days of DLQ partitions; matches the audit/retention
	// window so an operator inspecting the queue catches everything.
	for i := 0; i < 30 && len(out) < limit; i++ {
		partition := day.AddDate(0, 0, -i)
		iter := s.s.Query(
			`SELECT event_id, bucket_name, object_key, event_name, event_time, config_id, target_type, target_arn, payload, attempts, reason, enqueued_at
			 FROM notify_dlq WHERE bucket_id=? AND day=? LIMIT ?`,
			gocqlUUID(bucketID), partition, limit-len(out),
		).WithContext(ctx).Iter()
		var (
			eventID                                            gocql.UUID
			bucket, key, name, configID, targetType, targetARN string
			eventTime, enqueuedAt                              time.Time
			payload                                            []byte
			attempts                                           int
			reason                                             string
		)
		for iter.Scan(&eventID, &bucket, &key, &name, &eventTime, &configID, &targetType, &targetARN, &payload, &attempts, &reason, &enqueuedAt) {
			out = append(out, meta.NotificationDLQEntry{
				NotificationEvent: meta.NotificationEvent{
					BucketID:   bucketID,
					Bucket:     bucket,
					Key:        key,
					EventID:    eventID.String(),
					EventName:  name,
					EventTime:  eventTime,
					ConfigID:   configID,
					TargetType: targetType,
					TargetARN:  targetARN,
					Payload:    append([]byte(nil), payload...),
				},
				Attempts:   attempts,
				Reason:     reason,
				EnqueuedAt: enqueuedAt,
			})
			if len(out) >= limit {
				break
			}
		}
		if err := iter.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) EnqueueReplication(ctx context.Context, evt *meta.ReplicationEvent) error {
	if evt == nil {
		return nil
	}
	if evt.EventTime.IsZero() {
		evt.EventTime = time.Now().UTC()
	}
	if evt.EventID == "" {
		evt.EventID = gocql.TimeUUID().String()
	}
	eventUUID, err := gocql.ParseUUID(evt.EventID)
	if err != nil {
		return err
	}
	day := notifyDay(evt.EventTime)
	return s.s.Query(
		`INSERT INTO replication_queue (bucket_id, day, event_id, bucket_name, object_key, version_id, event_name, event_time, rule_id, destination_bucket, storage_class, destination_endpoint)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		gocqlUUID(evt.BucketID), day, eventUUID, evt.Bucket, evt.Key, evt.VersionID,
		evt.EventName, evt.EventTime, evt.RuleID, evt.DestinationBucket, evt.StorageClass, evt.DestinationEndpoint,
	).WithContext(ctx).Exec()
}

func (s *Store) ListPendingReplications(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.ReplicationEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	now := time.Now().UTC()
	day := notifyDay(now)
	out := make([]meta.ReplicationEvent, 0, limit)
	// Walk the last 30 days of partitions; matches notify_dlq retention so a
	// paused replicator catches everything within the operator-visible window.
	for i := 0; i < 30 && len(out) < limit; i++ {
		partition := day.AddDate(0, 0, -i)
		iter := s.s.Query(
			`SELECT event_id, bucket_name, object_key, version_id, event_name, event_time, rule_id, destination_bucket, storage_class, destination_endpoint
			 FROM replication_queue WHERE bucket_id=? AND day=? LIMIT ?`,
			gocqlUUID(bucketID), partition, limit-len(out),
		).WithContext(ctx).Iter()
		var (
			eventID                                                                                  gocql.UUID
			bucket, key, versionID, name, ruleID, destinationBucket, storageClass, destinationEndpoint string
			eventTime                                                                                time.Time
		)
		for iter.Scan(&eventID, &bucket, &key, &versionID, &name, &eventTime, &ruleID, &destinationBucket, &storageClass, &destinationEndpoint) {
			out = append(out, meta.ReplicationEvent{
				BucketID:            bucketID,
				Bucket:              bucket,
				Key:                 key,
				VersionID:           versionID,
				EventID:             eventID.String(),
				EventName:           name,
				EventTime:           eventTime,
				RuleID:              ruleID,
				DestinationBucket:   destinationBucket,
				DestinationEndpoint: destinationEndpoint,
				StorageClass:        storageClass,
			})
			if len(out) >= limit {
				break
			}
		}
		if err := iter.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) SetObjectReplicationStatus(ctx context.Context, bucketID uuid.UUID, key, versionID, status string) error {
	shard := shardOf(key, s.defaultShard)
	if versionID == "" {
		o, err := s.GetObject(ctx, bucketID, key, "")
		if err != nil {
			return err
		}
		versionID = o.VersionID
	}
	vUUID, err := versionToCQL(versionID)
	if err != nil {
		return meta.ErrObjectNotFound
	}
	return s.s.Query(
		`UPDATE objects SET replication_status=? WHERE bucket_id=? AND shard=? AND key=? AND version_id=?`,
		nilIfEmpty(status), gocqlUUID(bucketID), shard, key, vUUID,
	).WithContext(ctx).Exec()
}

func (s *Store) AckReplication(ctx context.Context, evt meta.ReplicationEvent) error {
	if evt.EventID == "" {
		return nil
	}
	eventUUID, err := gocql.ParseUUID(evt.EventID)
	if err != nil {
		return err
	}
	day := notifyDay(evt.EventTime)
	return s.s.Query(
		`DELETE FROM replication_queue WHERE bucket_id=? AND day=? AND event_id=?`,
		gocqlUUID(evt.BucketID), day, eventUUID,
	).WithContext(ctx).Exec()
}

func (s *Store) EnqueueAccessLog(ctx context.Context, entry *meta.AccessLogEntry) error {
	if entry == nil {
		return nil
	}
	if entry.Time.IsZero() {
		entry.Time = time.Now().UTC()
	}
	if entry.EventID == "" {
		entry.EventID = gocql.TimeUUID().String()
	}
	eventUUID, err := gocql.ParseUUID(entry.EventID)
	if err != nil {
		return err
	}
	hour := notifyHour(entry.Time)
	return s.s.Query(
		`INSERT INTO access_log_buffer (bucket_id, hour, event_id, ts, request_id, principal, source_ip, op, object_key, status, bytes_sent, object_size, total_time_ms, turn_around_ms, referrer, user_agent, version_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		gocqlUUID(entry.BucketID), hour, eventUUID, entry.Time,
		entry.RequestID, entry.Principal, entry.SourceIP, entry.Op, entry.Key,
		entry.Status, entry.BytesSent, entry.ObjectSize, entry.TotalTimeMS, entry.TurnAroundMS,
		entry.Referrer, entry.UserAgent, entry.VersionID,
	).WithContext(ctx).Exec()
}

func (s *Store) ListPendingAccessLog(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.AccessLogEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	now := time.Now().UTC()
	hour := notifyHour(now)
	out := make([]meta.AccessLogEntry, 0, limit)
	// Walk the last 48 hours so the access-log worker (US-014) catches buffered
	// rows even after a brief outage. Older partitions tombstone via TTL once
	// the worker drains them.
	for i := 0; i < 48 && len(out) < limit; i++ {
		partition := hour.Add(time.Duration(-i) * time.Hour)
		iter := s.s.Query(
			`SELECT event_id, ts, request_id, principal, source_ip, op, object_key, status, bytes_sent, object_size, total_time_ms, turn_around_ms, referrer, user_agent, version_id
			 FROM access_log_buffer WHERE bucket_id=? AND hour=? LIMIT ?`,
			gocqlUUID(bucketID), partition, limit-len(out),
		).WithContext(ctx).Iter()
		var (
			eventID                                                                  gocql.UUID
			ts                                                                       time.Time
			requestID, principal, sourceIP, op, key, referrer, userAgent, versionID  string
			status, totalTimeMS, turnAroundMS                                        int
			bytesSent, objectSize                                                    int64
		)
		for iter.Scan(&eventID, &ts, &requestID, &principal, &sourceIP, &op, &key, &status, &bytesSent, &objectSize, &totalTimeMS, &turnAroundMS, &referrer, &userAgent, &versionID) {
			out = append(out, meta.AccessLogEntry{
				BucketID:     bucketID,
				EventID:      eventID.String(),
				Time:         ts,
				RequestID:    requestID,
				Principal:    principal,
				SourceIP:     sourceIP,
				Op:           op,
				Key:          key,
				Status:       status,
				BytesSent:    bytesSent,
				ObjectSize:   objectSize,
				TotalTimeMS:  totalTimeMS,
				TurnAroundMS: turnAroundMS,
				Referrer:     referrer,
				UserAgent:    userAgent,
				VersionID:    versionID,
			})
			if len(out) >= limit {
				break
			}
		}
		if err := iter.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) AckAccessLog(ctx context.Context, entry meta.AccessLogEntry) error {
	if entry.EventID == "" {
		return nil
	}
	eventUUID, err := gocql.ParseUUID(entry.EventID)
	if err != nil {
		return err
	}
	hour := notifyHour(entry.Time)
	return s.s.Query(
		`DELETE FROM access_log_buffer WHERE bucket_id=? AND hour=? AND event_id=?`,
		gocqlUUID(entry.BucketID), hour, eventUUID,
	).WithContext(ctx).Exec()
}

func (s *Store) EnqueueAudit(ctx context.Context, entry *meta.AuditEvent, ttl time.Duration) error {
	if entry == nil {
		return nil
	}
	if entry.Time.IsZero() {
		entry.Time = time.Now().UTC()
	}
	if entry.EventID == "" {
		entry.EventID = gocql.TimeUUID().String()
	}
	eventUUID, err := gocql.ParseUUID(entry.EventID)
	if err != nil {
		return err
	}
	day := notifyDay(entry.Time)
	bucket := entry.Bucket
	if bucket == "" {
		bucket = "-"
	}
	q := `INSERT INTO audit_log (bucket_id, day, event_id, ts, principal, action, resource, result, request_id, source_ip, bucket_name, user_agent, total_time_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	args := []any{
		gocqlUUID(entry.BucketID), day, eventUUID, entry.Time,
		entry.Principal, entry.Action, entry.Resource, entry.Result,
		entry.RequestID, entry.SourceIP, bucket, entry.UserAgent, entry.TotalTimeMS,
	}
	if ttl > 0 {
		q += ` USING TTL ?`
		args = append(args, max(int(ttl/time.Second), 1))
	}
	return s.s.Query(q, args...).WithContext(ctx).Exec()
}

func (s *Store) ListAuditFiltered(ctx context.Context, f meta.AuditFilter) ([]meta.AuditEvent, string, error) {
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	now := time.Now().UTC()
	end := f.End
	if end.IsZero() {
		end = now
	}
	start := f.Start
	if start.IsZero() {
		start = end.AddDate(0, 0, -30)
	}
	type partition struct {
		bucket gocql.UUID
		day    time.Time
	}
	var parts []partition
	endDay := notifyDay(end)
	startDay := notifyDay(start)
	if f.BucketScoped {
		bid := gocqlUUID(f.BucketID)
		for d := endDay; !d.Before(startDay); d = d.AddDate(0, 0, -1) {
			parts = append(parts, partition{bid, d})
		}
	} else {
		iter := s.s.Query(`SELECT DISTINCT bucket_id, day FROM audit_log`).WithContext(ctx).Iter()
		var (
			bid gocql.UUID
			d   time.Time
		)
		for iter.Scan(&bid, &d) {
			d = d.UTC()
			if d.Before(startDay) || d.After(endDay) {
				continue
			}
			parts = append(parts, partition{bid, d})
		}
		if err := iter.Close(); err != nil {
			return nil, "", err
		}
	}
	var all []meta.AuditEvent
	for _, p := range parts {
		iter := s.s.Query(
			`SELECT event_id, ts, principal, action, resource, result, request_id, source_ip, bucket_name, user_agent, total_time_ms
			 FROM audit_log WHERE bucket_id=? AND day=?`,
			p.bucket, p.day,
		).WithContext(ctx).Iter()
		var (
			eventID                                                                         gocql.UUID
			ts                                                                              time.Time
			principal, action, resource, result, requestID, sourceIP, bucketName, userAgent string
			totalTimeMS                                                                     int
		)
		for iter.Scan(&eventID, &ts, &principal, &action, &resource, &result, &requestID, &sourceIP, &bucketName, &userAgent, &totalTimeMS) {
			all = append(all, meta.AuditEvent{
				BucketID:    uuidFromGocql(p.bucket),
				Bucket:      bucketName,
				EventID:     eventID.String(),
				Time:        ts,
				Principal:   principal,
				Action:      action,
				Resource:    resource,
				Result:      result,
				RequestID:   requestID,
				SourceIP:    sourceIP,
				UserAgent:   userAgent,
				TotalTimeMS: totalTimeMS,
			})
		}
		if err := iter.Close(); err != nil {
			return nil, "", err
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].Time.Equal(all[j].Time) {
			return all[i].Time.After(all[j].Time)
		}
		return all[i].EventID > all[j].EventID
	})
	out := make([]meta.AuditEvent, 0, limit)
	started := f.Continuation == ""
	for _, e := range all {
		if !f.Start.IsZero() && e.Time.Before(f.Start) {
			continue
		}
		if !f.End.IsZero() && e.Time.After(f.End) {
			continue
		}
		if f.Principal != "" && e.Principal != f.Principal {
			continue
		}
		if !started {
			if e.EventID == f.Continuation {
				started = true
			}
			continue
		}
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	next := ""
	if len(out) >= limit {
		next = out[len(out)-1].EventID
	}
	return out, next, nil
}

func (s *Store) ListAuditPartitionsBefore(ctx context.Context, before time.Time) ([]meta.AuditPartition, error) {
	cutoff := notifyDay(before)
	type partition struct {
		bid gocql.UUID
		day time.Time
	}
	var parts []partition
	iter := s.s.Query(`SELECT DISTINCT bucket_id, day FROM audit_log`).WithContext(ctx).Iter()
	var (
		bid gocql.UUID
		d   time.Time
	)
	for iter.Scan(&bid, &d) {
		d = d.UTC()
		if !d.Before(cutoff) {
			continue
		}
		parts = append(parts, partition{bid, d})
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	out := make([]meta.AuditPartition, 0, len(parts))
	for _, p := range parts {
		var bucketName string
		// One-row peek so the export key carries the human-readable bucket
		// name; uuid.Nil partitions return "-" so IAM rows stay routable.
		if err := s.s.Query(
			`SELECT bucket_name FROM audit_log WHERE bucket_id=? AND day=? LIMIT 1`,
			p.bid, p.day,
		).WithContext(ctx).Scan(&bucketName); err != nil && !errors.Is(err, gocql.ErrNotFound) {
			return nil, err
		}
		if bucketName == "" {
			bucketName = "-"
		}
		out = append(out, meta.AuditPartition{
			BucketID: uuidFromGocql(p.bid),
			Bucket:   bucketName,
			Day:      p.day,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Day.Equal(out[j].Day) {
			return out[i].Day.Before(out[j].Day)
		}
		return out[i].BucketID.String() < out[j].BucketID.String()
	})
	return out, nil
}

func (s *Store) ReadAuditPartition(ctx context.Context, bucketID uuid.UUID, day time.Time) ([]meta.AuditEvent, error) {
	d := notifyDay(day)
	iter := s.s.Query(
		`SELECT event_id, ts, principal, action, resource, result, request_id, source_ip, bucket_name, user_agent, total_time_ms
		 FROM audit_log WHERE bucket_id=? AND day=?`,
		gocqlUUID(bucketID), d,
	).WithContext(ctx).Iter()
	var (
		eventID                                                                         gocql.UUID
		ts                                                                              time.Time
		principal, action, resource, result, requestID, sourceIP, bucketName, userAgent string
		totalTimeMS                                                                     int
	)
	var out []meta.AuditEvent
	for iter.Scan(&eventID, &ts, &principal, &action, &resource, &result, &requestID, &sourceIP, &bucketName, &userAgent, &totalTimeMS) {
		out = append(out, meta.AuditEvent{
			BucketID:    bucketID,
			Bucket:      bucketName,
			EventID:     eventID.String(),
			Time:        ts,
			Principal:   principal,
			Action:      action,
			Resource:    resource,
			Result:      result,
			RequestID:   requestID,
			SourceIP:    sourceIP,
			UserAgent:   userAgent,
			TotalTimeMS: totalTimeMS,
		})
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EventID < out[j].EventID })
	return out, nil
}

func (s *Store) DeleteAuditPartition(ctx context.Context, bucketID uuid.UUID, day time.Time) error {
	d := notifyDay(day)
	return s.s.Query(
		`DELETE FROM audit_log WHERE bucket_id=? AND day=?`,
		gocqlUUID(bucketID), d,
	).WithContext(ctx).Exec()
}

func (s *Store) ListAudit(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.AuditEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	now := time.Now().UTC()
	day := notifyDay(now)
	out := make([]meta.AuditEvent, 0, limit)
	// Walk the last 30 days of partitions; matches the default audit retention
	// so a fresh inspection catches everything the TTL has not yet purged.
	for i := 0; i < 30 && len(out) < limit; i++ {
		partition := day.AddDate(0, 0, -i)
		iter := s.s.Query(
			`SELECT event_id, ts, principal, action, resource, result, request_id, source_ip, bucket_name, user_agent, total_time_ms
			 FROM audit_log WHERE bucket_id=? AND day=? LIMIT ?`,
			gocqlUUID(bucketID), partition, limit-len(out),
		).WithContext(ctx).Iter()
		var (
			eventID                                                                         gocql.UUID
			ts                                                                              time.Time
			principal, action, resource, result, requestID, sourceIP, bucketName, userAgent string
			totalTimeMS                                                                     int
		)
		for iter.Scan(&eventID, &ts, &principal, &action, &resource, &result, &requestID, &sourceIP, &bucketName, &userAgent, &totalTimeMS) {
			out = append(out, meta.AuditEvent{
				BucketID:    bucketID,
				Bucket:      bucketName,
				EventID:     eventID.String(),
				Time:        ts,
				Principal:   principal,
				Action:      action,
				Resource:    resource,
				Result:      result,
				RequestID:   requestID,
				SourceIP:    sourceIP,
				UserAgent:   userAgent,
				TotalTimeMS: totalTimeMS,
			})
			if len(out) >= limit {
				break
			}
		}
		if err := iter.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ListSlowQueries serves the US-003 slow-queries debug endpoint. Walks every
// (bucket, day) partition that overlaps the trailing `since` window and
// applies a server-side `total_time_ms >= ?` filter inside each partition
// (ALLOW FILTERING is acceptable here — the partition is already narrow
// because (bucket_id, day) is the PK). Results are merged and sorted by
// TotalTimeMS DESC; ties broken by Time DESC, then EventID DESC.
func (s *Store) ListSlowQueries(ctx context.Context, since time.Duration, minMs int, pageToken string) ([]meta.AuditEvent, string, error) {
	const limit = 100
	if since <= 0 {
		since = 15 * time.Minute
	}
	if minMs < 0 {
		minMs = 0
	}
	now := time.Now().UTC()
	startCutoff := now.Add(-since)
	endDay := notifyDay(now)
	startDay := notifyDay(startCutoff)
	type partition struct {
		bid gocql.UUID
		day time.Time
	}
	var parts []partition
	iter := s.s.Query(`SELECT DISTINCT bucket_id, day FROM audit_log`).WithContext(ctx).Iter()
	var (
		bid gocql.UUID
		d   time.Time
	)
	for iter.Scan(&bid, &d) {
		d = d.UTC()
		if d.Before(startDay) || d.After(endDay) {
			continue
		}
		parts = append(parts, partition{bid, d})
	}
	if err := iter.Close(); err != nil {
		return nil, "", err
	}
	var all []meta.AuditEvent
	for _, p := range parts {
		row := s.s.Query(
			`SELECT event_id, ts, principal, action, resource, result, request_id, source_ip, bucket_name, user_agent, total_time_ms
			 FROM audit_log WHERE bucket_id=? AND day=? AND total_time_ms >= ? ALLOW FILTERING`,
			p.bid, p.day, minMs,
		).WithContext(ctx).Iter()
		var (
			eventID                                                                         gocql.UUID
			ts                                                                              time.Time
			principal, action, resource, result, requestID, sourceIP, bucketName, userAgent string
			totalTimeMS                                                                     int
		)
		for row.Scan(&eventID, &ts, &principal, &action, &resource, &result, &requestID, &sourceIP, &bucketName, &userAgent, &totalTimeMS) {
			if ts.Before(startCutoff) {
				continue
			}
			all = append(all, meta.AuditEvent{
				BucketID:    uuidFromGocql(p.bid),
				Bucket:      bucketName,
				EventID:     eventID.String(),
				Time:        ts,
				Principal:   principal,
				Action:      action,
				Resource:    resource,
				Result:      result,
				RequestID:   requestID,
				SourceIP:    sourceIP,
				UserAgent:   userAgent,
				TotalTimeMS: totalTimeMS,
			})
		}
		if err := row.Close(); err != nil {
			return nil, "", err
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].TotalTimeMS != all[j].TotalTimeMS {
			return all[i].TotalTimeMS > all[j].TotalTimeMS
		}
		if !all[i].Time.Equal(all[j].Time) {
			return all[i].Time.After(all[j].Time)
		}
		return all[i].EventID > all[j].EventID
	})
	out := make([]meta.AuditEvent, 0, limit)
	started := pageToken == ""
	for _, e := range all {
		if !started {
			if e.EventID == pageToken {
				started = true
			}
			continue
		}
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	next := ""
	if len(out) >= limit {
		next = out[len(out)-1].EventID
	}
	return out, next, nil
}

func (s *Store) resolveVersionID(ctx context.Context, bucketID uuid.UUID, key, versionID string) (gocql.UUID, int, error) {
	shard := shardOf(key, s.defaultShard)
	if versionID != "" {
		v, err := versionToCQL(versionID)
		if err != nil {
			return gocql.UUID{}, 0, meta.ErrObjectNotFound
		}
		return v, shard, nil
	}
	var v gocql.UUID
	err := s.s.Query(
		`SELECT version_id FROM objects WHERE bucket_id=? AND shard=? AND key=? LIMIT 1`,
		gocqlUUID(bucketID), shard, key,
	).WithContext(ctx).Scan(&v)
	if errors.Is(err, gocql.ErrNotFound) {
		return gocql.UUID{}, 0, meta.ErrObjectNotFound
	}
	return v, shard, err
}

func (s *Store) SetObjectStorage(ctx context.Context, bucketID uuid.UUID, key, versionID, expectedClass, newClass string, manifest *data.Manifest) (bool, error) {
	v, shard, err := s.resolveVersionID(ctx, bucketID, key, versionID)
	if err != nil {
		return false, err
	}
	manifestBlob, err := encodeManifest(manifest)
	if err != nil {
		return false, err
	}
	if expectedClass == "" {
		err := s.s.Query(
			`UPDATE objects SET storage_class=?, manifest=? WHERE bucket_id=? AND shard=? AND key=? AND version_id=?`,
			newClass, manifestBlob, gocqlUUID(bucketID), shard, key, v,
		).WithContext(ctx).Exec()
		return err == nil, err
	}
	var currentClass string
	applied, err := s.s.Query(
		`UPDATE objects SET storage_class=?, manifest=?
		 WHERE bucket_id=? AND shard=? AND key=? AND version_id=?
		 IF storage_class=?`,
		newClass, manifestBlob, gocqlUUID(bucketID), shard, key, v, expectedClass,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(&currentClass)
	if err == nil && !applied {
		s.recordLWTConflict(ctx, "objects", bucketID, shard)
	}
	return applied, err
}

func (s *Store) SetObjectTags(ctx context.Context, bucketID uuid.UUID, key, versionID string, tags map[string]string) error {
	v, shard, err := s.resolveVersionID(ctx, bucketID, key, versionID)
	if err != nil {
		return err
	}
	return s.s.Query(
		`UPDATE objects SET tags=? WHERE bucket_id=? AND shard=? AND key=? AND version_id=?`,
		tags, gocqlUUID(bucketID), shard, key, v,
	).WithContext(ctx).Exec()
}

func (s *Store) GetObjectTags(ctx context.Context, bucketID uuid.UUID, key, versionID string) (map[string]string, error) {
	v, shard, err := s.resolveVersionID(ctx, bucketID, key, versionID)
	if err != nil {
		return nil, err
	}
	var tags map[string]string
	err = s.s.Query(
		`SELECT tags FROM objects WHERE bucket_id=? AND shard=? AND key=? AND version_id=?`,
		gocqlUUID(bucketID), shard, key, v,
	).WithContext(ctx).Scan(&tags)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrObjectNotFound
	}
	return tags, err
}

func (s *Store) DeleteObjectTags(ctx context.Context, bucketID uuid.UUID, key, versionID string) error {
	v, shard, err := s.resolveVersionID(ctx, bucketID, key, versionID)
	if err != nil {
		return err
	}
	return s.s.Query(
		`DELETE tags FROM objects WHERE bucket_id=? AND shard=? AND key=? AND version_id=?`,
		gocqlUUID(bucketID), shard, key, v,
	).WithContext(ctx).Exec()
}

func (s *Store) SetObjectRetention(ctx context.Context, bucketID uuid.UUID, key, versionID, mode string, until time.Time) error {
	v, shard, err := s.resolveVersionID(ctx, bucketID, key, versionID)
	if err != nil {
		return err
	}
	var retainUntil *time.Time
	if !until.IsZero() {
		retainUntil = &until
	}
	return s.s.Query(
		`UPDATE objects SET retain_mode=?, retain_until=? WHERE bucket_id=? AND shard=? AND key=? AND version_id=?`,
		nilIfEmpty(mode), retainUntil, gocqlUUID(bucketID), shard, key, v,
	).WithContext(ctx).Exec()
}

func (s *Store) SetObjectLegalHold(ctx context.Context, bucketID uuid.UUID, key, versionID string, on bool) error {
	v, shard, err := s.resolveVersionID(ctx, bucketID, key, versionID)
	if err != nil {
		return err
	}
	return s.s.Query(
		`UPDATE objects SET legal_hold=? WHERE bucket_id=? AND shard=? AND key=? AND version_id=?`,
		on, gocqlUUID(bucketID), shard, key, v,
	).WithContext(ctx).Exec()
}

func (s *Store) SetObjectRestoreStatus(ctx context.Context, bucketID uuid.UUID, key, versionID, status string) error {
	v, shard, err := s.resolveVersionID(ctx, bucketID, key, versionID)
	if err != nil {
		return err
	}
	return s.s.Query(
		`UPDATE objects SET restore_status=? WHERE bucket_id=? AND shard=? AND key=? AND version_id=?`,
		nilIfEmpty(status), gocqlUUID(bucketID), shard, key, v,
	).WithContext(ctx).Exec()
}

func (s *Store) SetBucketLifecycle(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error {
	return s.s.Query(
		`INSERT INTO bucket_lifecycle (bucket_id, rules) VALUES (?, ?)`,
		gocqlUUID(bucketID), xmlBlob,
	).WithContext(ctx).Exec()
}

func (s *Store) GetBucketLifecycle(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	var rules []byte
	err := s.s.Query(
		`SELECT rules FROM bucket_lifecycle WHERE bucket_id=?`,
		gocqlUUID(bucketID),
	).WithContext(ctx).Scan(&rules)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrNoSuchLifecycle
	}
	if err != nil {
		return nil, err
	}
	if len(rules) == 0 {
		return nil, meta.ErrNoSuchLifecycle
	}
	return rules, nil
}

func (s *Store) DeleteBucketLifecycle(ctx context.Context, bucketID uuid.UUID) error {
	return s.s.Query(
		`DELETE FROM bucket_lifecycle WHERE bucket_id=?`,
		gocqlUUID(bucketID),
	).WithContext(ctx).Exec()
}

func (s *Store) CreateMultipartUpload(ctx context.Context, mu *meta.MultipartUpload) error {
	uploadUUID, err := gocql.ParseUUID(mu.UploadID)
	if err != nil {
		return fmt.Errorf("upload_id: %w", err)
	}
	return s.s.Query(
		`INSERT INTO multipart_uploads (bucket_id, upload_id, key, status, storage_class, content_type, initiated_at, sse, user_meta, cache_control, expires, checksum_algorithm, sse_key, sse_key_id, checksum_type)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		gocqlUUID(mu.BucketID), uploadUUID, mu.Key, "uploading", mu.StorageClass, mu.ContentType, mu.InitiatedAt, nilIfEmpty(mu.SSE),
		mu.UserMeta, nilIfEmpty(mu.CacheControl), nilIfEmpty(mu.Expires), nilIfEmpty(mu.ChecksumAlgorithm),
		nilIfEmptyBytes(mu.SSEKey), nilIfEmpty(mu.SSEKeyID), nilIfEmpty(mu.ChecksumType),
	).WithContext(ctx).Exec()
}

func (s *Store) GetMultipartUpload(ctx context.Context, bucketID uuid.UUID, uploadID string) (*meta.MultipartUpload, error) {
	uploadUUID, err := gocql.ParseUUID(uploadID)
	if err != nil {
		return nil, meta.ErrMultipartNotFound
	}
	var (
		key, status, class, ctype            string
		sse, cacheControl, expires, checksum string
		checksumType                         string
		sseKeyID                             string
		sseKey                               []byte
		userMeta                             map[string]string
		initiated                            time.Time
	)
	err = s.s.Query(
		`SELECT key, status, storage_class, content_type, initiated_at, sse, user_meta, cache_control, expires, checksum_algorithm, sse_key, sse_key_id, checksum_type
		 FROM multipart_uploads WHERE bucket_id=? AND upload_id=?`,
		gocqlUUID(bucketID), uploadUUID,
	).WithContext(ctx).Scan(&key, &status, &class, &ctype, &initiated, &sse, &userMeta, &cacheControl, &expires, &checksum, &sseKey, &sseKeyID, &checksumType)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrMultipartNotFound
	}
	if err != nil {
		return nil, err
	}
	return &meta.MultipartUpload{
		BucketID:          bucketID,
		UploadID:          uploadID,
		Key:               key,
		Status:            status,
		StorageClass:      class,
		ContentType:       ctype,
		InitiatedAt:       initiated,
		SSE:               sse,
		SSEKey:            sseKey,
		SSEKeyID:          sseKeyID,
		UserMeta:          userMeta,
		CacheControl:      cacheControl,
		Expires:           expires,
		ChecksumAlgorithm: checksum,
		ChecksumType:      checksumType,
	}, nil
}

func (s *Store) ListMultipartUploads(ctx context.Context, bucketID uuid.UUID, prefix string, limit int) ([]*meta.MultipartUpload, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	iter := s.s.Query(
		`SELECT upload_id, key, status, storage_class, content_type, initiated_at
		 FROM multipart_uploads WHERE bucket_id=?`,
		gocqlUUID(bucketID),
	).WithContext(ctx).Iter()
	defer iter.Close()

	var (
		out                       []*meta.MultipartUpload
		uploadUUID                gocql.UUID
		key, status, class, ctype string
		initiated                 time.Time
	)
	for iter.Scan(&uploadUUID, &key, &status, &class, &ctype, &initiated) {
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			continue
		}
		out = append(out, &meta.MultipartUpload{
			BucketID:     bucketID,
			UploadID:     uploadUUID.String(),
			Key:          key,
			Status:       status,
			StorageClass: class,
			ContentType:  ctype,
			InitiatedAt:  initiated,
		})
		if len(out) >= limit {
			break
		}
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) SavePart(ctx context.Context, bucketID uuid.UUID, uploadID string, part *meta.MultipartPart) error {
	uploadUUID, err := gocql.ParseUUID(uploadID)
	if err != nil {
		return meta.ErrMultipartNotFound
	}
	manifestBlob, err := encodeManifest(part.Manifest)
	if err != nil {
		return err
	}
	return s.s.Query(
		`INSERT INTO multipart_parts (bucket_id, upload_id, part_number, etag, size, mtime, manifest, checksums)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		gocqlUUID(bucketID), uploadUUID, part.PartNumber, part.ETag, part.Size, time.Now().UTC(), manifestBlob, part.Checksums,
	).WithContext(ctx).Exec()
}

func (s *Store) ListParts(ctx context.Context, bucketID uuid.UUID, uploadID string) ([]*meta.MultipartPart, error) {
	uploadUUID, err := gocql.ParseUUID(uploadID)
	if err != nil {
		return nil, meta.ErrMultipartNotFound
	}
	iter := s.s.Query(
		`SELECT part_number, etag, size, mtime, manifest, checksums
		 FROM multipart_parts WHERE bucket_id=? AND upload_id=?`,
		gocqlUUID(bucketID), uploadUUID,
	).WithContext(ctx).Iter()
	defer iter.Close()

	var (
		out          []*meta.MultipartPart
		partNumber   int
		etag         string
		size         int64
		mtime        time.Time
		manifestBlob []byte
		checksums    map[string]string
	)
	for iter.Scan(&partNumber, &etag, &size, &mtime, &manifestBlob, &checksums) {
		m, err := decodeManifest(manifestBlob)
		if err != nil {
			return nil, err
		}
		out = append(out, &meta.MultipartPart{
			PartNumber: partNumber,
			ETag:       etag,
			Size:       size,
			Mtime:      mtime,
			Manifest:   m,
			Checksums:  checksums,
		})
		checksums = nil
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) CompleteMultipartUpload(ctx context.Context, obj *meta.Object, uploadID string, parts []meta.CompletePart, versioned bool) ([]*data.Manifest, error) {
	uploadUUID, err := gocql.ParseUUID(uploadID)
	if err != nil {
		return nil, meta.ErrMultipartNotFound
	}

	// Read parts and validate every requested ETag BEFORE flipping the upload
	// to 'completing'. A stale-ETag Complete must leave the upload re-completable
	// rather than wedging it in the completing state. (US-005 resend-finishes-last.)
	storedParts, err := s.ListParts(ctx, obj.BucketID, uploadID)
	if err != nil {
		return nil, err
	}
	byNumber := make(map[int]*meta.MultipartPart, len(storedParts))
	for _, p := range storedParts {
		byNumber[p.PartNumber] = p
	}
	for _, cp := range parts {
		p, ok := byNumber[cp.PartNumber]
		if !ok {
			return nil, meta.ErrMultipartPartMissing
		}
		if strings.Trim(cp.ETag, `"`) != p.ETag {
			return nil, meta.ErrMultipartETagMismatch
		}
	}

	var currentStatus string
	applied, err := s.s.Query(
		`UPDATE multipart_uploads SET status='completing'
		 WHERE bucket_id=? AND upload_id=? IF status='uploading'`,
		gocqlUUID(obj.BucketID), uploadUUID,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(&currentStatus)
	if err != nil {
		return nil, err
	}
	if !applied {
		if currentStatus == "" {
			return nil, meta.ErrMultipartNotFound
		}
		return nil, meta.ErrMultipartInProgress
	}

	used := make(map[int]bool, len(parts))
	var chunks []data.ChunkRef
	var totalSize int64
	var ciphertextSize int64
	partChunkCounts := make([]int, 0, len(parts))
	partRanges := make([]data.PartRange, 0, len(parts))
	partSizes := make([]int64, 0, len(parts))
	partChecksums := make([]map[string]string, 0, len(parts))
	for _, cp := range parts {
		p := byNumber[cp.PartNumber]
		partChunkCount := 0
		if p.Manifest != nil {
			chunks = append(chunks, p.Manifest.Chunks...)
			partChunkCount = len(p.Manifest.Chunks)
			for _, c := range p.Manifest.Chunks {
				ciphertextSize += c.Size
			}
		}
		partChunkCounts = append(partChunkCounts, partChunkCount)
		partRanges = append(partRanges, meta.BuildPartRange(cp.PartNumber, totalSize, p))
		partSizes = append(partSizes, p.Size)
		partChecksums = append(partChecksums, p.Checksums)
		totalSize += p.Size
		used[cp.PartNumber] = true
	}

	obj.Manifest = &data.Manifest{
		Class:           obj.StorageClass,
		Size:            ciphertextSize,
		ChunkSize:       data.DefaultChunkSize,
		ETag:            obj.ETag,
		Chunks:          chunks,
		PartChunks:      partRanges,
		PartChunkCounts: partChunkCounts,
		PartChecksums:   partChecksums,
	}
	obj.Size = totalSize
	obj.PartSizes = partSizes
	obj.Mtime = time.Now().UTC()

	if err := s.PutObject(ctx, obj, versioned); err != nil {
		return nil, err
	}

	var orphans []*data.Manifest
	for _, p := range storedParts {
		if !used[p.PartNumber] && p.Manifest != nil {
			orphans = append(orphans, p.Manifest)
		}
	}

	if err := s.s.Query(
		`DELETE FROM multipart_parts WHERE bucket_id=? AND upload_id=?`,
		gocqlUUID(obj.BucketID), uploadUUID,
	).WithContext(ctx).Exec(); err != nil {
		return orphans, err
	}
	if err := s.s.Query(
		`DELETE FROM multipart_uploads WHERE bucket_id=? AND upload_id=?`,
		gocqlUUID(obj.BucketID), uploadUUID,
	).WithContext(ctx).Exec(); err != nil {
		return orphans, err
	}
	return orphans, nil
}

func (s *Store) RecordMultipartCompletion(ctx context.Context, rec *meta.MultipartCompletion, ttl time.Duration) error {
	if rec == nil {
		return nil
	}
	uploadUUID, err := gocql.ParseUUID(rec.UploadID)
	if err != nil {
		return fmt.Errorf("upload_id: %w", err)
	}
	ttlSec := max(int(ttl/time.Second), 1)
	completedAt := rec.CompletedAt
	if completedAt.IsZero() {
		completedAt = time.Now().UTC()
	}
	return s.s.Query(
		`INSERT INTO multipart_completions (bucket_id, upload_id, key, etag, version_id, body, headers, completed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?) USING TTL ?`,
		gocqlUUID(rec.BucketID), uploadUUID, rec.Key, rec.ETag, rec.VersionID, rec.Body, rec.Headers, completedAt, ttlSec,
	).WithContext(ctx).Exec()
}

func (s *Store) GetMultipartCompletion(ctx context.Context, bucketID uuid.UUID, uploadID string) (*meta.MultipartCompletion, error) {
	uploadUUID, err := gocql.ParseUUID(uploadID)
	if err != nil {
		return nil, meta.ErrMultipartCompletionNotFound
	}
	var (
		key, etag, versionID string
		body                 []byte
		headers              map[string]string
		completedAt          time.Time
	)
	err = s.s.Query(
		`SELECT key, etag, version_id, body, headers, completed_at
		 FROM multipart_completions WHERE bucket_id=? AND upload_id=?`,
		gocqlUUID(bucketID), uploadUUID,
	).WithContext(ctx).Scan(&key, &etag, &versionID, &body, &headers, &completedAt)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrMultipartCompletionNotFound
	}
	if err != nil {
		return nil, err
	}
	return &meta.MultipartCompletion{
		BucketID:    bucketID,
		UploadID:    uploadID,
		Key:         key,
		ETag:        etag,
		VersionID:   versionID,
		Body:        body,
		Headers:     headers,
		CompletedAt: completedAt,
	}, nil
}

func (s *Store) AbortMultipartUpload(ctx context.Context, bucketID uuid.UUID, uploadID string) ([]*data.Manifest, error) {
	uploadUUID, err := gocql.ParseUUID(uploadID)
	if err != nil {
		return nil, meta.ErrMultipartNotFound
	}
	parts, err := s.ListParts(ctx, bucketID, uploadID)
	if err != nil {
		return nil, err
	}
	manifests := make([]*data.Manifest, 0, len(parts))
	for _, p := range parts {
		if p.Manifest != nil {
			manifests = append(manifests, p.Manifest)
		}
	}
	if err := s.s.Query(
		`DELETE FROM multipart_parts WHERE bucket_id=? AND upload_id=?`,
		gocqlUUID(bucketID), uploadUUID,
	).WithContext(ctx).Exec(); err != nil {
		return manifests, err
	}
	if err := s.s.Query(
		`DELETE FROM multipart_uploads WHERE bucket_id=? AND upload_id=?`,
		gocqlUUID(bucketID), uploadUUID,
	).WithContext(ctx).Exec(); err != nil {
		return manifests, err
	}
	return manifests, nil
}

func (s *Store) setBucketBlob(ctx context.Context, table, col string, bucketID uuid.UUID, blob []byte) error {
	q := fmt.Sprintf("INSERT INTO %s (bucket_id, %s) VALUES (?, ?)", table, col)
	return s.s.Query(q, gocqlUUID(bucketID), blob).WithContext(ctx).Exec()
}

func (s *Store) getBucketBlob(ctx context.Context, table, col string, bucketID uuid.UUID, missing error) ([]byte, error) {
	var blob []byte
	q := fmt.Sprintf("SELECT %s FROM %s WHERE bucket_id=?", col, table)
	err := s.s.Query(q, gocqlUUID(bucketID)).WithContext(ctx).Scan(&blob)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, missing
	}
	if err != nil {
		return nil, err
	}
	if len(blob) == 0 {
		return nil, missing
	}
	return blob, nil
}

func (s *Store) deleteBucketBlob(ctx context.Context, table string, bucketID uuid.UUID) error {
	q := fmt.Sprintf("DELETE FROM %s WHERE bucket_id=?", table)
	return s.s.Query(q, gocqlUUID(bucketID)).WithContext(ctx).Exec()
}

func (s *Store) SetBucketCORS(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, "bucket_cors", "rules", bucketID, blob)
}
func (s *Store) GetBucketCORS(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, "bucket_cors", "rules", bucketID, meta.ErrNoSuchCORS)
}
func (s *Store) DeleteBucketCORS(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, "bucket_cors", bucketID)
}

func (s *Store) SetBucketPolicy(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, "bucket_policy", "document", bucketID, blob)
}
func (s *Store) GetBucketPolicy(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, "bucket_policy", "document", bucketID, meta.ErrNoSuchBucketPolicy)
}
func (s *Store) DeleteBucketPolicy(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, "bucket_policy", bucketID)
}

func (s *Store) SetBucketPublicAccessBlock(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, "bucket_public_access_block", "config", bucketID, blob)
}
func (s *Store) GetBucketPublicAccessBlock(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, "bucket_public_access_block", "config", bucketID, meta.ErrNoSuchPublicAccessBlock)
}
func (s *Store) DeleteBucketPublicAccessBlock(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, "bucket_public_access_block", bucketID)
}

func (s *Store) SetBucketOwnershipControls(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, "bucket_ownership_controls", "config", bucketID, blob)
}
func (s *Store) GetBucketOwnershipControls(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, "bucket_ownership_controls", "config", bucketID, meta.ErrNoSuchOwnershipControls)
}
func (s *Store) DeleteBucketOwnershipControls(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, "bucket_ownership_controls", bucketID)
}

func (s *Store) SetBucketEncryption(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, "bucket_encryption", "config", bucketID, blob)
}
func (s *Store) GetBucketEncryption(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, "bucket_encryption", "config", bucketID, meta.ErrNoSuchEncryption)
}
func (s *Store) DeleteBucketEncryption(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, "bucket_encryption", bucketID)
}

// SetBucketPlacement persists policy under bucket addressed by name. Looks
// up the bucket id, validates the policy, and writes the JSON blob via the
// shared setBucketBlob helper into the bucket_placement table (US-001).
func (s *Store) SetBucketPlacement(ctx context.Context, name string, policy map[string]int) error {
	if err := meta.ValidatePlacement(policy); err != nil {
		return err
	}
	b, err := s.GetBucket(ctx, name)
	if err != nil {
		return err
	}
	blob, err := meta.EncodeBucketPlacement(policy)
	if err != nil {
		return err
	}
	return s.setBucketBlob(ctx, "bucket_placement", "policy", b.ID, blob)
}

// GetBucketPlacement returns the configured policy, or (nil, nil) when no
// row exists — NOT an error — so the routing path can fall back to
// $defaultCluster without inspecting the error chain.
func (s *Store) GetBucketPlacement(ctx context.Context, name string) (map[string]int, error) {
	b, err := s.GetBucket(ctx, name)
	if err != nil {
		return nil, err
	}
	var blob []byte
	q := `SELECT policy FROM bucket_placement WHERE bucket_id=?`
	err = s.s.Query(q, gocqlUUID(b.ID)).WithContext(ctx).Scan(&blob)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return meta.DecodeBucketPlacement(blob)
}

// DeleteBucketPlacement drops the row. Idempotent.
func (s *Store) DeleteBucketPlacement(ctx context.Context, name string) error {
	b, err := s.GetBucket(ctx, name)
	if err != nil {
		return err
	}
	return s.deleteBucketBlob(ctx, "bucket_placement", b.ID)
}

// SetClusterState persists state under clusterID. Absence of a row is
// equivalent to meta.ClusterStateLive — operators only need to write
// rows when they want to drain or remove a cluster (US-006).
func (s *Store) SetClusterState(ctx context.Context, clusterID, state string) error {
	if clusterID == "" {
		return meta.ErrUnknownCluster
	}
	if err := meta.ValidateClusterState(state); err != nil {
		return err
	}
	return s.s.Query(
		`INSERT INTO cluster_state (cluster_id, state) VALUES (?, ?)`,
		clusterID, state,
	).WithContext(ctx).Exec()
}

// GetClusterState returns the persisted state. ok=false signals "no row
// exists" — callers should treat that as ClusterStateLive.
func (s *Store) GetClusterState(ctx context.Context, clusterID string) (string, bool, error) {
	var state string
	err := s.s.Query(
		`SELECT state FROM cluster_state WHERE cluster_id=?`,
		clusterID,
	).WithContext(ctx).Scan(&state)
	if errors.Is(err, gocql.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return state, true, nil
}

// ListClusterStates returns every persisted cluster_state row. Backs
// both the drain-state cache and the admin GET /admin/v1/clusters
// handler.
func (s *Store) ListClusterStates(ctx context.Context) (map[string]string, error) {
	iter := s.s.Query(`SELECT cluster_id, state FROM cluster_state`).WithContext(ctx).Iter()
	out := map[string]string{}
	var clusterID, state string
	for iter.Scan(&clusterID, &state) {
		out[clusterID] = state
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteClusterState drops the row. Idempotent — operators undrain a
// cluster by dropping the row (absence == live).
func (s *Store) DeleteClusterState(ctx context.Context, clusterID string) error {
	return s.s.Query(
		`DELETE FROM cluster_state WHERE cluster_id=?`,
		clusterID,
	).WithContext(ctx).Exec()
}

func (s *Store) SetBucketObjectLockEnabled(ctx context.Context, name string, enabled bool) error {
	applied, err := s.s.Query(
		`UPDATE buckets SET object_lock_enabled=? WHERE name=? IF EXISTS`,
		enabled, name,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(nil)
	if err != nil {
		return err
	}
	if !applied {
		return meta.ErrBucketNotFound
	}
	return nil
}

func (s *Store) SetBucketRegion(ctx context.Context, name, region string) error {
	applied, err := s.s.Query(
		`UPDATE buckets SET region=? WHERE name=? IF EXISTS`,
		region, name,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(nil)
	if err != nil {
		return err
	}
	if !applied {
		return meta.ErrBucketNotFound
	}
	return nil
}

func (s *Store) SetBucketMfaDelete(ctx context.Context, name, state string) error {
	applied, err := s.s.Query(
		`UPDATE buckets SET mfa_delete=? WHERE name=? IF EXISTS`,
		state, name,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(nil)
	if err != nil {
		return err
	}
	if !applied {
		return meta.ErrBucketNotFound
	}
	return nil
}

func (s *Store) SetBucketObjectLockConfig(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, "bucket_object_lock", "config", bucketID, blob)
}
func (s *Store) GetBucketObjectLockConfig(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, "bucket_object_lock", "config", bucketID, meta.ErrNoSuchObjectLockConfig)
}
func (s *Store) DeleteBucketObjectLockConfig(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, "bucket_object_lock", bucketID)
}

func (s *Store) SetBucketNotificationConfig(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, "bucket_notification", "config", bucketID, blob)
}
func (s *Store) GetBucketNotificationConfig(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, "bucket_notification", "config", bucketID, meta.ErrNoSuchNotification)
}
func (s *Store) DeleteBucketNotificationConfig(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, "bucket_notification", bucketID)
}

func (s *Store) SetBucketWebsite(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, "bucket_website", "config", bucketID, blob)
}
func (s *Store) GetBucketWebsite(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, "bucket_website", "config", bucketID, meta.ErrNoSuchWebsite)
}
func (s *Store) DeleteBucketWebsite(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, "bucket_website", bucketID)
}

func (s *Store) SetBucketReplication(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, "bucket_replication", "config", bucketID, blob)
}
func (s *Store) GetBucketReplication(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, "bucket_replication", "config", bucketID, meta.ErrNoSuchReplication)
}
func (s *Store) DeleteBucketReplication(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, "bucket_replication", bucketID)
}

func (s *Store) SetBucketLogging(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, "bucket_logging", "config", bucketID, blob)
}
func (s *Store) GetBucketLogging(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, "bucket_logging", "config", bucketID, meta.ErrNoSuchLogging)
}
func (s *Store) DeleteBucketLogging(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, "bucket_logging", bucketID)
}

func (s *Store) SetBucketTagging(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(ctx, "bucket_tagging", "config", bucketID, blob)
}
func (s *Store) GetBucketTagging(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(ctx, "bucket_tagging", "config", bucketID, meta.ErrNoSuchTagSet)
}
func (s *Store) DeleteBucketTagging(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, "bucket_tagging", bucketID)
}

func (s *Store) SetBucketQuota(ctx context.Context, bucketID uuid.UUID, q meta.BucketQuota) error {
	blob, err := meta.EncodeBucketQuota(q)
	if err != nil {
		return err
	}
	return s.setBucketBlob(ctx, "bucket_quota", "config", bucketID, blob)
}

func (s *Store) GetBucketQuota(ctx context.Context, bucketID uuid.UUID) (meta.BucketQuota, bool, error) {
	var blob []byte
	err := s.s.Query(
		`SELECT config FROM bucket_quota WHERE bucket_id=?`,
		gocqlUUID(bucketID),
	).WithContext(ctx).Scan(&blob)
	if errors.Is(err, gocql.ErrNotFound) || (err == nil && len(blob) == 0) {
		return meta.BucketQuota{}, false, nil
	}
	if err != nil {
		return meta.BucketQuota{}, false, err
	}
	q, err := meta.DecodeBucketQuota(blob)
	if err != nil {
		return meta.BucketQuota{}, false, err
	}
	return q, true, nil
}

func (s *Store) DeleteBucketQuota(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(ctx, "bucket_quota", bucketID)
}

func (s *Store) SetUserQuota(ctx context.Context, userName string, q meta.UserQuota) error {
	blob, err := meta.EncodeUserQuota(q)
	if err != nil {
		return err
	}
	return s.s.Query(
		`INSERT INTO user_quota (user_name, config) VALUES (?, ?)`,
		userName, blob,
	).WithContext(ctx).Exec()
}

func (s *Store) GetUserQuota(ctx context.Context, userName string) (meta.UserQuota, bool, error) {
	var blob []byte
	err := s.s.Query(
		`SELECT config FROM user_quota WHERE user_name=?`,
		userName,
	).WithContext(ctx).Scan(&blob)
	if errors.Is(err, gocql.ErrNotFound) || (err == nil && len(blob) == 0) {
		return meta.UserQuota{}, false, nil
	}
	if err != nil {
		return meta.UserQuota{}, false, err
	}
	q, err := meta.DecodeUserQuota(blob)
	if err != nil {
		return meta.UserQuota{}, false, err
	}
	return q, true, nil
}

func (s *Store) DeleteUserQuota(ctx context.Context, userName string) error {
	return s.s.Query(
		`DELETE FROM user_quota WHERE user_name=?`,
		userName,
	).WithContext(ctx).Exec()
}

// GetBucketStats returns the live counter row for the bucket, or zero stats
// when no row exists yet (the first bump lazily upserts).
func (s *Store) GetBucketStats(ctx context.Context, bucketID uuid.UUID) (meta.BucketStats, error) {
	var bytes, objects int64
	var updated time.Time
	err := s.s.Query(
		`SELECT used_bytes, used_objects, updated_at FROM bucket_stats WHERE bucket_id=?`,
		gocqlUUID(bucketID),
	).WithContext(ctx).Scan(&bytes, &objects, &updated)
	if errors.Is(err, gocql.ErrNotFound) {
		return meta.BucketStats{}, nil
	}
	if err != nil {
		return meta.BucketStats{}, err
	}
	return meta.BucketStats{UsedBytes: bytes, UsedObjects: objects, UpdatedAt: updated}, nil
}

// BumpBucketStats atomically applies (deltaBytes, deltaObjects) to the row.
// Read-modify-CAS loop with bounded retry: on first call upserts via INSERT
// IF NOT EXISTS; subsequent bumps re-read + LWT-update IF the prior counter
// values still match. The IF predicate enforces read-after-write coherence
// per the LWT gotcha in CLAUDE.md.
func (s *Store) BumpBucketStats(ctx context.Context, bucketID uuid.UUID, deltaBytes, deltaObjects int64) (meta.BucketStats, error) {
	const maxAttempts = 32
	for attempt := 0; attempt < maxAttempts; attempt++ {
		cur, err := s.GetBucketStats(ctx, bucketID)
		if err != nil {
			return meta.BucketStats{}, err
		}
		next := meta.BucketStats{
			UsedBytes:   cur.UsedBytes + deltaBytes,
			UsedObjects: cur.UsedObjects + deltaObjects,
			UpdatedAt:   time.Now().UTC(),
		}
		if cur.UpdatedAt.IsZero() {
			applied, err := s.s.Query(
				`INSERT INTO bucket_stats (bucket_id, used_bytes, used_objects, updated_at)
				   VALUES (?, ?, ?, ?) IF NOT EXISTS`,
				gocqlUUID(bucketID), next.UsedBytes, next.UsedObjects, next.UpdatedAt,
			).WithContext(ctx).MapScanCAS(map[string]interface{}{})
			if err != nil {
				return meta.BucketStats{}, err
			}
			if applied {
				return next, nil
			}
			continue
		}
		applied, err := s.s.Query(
			`UPDATE bucket_stats SET used_bytes=?, used_objects=?, updated_at=?
			   WHERE bucket_id=? IF used_bytes=? AND used_objects=?`,
			next.UsedBytes, next.UsedObjects, next.UpdatedAt,
			gocqlUUID(bucketID), cur.UsedBytes, cur.UsedObjects,
		).WithContext(ctx).MapScanCAS(map[string]interface{}{})
		if err != nil {
			return meta.BucketStats{}, err
		}
		if applied {
			return next, nil
		}
	}
	return meta.BucketStats{}, fmt.Errorf("cassandra: bucket_stats CAS exhausted retries for bucket %s", bucketID)
}

// WriteUsageAggregate persists one row in the (bucket, storageClass, day)
// rollup feed (US-008). Day is normalised to UTC midnight before write so
// gocql's `date` codec round-trips cleanly. A second row is also written to
// usage_aggregates_classes so ListUserUsage can enumerate the storage
// classes seen by a bucket without a cluster-wide scan.
func (s *Store) WriteUsageAggregate(ctx context.Context, agg meta.UsageAggregate) error {
	day := normalizeUsageDay(agg.Day)
	computed := agg.ComputedAt
	if computed.IsZero() {
		computed = time.Now().UTC()
	}
	if err := s.s.Query(
		`INSERT INTO usage_aggregates (
			bucket_id, storage_class, day,
			byte_seconds, object_count_avg, object_count_max, computed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		gocqlUUID(agg.BucketID), agg.StorageClass, day,
		agg.ByteSeconds, agg.ObjectCountAvg, agg.ObjectCountMax, computed,
	).WithContext(ctx).Exec(); err != nil {
		return err
	}
	return s.s.Query(
		`INSERT INTO usage_aggregates_classes (bucket_id, storage_class) VALUES (?, ?)`,
		gocqlUUID(agg.BucketID), agg.StorageClass,
	).WithContext(ctx).Exec()
}

func (s *Store) ListUsageAggregates(ctx context.Context, bucketID uuid.UUID, storageClass string, dayFrom, dayTo time.Time) ([]meta.UsageAggregate, error) {
	from := normalizeUsageDay(dayFrom)
	to := normalizeUsageDay(dayTo)
	bucketName := s.bucketNameOrEmpty(bucketID)
	// Empty storageClass means "all classes recorded for this bucket" — fan
	// out across the sibling presence index so the caller can render an
	// operator-facing usage feed without picking a class up front.
	classes := []string{storageClass}
	if storageClass == "" {
		discovered, derr := s.usageStorageClassesForBucket(ctx, bucketID)
		if derr != nil {
			return nil, derr
		}
		classes = discovered
	}
	var out []meta.UsageAggregate
	for _, cls := range classes {
		iter := s.s.Query(
			`SELECT day, byte_seconds, object_count_avg, object_count_max, computed_at
			   FROM usage_aggregates
			  WHERE bucket_id=? AND storage_class=? AND day >= ? AND day < ?`,
			gocqlUUID(bucketID), cls, from, to,
		).WithContext(ctx).Iter()
		var day time.Time
		var byteSeconds, oavg, omax int64
		var computed time.Time
		for iter.Scan(&day, &byteSeconds, &oavg, &omax, &computed) {
			out = append(out, meta.UsageAggregate{
				BucketID:       bucketID,
				Bucket:         bucketName,
				StorageClass:   cls,
				Day:            day.UTC(),
				ByteSeconds:    byteSeconds,
				ObjectCountAvg: oavg,
				ObjectCountMax: omax,
				ComputedAt:     computed,
			})
		}
		if err := iter.Close(); err != nil {
			return nil, err
		}
	}
	if storageClass == "" {
		sort.Slice(out, func(i, j int) bool {
			if !out[i].Day.Equal(out[j].Day) {
				return out[i].Day.Before(out[j].Day)
			}
			return out[i].StorageClass < out[j].StorageClass
		})
	}
	return out, nil
}

func (s *Store) ListUserUsage(ctx context.Context, userName string, dayFrom, dayTo time.Time) ([]meta.UsageAggregate, error) {
	buckets, err := s.ListBuckets(ctx, userName)
	if err != nil {
		return nil, err
	}
	type sumKey struct {
		StorageClass string
		Day          int64
	}
	sums := make(map[sumKey]meta.UsageAggregate)
	from := normalizeUsageDay(dayFrom)
	to := normalizeUsageDay(dayTo)
	for _, b := range buckets {
		// The (bucket_id, storage_class) partition key forces a per-class
		// fan-out. Read the usage_aggregates_classes index to discover which
		// classes a bucket has ever rolled up, then issue one ListUsageAggregates
		// per (bucket, class). v1 keeps the class set tiny (typically just
		// STANDARD), so the fan-out is bounded.
		classes, cerr := s.usageStorageClassesForBucket(ctx, b.ID)
		if cerr != nil {
			return nil, cerr
		}
		for _, cls := range classes {
			rows, lerr := s.ListUsageAggregates(ctx, b.ID, cls, from, to)
			if lerr != nil {
				return nil, lerr
			}
			for _, r := range rows {
				k := sumKey{StorageClass: r.StorageClass, Day: r.Day.Unix()}
				acc := sums[k]
				acc.StorageClass = r.StorageClass
				acc.Day = r.Day
				acc.ByteSeconds += r.ByteSeconds
				acc.ObjectCountAvg += r.ObjectCountAvg
				if r.ObjectCountMax > acc.ObjectCountMax {
					acc.ObjectCountMax = r.ObjectCountMax
				}
				if r.ComputedAt.After(acc.ComputedAt) {
					acc.ComputedAt = r.ComputedAt
				}
				sums[k] = acc
			}
		}
	}
	out := make([]meta.UsageAggregate, 0, len(sums))
	for _, v := range sums {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Day.Equal(out[j].Day) {
			return out[i].Day.Before(out[j].Day)
		}
		return out[i].StorageClass < out[j].StorageClass
	})
	return out, nil
}

// usageStorageClassesForBucket returns the distinct storage classes that
// have ever been rolled up for bucketID. Backed by the
// usage_aggregates_classes index table that WriteUsageAggregate maintains
// alongside the main rollup row.
func (s *Store) usageStorageClassesForBucket(ctx context.Context, bucketID uuid.UUID) ([]string, error) {
	iter := s.s.Query(
		`SELECT storage_class FROM usage_aggregates_classes WHERE bucket_id=?`,
		gocqlUUID(bucketID),
	).WithContext(ctx).Iter()
	defer iter.Close()
	var out []string
	var cls string
	for iter.Scan(&cls) {
		out = append(out, cls)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) bucketNameOrEmpty(bucketID uuid.UUID) string {
	s.bucketNamesMu.RLock()
	name, ok := s.bucketNames[bucketID]
	s.bucketNamesMu.RUnlock()
	if ok {
		return name
	}
	return ""
}

func normalizeUsageDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func (s *Store) SetBucketInventoryConfig(ctx context.Context, bucketID uuid.UUID, configID string, blob []byte) error {
	return s.s.Query(
		`INSERT INTO bucket_inventory_configs (bucket_id, config_id, config) VALUES (?, ?, ?)`,
		gocqlUUID(bucketID), configID, blob,
	).WithContext(ctx).Exec()
}

func (s *Store) GetBucketInventoryConfig(ctx context.Context, bucketID uuid.UUID, configID string) ([]byte, error) {
	var blob []byte
	err := s.s.Query(
		`SELECT config FROM bucket_inventory_configs WHERE bucket_id=? AND config_id=?`,
		gocqlUUID(bucketID), configID,
	).WithContext(ctx).Scan(&blob)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrNoSuchInventoryConfig
	}
	if err != nil {
		return nil, err
	}
	if len(blob) == 0 {
		return nil, meta.ErrNoSuchInventoryConfig
	}
	return blob, nil
}

func (s *Store) DeleteBucketInventoryConfig(ctx context.Context, bucketID uuid.UUID, configID string) error {
	return s.s.Query(
		`DELETE FROM bucket_inventory_configs WHERE bucket_id=? AND config_id=?`,
		gocqlUUID(bucketID), configID,
	).WithContext(ctx).Exec()
}

func (s *Store) ListBucketInventoryConfigs(ctx context.Context, bucketID uuid.UUID) (map[string][]byte, error) {
	iter := s.s.Query(
		`SELECT config_id, config FROM bucket_inventory_configs WHERE bucket_id=?`,
		gocqlUUID(bucketID),
	).WithContext(ctx).Iter()
	out := make(map[string][]byte)
	var id string
	var blob []byte
	for iter.Scan(&id, &blob) {
		if len(blob) == 0 {
			continue
		}
		out[id] = append([]byte(nil), blob...)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) CreateAccessPoint(ctx context.Context, ap *meta.AccessPoint) error {
	applied, err := s.s.Query(
		`INSERT INTO access_points
			(name, bucket_id, bucket, alias, network_origin, vpc_id, policy, public_access_block, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) IF NOT EXISTS`,
		ap.Name, gocqlUUID(ap.BucketID), ap.Bucket, ap.Alias, ap.NetworkOrigin,
		ap.VPCID, nilIfEmptyBytes(ap.Policy), nilIfEmptyBytes(ap.PublicAccessBlock),
		ap.CreatedAt.UTC(),
	).WithContext(ctx).MapScanCAS(map[string]interface{}{})
	if err != nil {
		return err
	}
	if !applied {
		return meta.ErrAccessPointAlreadyExists
	}
	return nil
}

func (s *Store) GetAccessPoint(ctx context.Context, name string) (*meta.AccessPoint, error) {
	var (
		bucketID      gocql.UUID
		bucket        string
		alias         string
		networkOrigin string
		vpcID         string
		policy        []byte
		pab           []byte
		createdAt     time.Time
	)
	err := s.s.Query(
		`SELECT bucket_id, bucket, alias, network_origin, vpc_id, policy, public_access_block, created_at
		 FROM access_points WHERE name=?`,
		name,
	).WithContext(ctx).Scan(&bucketID, &bucket, &alias, &networkOrigin, &vpcID, &policy, &pab, &createdAt)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrAccessPointNotFound
	}
	if err != nil {
		return nil, err
	}
	return &meta.AccessPoint{
		Name:              name,
		BucketID:          uuidFromGocql(bucketID),
		Bucket:            bucket,
		Alias:             alias,
		NetworkOrigin:     networkOrigin,
		VPCID:             vpcID,
		Policy:            append([]byte(nil), policy...),
		PublicAccessBlock: append([]byte(nil), pab...),
		CreatedAt:         createdAt,
	}, nil
}

func (s *Store) GetAccessPointByAlias(ctx context.Context, alias string) (*meta.AccessPoint, error) {
	var (
		name          string
		bucketID      gocql.UUID
		bucket        string
		networkOrigin string
		vpcID         string
		policy        []byte
		pab           []byte
		createdAt     time.Time
	)
	err := s.s.Query(
		`SELECT name, bucket_id, bucket, network_origin, vpc_id, policy, public_access_block, created_at
		 FROM access_points WHERE alias=? LIMIT 1 ALLOW FILTERING`,
		alias,
	).WithContext(ctx).Scan(&name, &bucketID, &bucket, &networkOrigin, &vpcID, &policy, &pab, &createdAt)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrAccessPointNotFound
	}
	if err != nil {
		return nil, err
	}
	return &meta.AccessPoint{
		Name:              name,
		BucketID:          uuidFromGocql(bucketID),
		Bucket:            bucket,
		Alias:             alias,
		NetworkOrigin:     networkOrigin,
		VPCID:             vpcID,
		Policy:            append([]byte(nil), policy...),
		PublicAccessBlock: append([]byte(nil), pab...),
		CreatedAt:         createdAt,
	}, nil
}

func (s *Store) DeleteAccessPoint(ctx context.Context, name string) error {
	applied, err := s.s.Query(
		`DELETE FROM access_points WHERE name=? IF EXISTS`,
		name,
	).WithContext(ctx).MapScanCAS(map[string]interface{}{})
	if err != nil {
		return err
	}
	if !applied {
		return meta.ErrAccessPointNotFound
	}
	return nil
}

func (s *Store) ListAccessPoints(ctx context.Context, bucketID uuid.UUID) ([]*meta.AccessPoint, error) {
	var iter *gocql.Iter
	if bucketID == uuid.Nil {
		iter = s.s.Query(
			`SELECT name, bucket_id, bucket, alias, network_origin, vpc_id, policy, public_access_block, created_at FROM access_points`,
		).WithContext(ctx).Iter()
	} else {
		iter = s.s.Query(
			`SELECT name, bucket_id, bucket, alias, network_origin, vpc_id, policy, public_access_block, created_at FROM access_points WHERE bucket_id=? ALLOW FILTERING`,
			gocqlUUID(bucketID),
		).WithContext(ctx).Iter()
	}
	var out []*meta.AccessPoint
	var (
		name          string
		bID           gocql.UUID
		bucket        string
		alias         string
		networkOrigin string
		vpcID         string
		policy        []byte
		pab           []byte
		createdAt     time.Time
	)
	for iter.Scan(&name, &bID, &bucket, &alias, &networkOrigin, &vpcID, &policy, &pab, &createdAt) {
		out = append(out, &meta.AccessPoint{
			Name:              name,
			BucketID:          uuidFromGocql(bID),
			Bucket:            bucket,
			Alias:             alias,
			NetworkOrigin:     networkOrigin,
			VPCID:             vpcID,
			Policy:            append([]byte(nil), policy...),
			PublicAccessBlock: append([]byte(nil), pab...),
			CreatedAt:         createdAt,
		})
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) UpdateObjectSSEWrap(ctx context.Context, bucketID uuid.UUID, key, versionID string, wrapped []byte, keyID string) error {
	shard := shardOf(key, s.defaultShard)
	if versionID == "" {
		o, err := s.GetObject(ctx, bucketID, key, "")
		if err != nil {
			return err
		}
		versionID = o.VersionID
	}
	vUUID, err := versionToCQL(versionID)
	if err != nil {
		return meta.ErrObjectNotFound
	}
	return s.s.Query(
		`UPDATE objects SET sse_key=?, sse_key_id=?
		 WHERE bucket_id=? AND shard=? AND key=? AND version_id=?`,
		nilIfEmptyBytes(wrapped), nilIfEmpty(keyID),
		gocqlUUID(bucketID), shard, key, vUUID,
	).WithContext(ctx).Exec()
}

func (s *Store) UpdateMultipartUploadSSEWrap(ctx context.Context, bucketID uuid.UUID, uploadID string, wrapped []byte, keyID string) error {
	uploadUUID, err := gocql.ParseUUID(uploadID)
	if err != nil {
		return meta.ErrMultipartNotFound
	}
	return s.s.Query(
		`UPDATE multipart_uploads SET sse_key=?, sse_key_id=?
		 WHERE bucket_id=? AND upload_id=?`,
		nilIfEmptyBytes(wrapped), nilIfEmpty(keyID),
		gocqlUUID(bucketID), uploadUUID,
	).WithContext(ctx).Exec()
}

// GetObjectManifestRaw returns the raw on-disk manifest blob for the given
// object version (US-049). When versionID is "" or "null" the latest row is
// returned. Returns ErrObjectNotFound when no row matches.
func (s *Store) GetObjectManifestRaw(ctx context.Context, bucketID uuid.UUID, key, versionID string) ([]byte, error) {
	resolved := meta.ResolveVersionID(versionID)
	if resolved == "" {
		o, err := s.GetObject(ctx, bucketID, key, "")
		if err != nil {
			return nil, err
		}
		resolved = o.VersionID
	}
	vUUID, err := versionToCQL(resolved)
	if err != nil {
		return nil, meta.ErrObjectNotFound
	}
	shard := shardOf(key, s.defaultShard)
	var blob []byte
	err = s.s.Query(
		`SELECT manifest FROM objects WHERE bucket_id=? AND shard=? AND key=? AND version_id=?`,
		gocqlUUID(bucketID), shard, key, vUUID,
	).WithContext(ctx).Scan(&blob)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrObjectNotFound
	}
	if err != nil {
		return nil, err
	}
	return blob, nil
}

// UpdateObjectManifestRaw overwrites just the manifest column for the given
// object version. The store does not validate the bytes; callers are
// expected to pass a valid JSON or proto manifest blob.
func (s *Store) UpdateObjectManifestRaw(ctx context.Context, bucketID uuid.UUID, key, versionID string, raw []byte) error {
	resolved := meta.ResolveVersionID(versionID)
	if resolved == "" {
		o, err := s.GetObject(ctx, bucketID, key, "")
		if err != nil {
			return err
		}
		resolved = o.VersionID
	}
	vUUID, err := versionToCQL(resolved)
	if err != nil {
		return meta.ErrObjectNotFound
	}
	shard := shardOf(key, s.defaultShard)
	return s.s.Query(
		`UPDATE objects SET manifest=? WHERE bucket_id=? AND shard=? AND key=? AND version_id=?`,
		nilIfEmptyBytes(raw),
		gocqlUUID(bucketID), shard, key, vUUID,
	).WithContext(ctx).Exec()
}

func (s *Store) SetRewrapProgress(ctx context.Context, p *meta.RewrapProgress) error {
	if p == nil {
		return nil
	}
	return s.s.Query(
		`INSERT INTO rewrap_progress (bucket_id, target_id, last_key, complete, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		gocqlUUID(p.BucketID), p.TargetID, p.LastKey, p.Complete, time.Now().UTC(),
	).WithContext(ctx).Exec()
}

func (s *Store) GetRewrapProgress(ctx context.Context, bucketID uuid.UUID) (*meta.RewrapProgress, error) {
	var (
		targetID, lastKey string
		complete          bool
		updatedAt         time.Time
	)
	err := s.s.Query(
		`SELECT target_id, last_key, complete, updated_at FROM rewrap_progress WHERE bucket_id=?`,
		gocqlUUID(bucketID),
	).WithContext(ctx).Scan(&targetID, &lastKey, &complete, &updatedAt)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrNoRewrapProgress
	}
	if err != nil {
		return nil, err
	}
	return &meta.RewrapProgress{
		BucketID:  bucketID,
		TargetID:  targetID,
		LastKey:   lastKey,
		Complete:  complete,
		UpdatedAt: updatedAt,
	}, nil
}

func gocqlUUID(u uuid.UUID) gocql.UUID {
	var g gocql.UUID
	copy(g[:], u[:])
	return g
}

func uuidFromGocql(g gocql.UUID) uuid.UUID {
	var u uuid.UUID
	copy(u[:], g[:])
	return u
}

// cassandraNullSentinel is a valid v1 timeuuid (version=1, variant=8, all
// other bits zero) used inside the Cassandra backend to represent the null
// version. The all-zero UUID exposed via meta.NullVersionID is not a valid
// timeuuid (Cassandra server-side validation rejects it on INSERT into
// `objects.version_id`), but we still want a single deterministic sentinel
// that sorts last under the `version_id DESC` clustering order. The chosen
// value has timestamp=0, so it sorts after every real v1 TimeUUID emitted at
// PUT time. Translation between the wire form (cassandra) and the canonical
// meta.NullVersionID happens at every gocql.ParseUUID / .String() boundary.
const cassandraNullSentinel = "00000000-0000-1000-8000-000000000000"

// versionToCQL maps the canonical meta.NullVersionID (zero UUID) onto the
// cassandra-internal v1 sentinel before going to the wire. All other inputs
// pass through gocql.ParseUUID unchanged. Empty strings are an error.
func versionToCQL(s string) (gocql.UUID, error) {
	if s == meta.NullVersionID {
		return gocql.ParseUUID(cassandraNullSentinel)
	}
	return gocql.ParseUUID(s)
}

// versionFromCQL is the inverse of versionToCQL — it converts a gocql.UUID
// scanned out of `objects.version_id` back into the canonical sentinel form
// when it matches the cassandra-internal v1 sentinel.
func versionFromCQL(g gocql.UUID) string {
	if g.String() == cassandraNullSentinel {
		return meta.NullVersionID
	}
	return g.String()
}
