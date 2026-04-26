package cassandra

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/gocql/gocql"
	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

type Store struct {
	s            *gocql.Session
	defaultShard int
}

type Options struct {
	DefaultShardCount int
}

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
	return &Store{s: s, defaultShard: opts.DefaultShardCount}, nil
}

func (s *Store) Close() error {
	s.s.Close()
	return nil
}

// Session exposes the underlying gocql session for auxiliary subsystems
// (leader election, admin tooling) that need direct CQL access.
func (s *Store) Session() *gocql.Session { return s.s }

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
		`INSERT INTO buckets (name, id, owner_id, created_at, versioning, default_class, shard_count)
		 VALUES (?, ?, ?, ?, ?, ?, ?) IF NOT EXISTS`,
		name, gocqlUUID(id), owner, b.CreatedAt, meta.VersioningDisabled, defaultClass, s.defaultShard,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).MapScanCAS(existing)
	if err != nil {
		return nil, err
	}
	if !applied {
		return nil, meta.ErrBucketAlreadyExists
	}
	return b, nil
}

func (s *Store) GetBucket(ctx context.Context, name string) (*meta.Bucket, error) {
	var (
		idG               gocql.UUID
		owner, class      string
		versioning, acl   string
		createdAt         time.Time
		shardCount        int
		objectLockEnabled bool
	)
	err := s.s.Query(
		`SELECT id, owner_id, created_at, default_class, versioning, shard_count, acl, object_lock_enabled FROM buckets WHERE name=?`,
		name,
	).WithContext(ctx).Scan(&idG, &owner, &createdAt, &class, &versioning, &shardCount, &acl, &objectLockEnabled)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrBucketNotFound
	}
	if err != nil {
		return nil, err
	}
	if versioning == "" {
		versioning = meta.VersioningDisabled
	}
	return &meta.Bucket{
		Name:              name,
		ID:                uuidFromGocql(idG),
		Owner:             owner,
		CreatedAt:         createdAt,
		DefaultClass:      class,
		Versioning:        versioning,
		ACL:               acl,
		ObjectLockEnabled: objectLockEnabled,
	}, nil
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
	iter := s.s.Query(`SELECT name, id, owner_id, created_at, default_class, versioning, acl FROM buckets`).
		WithContext(ctx).Iter()
	defer iter.Close()

	var (
		out                                   []*meta.Bucket
		name, ownerID, class, versioning, acl string
		idG                                   gocql.UUID
		createdAt                             time.Time
	)
	for iter.Scan(&name, &idG, &ownerID, &createdAt, &class, &versioning, &acl) {
		if owner != "" && ownerID != owner {
			continue
		}
		if versioning == "" {
			versioning = meta.VersioningDisabled
		}
		out = append(out, &meta.Bucket{
			Name:         name,
			ID:           uuidFromGocql(idG),
			Owner:        ownerID,
			CreatedAt:    createdAt,
			DefaultClass: class,
			Versioning:   versioning,
			ACL:          acl,
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
	if o.VersionID != "" {
		parsed, err := gocql.ParseUUID(o.VersionID)
		if err != nil {
			return err
		}
		versionID = parsed
	} else {
		versionID = gocql.TimeUUID()
		o.VersionID = versionID.String()
	}
	if !versioned {
		if err := s.s.Query(
			`DELETE FROM objects WHERE bucket_id=? AND shard=? AND key=?`,
			gocqlUUID(o.BucketID), shard, o.Key,
		).WithContext(ctx).Exec(); err != nil {
			return err
		}
	}
	var retainUntil *time.Time
	if !o.RetainUntil.IsZero() {
		t := o.RetainUntil
		retainUntil = &t
	}
	return s.s.Query(
		`INSERT INTO objects (bucket_id, shard, key, version_id, is_latest, is_delete_marker,
		 size, etag, content_type, storage_class, mtime, manifest, user_meta, tags,
		 retain_until, retain_mode, legal_hold, checksums, sse, ssec_key_md5, restore_status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		gocqlUUID(o.BucketID), shard, o.Key, versionID, true, o.IsDeleteMarker,
		o.Size, o.ETag, o.ContentType, o.StorageClass, o.Mtime, manifestBlob, o.UserMeta, o.Tags,
		retainUntil, nilIfEmpty(o.RetainMode), o.LegalHold, o.Checksums, nilIfEmpty(o.SSE), nilIfEmpty(o.SSECKeyMD5), nilIfEmpty(o.RestoreStatus),
	).WithContext(ctx).Exec()
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func (s *Store) GetObject(ctx context.Context, bucketID uuid.UUID, key, versionID string) (*meta.Object, error) {
	shard := shardOf(key, s.defaultShard)
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
	)
	var err error
	if versionID == "" {
		err = s.s.Query(
			`SELECT version_id, is_latest, is_delete_marker, size, etag, content_type,
			        storage_class, mtime, manifest, user_meta, tags,
			        retain_until, retain_mode, legal_hold, checksums, sse, ssec_key_md5, restore_status
			 FROM objects WHERE bucket_id=? AND shard=? AND key=? LIMIT 1`,
			gocqlUUID(bucketID), shard, key,
		).WithContext(ctx).Scan(&versionUUID, &isLatest, &isDeleteMark, &size, &etag, &ctype,
			&class, &mtime, &manifestBlob, &userMeta, &tags,
			&retainUntil, &retainMode, &legalHold, &checksums, &sse, &ssecKeyMD5, &restore)
	} else {
		vUUID, perr := gocql.ParseUUID(versionID)
		if perr != nil {
			return nil, meta.ErrObjectNotFound
		}
		err = s.s.Query(
			`SELECT version_id, is_latest, is_delete_marker, size, etag, content_type,
			        storage_class, mtime, manifest, user_meta, tags,
			        retain_until, retain_mode, legal_hold, checksums, sse, ssec_key_md5, restore_status
			 FROM objects WHERE bucket_id=? AND shard=? AND key=? AND version_id=?`,
			gocqlUUID(bucketID), shard, key, vUUID,
		).WithContext(ctx).Scan(&versionUUID, &isLatest, &isDeleteMark, &size, &etag, &ctype,
			&class, &mtime, &manifestBlob, &userMeta, &tags,
			&retainUntil, &retainMode, &legalHold, &checksums, &sse, &ssecKeyMD5, &restore)
	}
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrObjectNotFound
	}
	if err != nil {
		return nil, err
	}
	if versionID == "" && isDeleteMark {
		return nil, meta.ErrObjectNotFound
	}
	m, err := decodeManifest(manifestBlob)
	if err != nil {
		return nil, err
	}
	return &meta.Object{
		BucketID:       bucketID,
		Key:            key,
		VersionID:      versionUUID.String(),
		IsLatest:       isLatest,
		IsDeleteMarker: isDeleteMark,
		Size:           size,
		ETag:           etag,
		ContentType:    ctype,
		StorageClass:   class,
		Mtime:          mtime,
		Manifest:       m,
		UserMeta:       userMeta,
		Tags:           tags,
		RetainUntil:    retainUntil,
		RetainMode:     retainMode,
		LegalHold:      legalHold,
		Checksums:      checksums,
		SSE:            sse,
		SSECKeyMD5:     ssecKeyMD5,
		RestoreStatus:  restore,
	}, nil
}

func (s *Store) DeleteObject(ctx context.Context, bucketID uuid.UUID, key, versionID string, versioned bool) (*meta.Object, error) {
	shard := shardOf(key, s.defaultShard)

	if versionID != "" {
		vUUID, err := gocql.ParseUUID(versionID)
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
	)
	if !c.iter.Scan(&key, &versionID, &isDeleteMark, &size, &etag, &ctype,
		&class, &mtime, &manifestBlob, &userMeta) {
		return false
	}
	m, _ := decodeManifest(manifestBlob)
	c.current = &meta.Object{
		BucketID:       c.bucket,
		Key:            key,
		VersionID:      versionID.String(),
		IsDeleteMarker: isDeleteMark,
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
			        storage_class, mtime, manifest, user_meta
			 FROM objects WHERE bucket_id=? AND shard=?`,
			gocqlUUID(bucketID), shard,
		).WithContext(ctx).PageSize(pageSize).Iter()
	} else {
		iter = s.s.Query(
			`SELECT key, version_id, is_delete_marker, size, etag, content_type,
			        storage_class, mtime, manifest, user_meta
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
			VersionID:    versionID.String(),
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
		batch.Query(
			`INSERT INTO gc_queue (region, enqueued_at, oid, pool, cluster, namespace)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			region, now, c.OID, c.Pool, c.Cluster, c.Namespace,
		)
	}
	return s.s.ExecuteBatch(batch)
}

func (s *Store) ListGCEntries(ctx context.Context, region string, before time.Time, limit int) ([]meta.GCEntry, error) {
	iter := s.s.Query(
		`SELECT enqueued_at, oid, pool, cluster, namespace
		 FROM gc_queue WHERE region=? AND enqueued_at <= ? LIMIT ?`,
		region, before, limit,
	).WithContext(ctx).Iter()
	defer iter.Close()

	var (
		out        []meta.GCEntry
		enqueuedAt time.Time
		oid, pool  string
		cluster    string
		namespace  string
	)
	for iter.Scan(&enqueuedAt, &oid, &pool, &cluster, &namespace) {
		out = append(out, meta.GCEntry{
			EnqueuedAt: enqueuedAt,
			Chunk: data.ChunkRef{
				Cluster:   cluster,
				Pool:      pool,
				Namespace: namespace,
				OID:       oid,
			},
		})
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) AckGCEntry(ctx context.Context, region string, e meta.GCEntry) error {
	return s.s.Query(
		`DELETE FROM gc_queue WHERE region=? AND enqueued_at=? AND oid=?`,
		region, e.EnqueuedAt, e.Chunk.OID,
	).WithContext(ctx).Exec()
}

func (s *Store) resolveVersionID(ctx context.Context, bucketID uuid.UUID, key, versionID string) (gocql.UUID, int, error) {
	shard := shardOf(key, s.defaultShard)
	if versionID != "" {
		v, err := gocql.ParseUUID(versionID)
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
		`INSERT INTO multipart_uploads (bucket_id, upload_id, key, status, storage_class, content_type, initiated_at, sse)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		gocqlUUID(mu.BucketID), uploadUUID, mu.Key, "uploading", mu.StorageClass, mu.ContentType, mu.InitiatedAt, nilIfEmpty(mu.SSE),
	).WithContext(ctx).Exec()
}

func (s *Store) GetMultipartUpload(ctx context.Context, bucketID uuid.UUID, uploadID string) (*meta.MultipartUpload, error) {
	uploadUUID, err := gocql.ParseUUID(uploadID)
	if err != nil {
		return nil, meta.ErrMultipartNotFound
	}
	var (
		key, status, class, ctype string
		sse                       string
		initiated                 time.Time
	)
	err = s.s.Query(
		`SELECT key, status, storage_class, content_type, initiated_at, sse
		 FROM multipart_uploads WHERE bucket_id=? AND upload_id=?`,
		gocqlUUID(bucketID), uploadUUID,
	).WithContext(ctx).Scan(&key, &status, &class, &ctype, &initiated, &sse)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrMultipartNotFound
	}
	if err != nil {
		return nil, err
	}
	return &meta.MultipartUpload{
		BucketID:     bucketID,
		UploadID:     uploadID,
		Key:          key,
		Status:       status,
		StorageClass: class,
		ContentType:  ctype,
		InitiatedAt:  initiated,
		SSE:          sse,
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

	storedParts, err := s.ListParts(ctx, obj.BucketID, uploadID)
	if err != nil {
		return nil, err
	}
	byNumber := make(map[int]*meta.MultipartPart, len(storedParts))
	for _, p := range storedParts {
		byNumber[p.PartNumber] = p
	}

	used := make(map[int]bool, len(parts))
	var chunks []data.ChunkRef
	var totalSize int64
	for _, cp := range parts {
		p, ok := byNumber[cp.PartNumber]
		if !ok {
			return nil, meta.ErrMultipartPartMissing
		}
		if strings.Trim(cp.ETag, `"`) != p.ETag {
			return nil, meta.ErrMultipartETagMismatch
		}
		if p.Manifest != nil {
			chunks = append(chunks, p.Manifest.Chunks...)
		}
		totalSize += p.Size
		used[cp.PartNumber] = true
	}

	obj.Manifest = &data.Manifest{
		Class:     obj.StorageClass,
		Size:      totalSize,
		ChunkSize: data.DefaultChunkSize,
		ETag:      obj.ETag,
		Chunks:    chunks,
	}
	obj.Size = totalSize
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
