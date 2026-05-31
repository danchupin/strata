package cassandra

import (
	"context"
	"errors"
	"time"

	"github.com/gocql/gocql"
	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// objectSelectCols is the full non-partition-key projection of the `objects`
// table. scanObjectLimit1 / scanObjectByVersion / scanAllVersionsAtShard all
// decode exactly these columns (in this order) via scanObjectQuery — keep the
// SELECT order in lockstep with the Scan order in scanObjectQuery and the INSERT
// order in insertObjectRowAtShard / PutObject.
const objectSelectCols = `version_id, is_latest, is_delete_marker, size, etag, content_type,
	        storage_class, mtime, manifest, user_meta, tags,
	        retain_until, retain_mode, legal_hold, checksums, sse, ssec_key_md5, restore_status,
	        cache_control, expires, parts_count, sse_key, sse_key_id, replication_status, part_sizes, checksum_type, is_null`

// MigrateReshardKey relocates every version row of key from its source shard
// (fnv%active) to its target shard (fnv%target) for the in-flight reshard on
// bucketID, then deletes the source orphans. It is the per-key unit the US-003
// reshard worker drives to physically move rows before CompleteReshard flips the
// active count (the US-002 transitional union read keeps the key visible from the
// source layout throughout, so cleanup-before-flip is safe: the source row is
// deleted only after its target copy exists).
//
// Idempotent + crash-safe:
//   - INSERT … IF NOT EXISTS into the target so a newer concurrent client write
//     that already landed in the target layout (PutObject routes writes there
//     during a reshard) is never clobbered by the older source row.
//   - DELETE of the source row is unconditional but a no-op once already gone, so
//     a re-run after a partial move converges. A row present in both partitions
//     (crash between INSERT and DELETE) is reconciled by re-running: the target
//     INSERT IF NOT EXISTS skips, the source DELETE removes the orphan.
//
// Returns 0 (no error) when no reshard is in flight (target<=0), the target is
// not a strict resize (target<=active), or the key does not diverge
// (src==dst) — its row already sits in the post-flip partition.
//
// KNOWN NARROW RACE (copy-first is deliberate, see ROADMAP "reshard migration
// vs concurrent specific-version DELETE"): a client DELETE of the exact version
// being migrated, landing AFTER this call's source read but BEFORE its target
// INSERT, can resurrect that version in the target (the client cleared both
// layouts on an empty target; the IF-NOT-EXISTS insert then re-creates it). The
// alternative — delete source first — would open a window where the key is in
// neither partition, breaking the US-002 "stable key set" guarantee, so copy-
// first is the right trade. The window is microseconds and only during an active
// reshard; US-005's concurrent-write smoke stresses the common paths.
func (s *Store) MigrateReshardKey(ctx context.Context, bucketID uuid.UUID, key string) (int, error) {
	active, target, err := s.bucketShardCounts(ctx, bucketID)
	if err != nil {
		return 0, err
	}
	if target <= 0 || target <= active {
		return 0, nil
	}
	src := shardOf(key, active)
	dst := shardOf(key, target)
	if src == dst {
		return 0, nil
	}
	rows, err := s.scanAllVersionsAtShard(ctx, bucketID, src, key)
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, o := range rows {
		vUUID, err := versionToCQL(o.VersionID)
		if err != nil {
			return moved, err
		}
		applied, err := s.insertObjectRowAtShard(ctx, dst, key, vUUID, o, true)
		if err != nil {
			return moved, err
		}
		if err := s.s.Query(
			`DELETE FROM objects WHERE bucket_id=? AND shard=? AND key=? AND version_id=?`,
			gocqlUUID(bucketID), src, key, vUUID,
		).WithContext(ctx).Exec(); err != nil {
			return moved, err
		}
		// Count only rows this call actually relocated: applied=false means the
		// target already held the version (a newer concurrent write, or a re-run
		// after a partial crash), so it is not a fresh move.
		if applied {
			moved++
		}
	}
	return moved, nil
}

// scanAllVersionsAtShard reads every stored version row of key within one shard
// partition (full column projection), newest version first (clustering order).
// Returns an empty slice (no error) when the partition holds no row for the key.
func (s *Store) scanAllVersionsAtShard(ctx context.Context, bucketID uuid.UUID, shard int, key string) ([]*meta.Object, error) {
	iter := s.s.Query(
		`SELECT `+objectSelectCols+`
		 FROM objects WHERE bucket_id=? AND shard=? AND key=?`,
		gocqlUUID(bucketID), shard, key,
	).WithContext(ctx).PageSize(2000).Iter()

	var out []*meta.Object
	for {
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
		if !iter.Scan(&versionUUID, &isLatest, &isDeleteMark, &size, &etag, &ctype,
			&class, &mtime, &manifestBlob, &userMeta, &tags,
			&retainUntil, &retainMode, &legalHold, &checksums, &sse, &ssecKeyMD5, &restore,
			&cacheControl, &expires, &partsCount, &sseKey, &sseKeyID, &replication, &partSizes, &checksumType, &isNull) {
			break
		}
		m, err := decodeManifest(manifestBlob)
		if err != nil {
			_ = iter.Close()
			return nil, err
		}
		out = append(out, &meta.Object{
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
		})
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// insertObjectRowAtShard writes o's full column set into an explicit shard
// partition under an explicit version_id, preserving every field verbatim (NO
// bucket_stats bump — a reshard move conserves total bytes/objects). When
// ifNotExists is set the write is an LWT INSERT … IF NOT EXISTS and the returned
// bool reports whether it applied (false = the target already holds this
// (key, version_id), so the migration leaves the existing — newer — row intact).
// The column list MUST stay in lockstep with PutObject's INSERT.
func (s *Store) insertObjectRowAtShard(ctx context.Context, shard int, key string, versionID gocql.UUID, o *meta.Object, ifNotExists bool) (bool, error) {
	manifestBlob, err := encodeManifest(o.Manifest)
	if err != nil {
		return false, err
	}
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
	const cols = `INSERT INTO objects (bucket_id, shard, key, version_id, is_latest, is_delete_marker,
		 size, etag, content_type, storage_class, mtime, manifest, user_meta, tags,
		 retain_until, retain_mode, legal_hold, checksums, sse, ssec_key_md5, restore_status,
		 cache_control, expires, parts_count, sse_key, sse_key_id, replication_status, part_sizes, checksum_type, is_null)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	args := []interface{}{
		gocqlUUID(o.BucketID), shard, key, versionID, o.IsLatest, o.IsDeleteMarker,
		o.Size, o.ETag, o.ContentType, o.StorageClass, o.Mtime, manifestBlob, o.UserMeta, o.Tags,
		retainUntil, nilIfEmpty(o.RetainMode), o.LegalHold, o.Checksums, nilIfEmpty(o.SSE), nilIfEmpty(o.SSECKeyMD5), nilIfEmpty(o.RestoreStatus),
		nilIfEmpty(o.CacheControl), nilIfEmpty(o.Expires), partsCount, nilIfEmptyBytes(o.SSEKey), nilIfEmpty(o.SSEKeyID), nilIfEmpty(o.ReplicationStatus), partSizes, nilIfEmpty(o.ChecksumType), o.IsNull,
	}
	if !ifNotExists {
		return true, s.s.Query(cols, args...).WithContext(ctx).Exec()
	}
	// MapScanCAS (not ScanCAS(nil...)) for INSERT … IF NOT EXISTS — the CAS row
	// returns every column on a non-apply, and ScanCAS's positional binding
	// silently mis-counts (the CreateBucket column-count bug). The map is
	// discarded; we only need the applied flag.
	applied, err := s.s.Query(cols+` IF NOT EXISTS`, args...).
		WithContext(ctx).SerialConsistency(gocql.LocalSerial).MapScanCAS(make(map[string]interface{}))
	return applied, err
}

// StartReshard inserts a new reshard_jobs row IF NOT EXISTS and stamps the
// bucket's shard_count_target column. Both writes are LWT so a concurrent
// retry surfaces ErrReshardInProgress instead of racing two workers.
func (s *Store) StartReshard(ctx context.Context, bucketID uuid.UUID, target int) (*meta.ReshardJob, error) {
	if !meta.IsValidShardCount(target) {
		return nil, meta.ErrReshardInvalidTarget
	}
	b, err := s.getBucketByID(ctx, bucketID)
	if err != nil {
		return nil, err
	}
	if target <= b.ShardCount {
		return nil, meta.ErrReshardInvalidTarget
	}
	now := time.Now().UTC()
	job := &meta.ReshardJob{
		BucketID:  bucketID,
		Bucket:    b.Name,
		Source:    b.ShardCount,
		Target:    target,
		CreatedAt: now,
		UpdatedAt: now,
	}
	existing := make(map[string]interface{})
	applied, err := s.s.Query(
		`INSERT INTO reshard_jobs (bucket_id, bucket_name, source, target, last_key, done, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?) IF NOT EXISTS`,
		gocqlUUID(bucketID), b.Name, job.Source, job.Target, "", false, now, now,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).MapScanCAS(existing)
	if err != nil {
		return nil, err
	}
	if !applied {
		return nil, meta.ErrReshardInProgress
	}
	if _, err := s.s.Query(
		`UPDATE buckets SET shard_count_target=? WHERE name=? IF EXISTS`,
		target, b.Name,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(nil); err != nil {
		return nil, err
	}
	// Drop the cached layout so in-flight object ops re-read and pick up the
	// new shard_count_target (US-001 cache + US-002 transitional routing).
	s.invalidateShardCache(bucketID)
	return job, nil
}

func (s *Store) GetReshardJob(ctx context.Context, bucketID uuid.UUID) (*meta.ReshardJob, error) {
	job, err := s.scanReshardJob(ctx, bucketID)
	if err != nil {
		return nil, err
	}
	return job, nil
}

func (s *Store) UpdateReshardJob(ctx context.Context, job *meta.ReshardJob) error {
	if job == nil {
		return nil
	}
	now := time.Now().UTC()
	applied, err := s.s.Query(
		`UPDATE reshard_jobs SET last_key=?, done=?, updated_at=? WHERE bucket_id=? IF EXISTS`,
		job.LastKey, job.Done, now, gocqlUUID(job.BucketID),
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(nil)
	if err != nil {
		return err
	}
	if !applied {
		return meta.ErrReshardNotFound
	}
	return nil
}

func (s *Store) CompleteReshard(ctx context.Context, bucketID uuid.UUID) error {
	job, err := s.scanReshardJob(ctx, bucketID)
	if err != nil {
		return err
	}
	if _, err := s.s.Query(
		`UPDATE buckets SET shard_count=?, shard_count_target=? WHERE name=? IF EXISTS`,
		job.Target, 0, job.Bucket,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(nil); err != nil {
		return err
	}
	// Synchronously invalidate so the very next object op resolves the flipped
	// active shard_count instead of a stale cached layout (US-001).
	s.invalidateShardCache(bucketID)
	return s.s.Query(
		`DELETE FROM reshard_jobs WHERE bucket_id=?`,
		gocqlUUID(bucketID),
	).WithContext(ctx).Exec()
}

func (s *Store) ListReshardJobs(ctx context.Context) ([]*meta.ReshardJob, error) {
	iter := s.s.Query(
		`SELECT bucket_id, bucket_name, source, target, last_key, done, created_at, updated_at FROM reshard_jobs`,
	).WithContext(ctx).Iter()
	defer iter.Close()

	var (
		out        []*meta.ReshardJob
		idG        gocql.UUID
		bucketName string
		source     int
		target     int
		lastKey    string
		done       bool
		createdAt  time.Time
		updatedAt  time.Time
	)
	for iter.Scan(&idG, &bucketName, &source, &target, &lastKey, &done, &createdAt, &updatedAt) {
		out = append(out, &meta.ReshardJob{
			BucketID:  uuidFromGocql(idG),
			Bucket:    bucketName,
			Source:    source,
			Target:    target,
			LastKey:   lastKey,
			Done:      done,
			CreatedAt: createdAt,
			UpdatedAt: updatedAt,
		})
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) scanReshardJob(ctx context.Context, bucketID uuid.UUID) (*meta.ReshardJob, error) {
	var (
		bucketName string
		source     int
		target     int
		lastKey    string
		done       bool
		createdAt  time.Time
		updatedAt  time.Time
	)
	err := s.s.Query(
		`SELECT bucket_name, source, target, last_key, done, created_at, updated_at
		 FROM reshard_jobs WHERE bucket_id=?`,
		gocqlUUID(bucketID),
	).WithContext(ctx).Scan(&bucketName, &source, &target, &lastKey, &done, &createdAt, &updatedAt)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrReshardNotFound
	}
	if err != nil {
		return nil, err
	}
	return &meta.ReshardJob{
		BucketID:  bucketID,
		Bucket:    bucketName,
		Source:    source,
		Target:    target,
		LastKey:   lastKey,
		Done:      done,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}, nil
}

// getBucketByID is a small helper used by reshard ops that arrive with a UUID
// rather than a bucket name.
func (s *Store) getBucketByID(ctx context.Context, bucketID uuid.UUID) (*meta.Bucket, error) {
	buckets, err := s.ListBuckets(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, b := range buckets {
		if b.ID == bucketID {
			return b, nil
		}
	}
	return nil, meta.ErrBucketNotFound
}
