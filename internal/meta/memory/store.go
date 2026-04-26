package memory

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gocql/gocql"
	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

type Store struct {
	mu           sync.RWMutex
	buckets      map[string]*meta.Bucket
	objects      map[uuid.UUID]map[string][]*meta.Object
	multiparts   map[uuid.UUID]map[string]*mpState
	lifecycles   map[uuid.UUID][]byte
	cors         map[uuid.UUID][]byte
	policies     map[uuid.UUID][]byte
	pab          map[uuid.UUID][]byte
	ownership    map[uuid.UUID][]byte
	encryption   map[uuid.UUID][]byte
	objectLock   map[uuid.UUID][]byte
	notification map[uuid.UUID][]byte
	website      map[uuid.UUID][]byte
	replication  map[uuid.UUID][]byte
	logging      map[uuid.UUID][]byte
	tagging      map[uuid.UUID][]byte
	bucketGrants map[uuid.UUID][]meta.Grant
	objectGrants map[grantKey][]meta.Grant
	iamUsers     map[string]*meta.IAMUser
	accessKeys   map[string]*meta.IAMAccessKey
	gc           map[string][]meta.GCEntry
	locker       *Locker
}

type grantKey struct {
	BucketID  uuid.UUID
	Key       string
	VersionID string
}

type mpState struct {
	upload *meta.MultipartUpload
	parts  map[int]*meta.MultipartPart
}

func New() *Store {
	return &Store{
		buckets:      make(map[string]*meta.Bucket),
		objects:      make(map[uuid.UUID]map[string][]*meta.Object),
		multiparts:   make(map[uuid.UUID]map[string]*mpState),
		lifecycles:   make(map[uuid.UUID][]byte),
		cors:         make(map[uuid.UUID][]byte),
		policies:     make(map[uuid.UUID][]byte),
		pab:          make(map[uuid.UUID][]byte),
		ownership:    make(map[uuid.UUID][]byte),
		encryption:   make(map[uuid.UUID][]byte),
		objectLock:   make(map[uuid.UUID][]byte),
		notification: make(map[uuid.UUID][]byte),
		website:      make(map[uuid.UUID][]byte),
		replication:  make(map[uuid.UUID][]byte),
		logging:      make(map[uuid.UUID][]byte),
		tagging:      make(map[uuid.UUID][]byte),
		bucketGrants: make(map[uuid.UUID][]meta.Grant),
		objectGrants: make(map[grantKey][]meta.Grant),
		iamUsers:     make(map[string]*meta.IAMUser),
		accessKeys:   make(map[string]*meta.IAMAccessKey),
		gc:           make(map[string][]meta.GCEntry),
		locker:       NewLocker(),
	}
}

// Locker returns the in-process leader-election locker for this store.
func (s *Store) Locker() *Locker { return s.locker }

func (s *Store) EnqueueChunkDeletion(ctx context.Context, region string, chunks []data.ChunkRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	for _, c := range chunks {
		s.gc[region] = append(s.gc[region], meta.GCEntry{Chunk: c, EnqueuedAt: now})
	}
	return nil
}

func (s *Store) ListGCEntries(ctx context.Context, region string, before time.Time, limit int) ([]meta.GCEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries := s.gc[region]
	out := make([]meta.GCEntry, 0, len(entries))
	for _, e := range entries {
		if !e.EnqueuedAt.After(before) {
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (s *Store) AckGCEntry(ctx context.Context, region string, e meta.GCEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.gc[region]
	for i, x := range entries {
		if x.Chunk.OID == e.Chunk.OID && x.EnqueuedAt.Equal(e.EnqueuedAt) {
			s.gc[region] = append(entries[:i], entries[i+1:]...)
			return nil
		}
	}
	return nil
}

func (s *Store) CreateBucket(ctx context.Context, name, owner, defaultClass string) (*meta.Bucket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.buckets[name]; ok {
		return nil, meta.ErrBucketAlreadyExists
	}
	b := &meta.Bucket{
		Name:         name,
		ID:           uuid.New(),
		Owner:        owner,
		CreatedAt:    time.Now().UTC(),
		DefaultClass: defaultClass,
		Versioning:   meta.VersioningDisabled,
	}
	s.buckets[name] = b
	s.objects[b.ID] = make(map[string][]*meta.Object)
	s.multiparts[b.ID] = make(map[string]*mpState)
	return b, nil
}

func (s *Store) GetBucket(ctx context.Context, name string) (*meta.Bucket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.buckets[name]
	if !ok {
		return nil, meta.ErrBucketNotFound
	}
	cp := *b
	return &cp, nil
}

func (s *Store) DeleteBucket(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[name]
	if !ok {
		return meta.ErrBucketNotFound
	}
	if len(s.objects[b.ID]) > 0 {
		return meta.ErrBucketNotEmpty
	}
	if len(s.multiparts[b.ID]) > 0 {
		return meta.ErrBucketNotEmpty
	}
	delete(s.buckets, name)
	delete(s.objects, b.ID)
	delete(s.multiparts, b.ID)
	return nil
}

func (s *Store) ListBuckets(ctx context.Context, owner string) ([]*meta.Bucket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*meta.Bucket, 0, len(s.buckets))
	for _, b := range s.buckets {
		if owner != "" && b.Owner != owner {
			continue
		}
		cp := *b
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) SetBucketVersioning(ctx context.Context, name, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[name]
	if !ok {
		return meta.ErrBucketNotFound
	}
	b.Versioning = state
	return nil
}

func (s *Store) SetBucketACL(ctx context.Context, name, canned string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[name]
	if !ok {
		return meta.ErrBucketNotFound
	}
	b.ACL = canned
	return nil
}

func (s *Store) SetBucketGrants(ctx context.Context, bucketID uuid.UUID, grants []meta.Grant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[bucketID]; !ok {
		return meta.ErrBucketNotFound
	}
	cp := append([]meta.Grant(nil), grants...)
	s.bucketGrants[bucketID] = cp
	return nil
}

func (s *Store) GetBucketGrants(ctx context.Context, bucketID uuid.UUID) ([]meta.Grant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.bucketGrants[bucketID]
	if !ok {
		return nil, meta.ErrNoSuchGrants
	}
	return append([]meta.Grant(nil), g...), nil
}

func (s *Store) DeleteBucketGrants(ctx context.Context, bucketID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bucketGrants, bucketID)
	return nil
}

func (s *Store) SetObjectGrants(ctx context.Context, bucketID uuid.UUID, key, versionID string, grants []meta.Grant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.findLatest(bucketID, key, versionID)
	if err != nil {
		return err
	}
	cp := append([]meta.Grant(nil), grants...)
	s.objectGrants[grantKey{BucketID: bucketID, Key: key, VersionID: o.VersionID}] = cp
	return nil
}

func (s *Store) GetObjectGrants(ctx context.Context, bucketID uuid.UUID, key, versionID string) ([]meta.Grant, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, err := s.findLatest(bucketID, key, versionID)
	if err != nil {
		return nil, err
	}
	g, ok := s.objectGrants[grantKey{BucketID: bucketID, Key: key, VersionID: o.VersionID}]
	if !ok {
		return nil, meta.ErrNoSuchGrants
	}
	return append([]meta.Grant(nil), g...), nil
}

func (s *Store) PutObject(ctx context.Context, o *meta.Object, versioned bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.objects[o.BucketID]
	if !ok {
		return meta.ErrBucketNotFound
	}
	if o.VersionID == "" {
		o.VersionID = gocql.TimeUUID().String()
	}
	cp := *o
	if versioned {
		bucket[o.Key] = append([]*meta.Object{&cp}, bucket[o.Key]...)
	} else {
		bucket[o.Key] = []*meta.Object{&cp}
	}
	return nil
}

func (s *Store) GetObject(ctx context.Context, bucketID uuid.UUID, key, versionID string) (*meta.Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	bucket, ok := s.objects[bucketID]
	if !ok {
		return nil, meta.ErrBucketNotFound
	}
	versions, ok := bucket[key]
	if !ok || len(versions) == 0 {
		return nil, meta.ErrObjectNotFound
	}
	if versionID == "" {
		latest := versions[0]
		if latest.IsDeleteMarker {
			return nil, meta.ErrObjectNotFound
		}
		cp := *latest
		return &cp, nil
	}
	for _, v := range versions {
		if v.VersionID == versionID {
			cp := *v
			return &cp, nil
		}
	}
	return nil, meta.ErrObjectNotFound
}

func (s *Store) DeleteObject(ctx context.Context, bucketID uuid.UUID, key, versionID string, versioned bool) (*meta.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.objects[bucketID]
	if !ok {
		return nil, meta.ErrBucketNotFound
	}
	versions, ok := bucket[key]
	if !ok || len(versions) == 0 {
		if versioned && versionID == "" {
			marker := &meta.Object{
				BucketID:       bucketID,
				Key:            key,
				VersionID:      gocql.TimeUUID().String(),
				IsLatest:       true,
				IsDeleteMarker: true,
				Mtime:          time.Now().UTC(),
			}
			bucket[key] = []*meta.Object{marker}
			return marker, nil
		}
		return nil, meta.ErrObjectNotFound
	}

	if versionID != "" {
		for i, v := range versions {
			if v.VersionID == versionID {
				cp := *v
				bucket[key] = append(versions[:i], versions[i+1:]...)
				if len(bucket[key]) == 0 {
					delete(bucket, key)
				}
				return &cp, nil
			}
		}
		return nil, meta.ErrObjectNotFound
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
		bucket[key] = append([]*meta.Object{marker}, versions...)
		return marker, nil
	}

	latest := versions[0]
	delete(bucket, key)
	cp := *latest
	return &cp, nil
}

func (s *Store) ListObjects(ctx context.Context, bucketID uuid.UUID, opts meta.ListOptions) (*meta.ListResult, error) {
	s.mu.RLock()
	bucket, ok := s.objects[bucketID]
	if !ok {
		s.mu.RUnlock()
		return nil, meta.ErrBucketNotFound
	}
	keys := make([]string, 0, len(bucket))
	for k := range bucket {
		keys = append(keys, k)
	}
	s.mu.RUnlock()
	sort.Strings(keys)

	limit := opts.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}

	res := &meta.ListResult{}
	seenPrefixes := make(map[string]struct{})

	for _, k := range keys {
		if opts.Marker != "" && k <= opts.Marker {
			continue
		}
		if opts.Prefix != "" && !strings.HasPrefix(k, opts.Prefix) {
			continue
		}
		s.mu.RLock()
		versions := bucket[k]
		s.mu.RUnlock()
		if len(versions) == 0 || versions[0].IsDeleteMarker {
			continue
		}
		if opts.Delimiter != "" {
			rest := k[len(opts.Prefix):]
			if idx := strings.Index(rest, opts.Delimiter); idx >= 0 {
				pfx := opts.Prefix + rest[:idx+len(opts.Delimiter)]
				if _, ok := seenPrefixes[pfx]; !ok {
					if len(res.Objects)+len(res.CommonPrefixes) >= limit {
						res.Truncated = true
						res.NextMarker = pfx
						return res, nil
					}
					seenPrefixes[pfx] = struct{}{}
					res.CommonPrefixes = append(res.CommonPrefixes, pfx)
				}
				continue
			}
		}
		if len(res.Objects)+len(res.CommonPrefixes) >= limit {
			res.Truncated = true
			res.NextMarker = k
			return res, nil
		}
		cp := *versions[0]
		res.Objects = append(res.Objects, &cp)
	}
	return res, nil
}

func (s *Store) ListObjectVersions(ctx context.Context, bucketID uuid.UUID, opts meta.ListOptions) (*meta.ListVersionsResult, error) {
	s.mu.RLock()
	bucket, ok := s.objects[bucketID]
	if !ok {
		s.mu.RUnlock()
		return nil, meta.ErrBucketNotFound
	}
	keys := make([]string, 0, len(bucket))
	for k := range bucket {
		keys = append(keys, k)
	}
	s.mu.RUnlock()
	sort.Strings(keys)

	limit := opts.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}

	res := &meta.ListVersionsResult{}
	seenPrefixes := make(map[string]struct{})

	for _, k := range keys {
		if opts.Marker != "" && k < opts.Marker {
			continue
		}
		if opts.Prefix != "" && !strings.HasPrefix(k, opts.Prefix) {
			continue
		}
		if opts.Delimiter != "" {
			rest := k[len(opts.Prefix):]
			if idx := strings.Index(rest, opts.Delimiter); idx >= 0 {
				pfx := opts.Prefix + rest[:idx+len(opts.Delimiter)]
				if _, ok := seenPrefixes[pfx]; !ok {
					seenPrefixes[pfx] = struct{}{}
					res.CommonPrefixes = append(res.CommonPrefixes, pfx)
				}
				continue
			}
		}
		s.mu.RLock()
		versions := bucket[k]
		s.mu.RUnlock()
		for i, v := range versions {
			cp := *v
			cp.IsLatest = (i == 0)
			res.Versions = append(res.Versions, &cp)
			if len(res.Versions) >= limit {
				res.Truncated = true
				res.NextKeyMarker = k
				res.NextVersionID = v.VersionID
				return res, nil
			}
		}
	}
	return res, nil
}

func (s *Store) SetObjectStorage(ctx context.Context, bucketID uuid.UUID, key, versionID, expectedClass, newClass string, manifest *data.Manifest) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.findLatest(bucketID, key, versionID)
	if err != nil {
		return false, err
	}
	if expectedClass != "" && o.StorageClass != expectedClass {
		return false, nil
	}
	o.StorageClass = newClass
	o.Manifest = manifest
	return true, nil
}

func (s *Store) findLatest(bucketID uuid.UUID, key, versionID string) (*meta.Object, error) {
	bucket, ok := s.objects[bucketID]
	if !ok {
		return nil, meta.ErrBucketNotFound
	}
	versions, ok := bucket[key]
	if !ok || len(versions) == 0 {
		return nil, meta.ErrObjectNotFound
	}
	if versionID == "" {
		return versions[0], nil
	}
	for _, v := range versions {
		if v.VersionID == versionID {
			return v, nil
		}
	}
	return nil, meta.ErrObjectNotFound
}

func (s *Store) SetObjectTags(ctx context.Context, bucketID uuid.UUID, key, versionID string, tags map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.findLatest(bucketID, key, versionID)
	if err != nil {
		return err
	}
	o.Tags = tags
	return nil
}

func (s *Store) GetObjectTags(ctx context.Context, bucketID uuid.UUID, key, versionID string) (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, err := s.findLatest(bucketID, key, versionID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(o.Tags))
	for k, v := range o.Tags {
		out[k] = v
	}
	return out, nil
}

func (s *Store) DeleteObjectTags(ctx context.Context, bucketID uuid.UUID, key, versionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.findLatest(bucketID, key, versionID)
	if err != nil {
		return err
	}
	o.Tags = nil
	return nil
}

func (s *Store) SetObjectRetention(ctx context.Context, bucketID uuid.UUID, key, versionID, mode string, until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.findLatest(bucketID, key, versionID)
	if err != nil {
		return err
	}
	o.RetainMode = mode
	o.RetainUntil = until
	return nil
}

func (s *Store) SetObjectLegalHold(ctx context.Context, bucketID uuid.UUID, key, versionID string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.findLatest(bucketID, key, versionID)
	if err != nil {
		return err
	}
	o.LegalHold = on
	return nil
}

func (s *Store) SetObjectRestoreStatus(ctx context.Context, bucketID uuid.UUID, key, versionID, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, err := s.findLatest(bucketID, key, versionID)
	if err != nil {
		return err
	}
	o.RestoreStatus = status
	return nil
}

func (s *Store) SetBucketLifecycle(ctx context.Context, bucketID uuid.UUID, xmlBlob []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[bucketID]; !ok {
		return meta.ErrBucketNotFound
	}
	cp := append([]byte(nil), xmlBlob...)
	s.lifecycles[bucketID] = cp
	return nil
}

func (s *Store) GetBucketLifecycle(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	blob, ok := s.lifecycles[bucketID]
	if !ok {
		return nil, meta.ErrNoSuchLifecycle
	}
	return append([]byte(nil), blob...), nil
}

func (s *Store) DeleteBucketLifecycle(ctx context.Context, bucketID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.lifecycles, bucketID)
	return nil
}

func (s *Store) setBucketBlob(m map[uuid.UUID][]byte, bucketID uuid.UUID, blob []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[bucketID]; !ok {
		return meta.ErrBucketNotFound
	}
	m[bucketID] = append([]byte(nil), blob...)
	return nil
}

func (s *Store) getBucketBlob(m map[uuid.UUID][]byte, bucketID uuid.UUID, missing error) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	blob, ok := m[bucketID]
	if !ok {
		return nil, missing
	}
	return append([]byte(nil), blob...), nil
}

func (s *Store) deleteBucketBlob(m map[uuid.UUID][]byte, bucketID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(m, bucketID)
	return nil
}

func (s *Store) SetBucketCORS(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(s.cors, bucketID, blob)
}
func (s *Store) GetBucketCORS(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(s.cors, bucketID, meta.ErrNoSuchCORS)
}
func (s *Store) DeleteBucketCORS(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(s.cors, bucketID)
}

func (s *Store) SetBucketPolicy(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(s.policies, bucketID, blob)
}
func (s *Store) GetBucketPolicy(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(s.policies, bucketID, meta.ErrNoSuchBucketPolicy)
}
func (s *Store) DeleteBucketPolicy(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(s.policies, bucketID)
}

func (s *Store) SetBucketPublicAccessBlock(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(s.pab, bucketID, blob)
}
func (s *Store) GetBucketPublicAccessBlock(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(s.pab, bucketID, meta.ErrNoSuchPublicAccessBlock)
}
func (s *Store) DeleteBucketPublicAccessBlock(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(s.pab, bucketID)
}

func (s *Store) SetBucketOwnershipControls(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(s.ownership, bucketID, blob)
}
func (s *Store) GetBucketOwnershipControls(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(s.ownership, bucketID, meta.ErrNoSuchOwnershipControls)
}
func (s *Store) DeleteBucketOwnershipControls(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(s.ownership, bucketID)
}

func (s *Store) SetBucketEncryption(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(s.encryption, bucketID, blob)
}
func (s *Store) GetBucketEncryption(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(s.encryption, bucketID, meta.ErrNoSuchEncryption)
}
func (s *Store) DeleteBucketEncryption(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(s.encryption, bucketID)
}

func (s *Store) SetBucketObjectLockEnabled(ctx context.Context, name string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[name]
	if !ok {
		return meta.ErrBucketNotFound
	}
	b.ObjectLockEnabled = enabled
	return nil
}

func (s *Store) SetBucketRegion(ctx context.Context, name, region string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[name]
	if !ok {
		return meta.ErrBucketNotFound
	}
	b.Region = region
	return nil
}

func (s *Store) SetBucketObjectLockConfig(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(s.objectLock, bucketID, blob)
}
func (s *Store) GetBucketObjectLockConfig(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(s.objectLock, bucketID, meta.ErrNoSuchObjectLockConfig)
}
func (s *Store) DeleteBucketObjectLockConfig(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(s.objectLock, bucketID)
}

func (s *Store) SetBucketNotificationConfig(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(s.notification, bucketID, blob)
}
func (s *Store) GetBucketNotificationConfig(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(s.notification, bucketID, meta.ErrNoSuchNotification)
}
func (s *Store) DeleteBucketNotificationConfig(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(s.notification, bucketID)
}

func (s *Store) SetBucketWebsite(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(s.website, bucketID, blob)
}
func (s *Store) GetBucketWebsite(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(s.website, bucketID, meta.ErrNoSuchWebsite)
}
func (s *Store) DeleteBucketWebsite(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(s.website, bucketID)
}

func (s *Store) SetBucketReplication(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(s.replication, bucketID, blob)
}
func (s *Store) GetBucketReplication(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(s.replication, bucketID, meta.ErrNoSuchReplication)
}
func (s *Store) DeleteBucketReplication(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(s.replication, bucketID)
}

func (s *Store) SetBucketLogging(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(s.logging, bucketID, blob)
}
func (s *Store) GetBucketLogging(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(s.logging, bucketID, meta.ErrNoSuchLogging)
}
func (s *Store) DeleteBucketLogging(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(s.logging, bucketID)
}

func (s *Store) SetBucketTagging(ctx context.Context, bucketID uuid.UUID, blob []byte) error {
	return s.setBucketBlob(s.tagging, bucketID, blob)
}
func (s *Store) GetBucketTagging(ctx context.Context, bucketID uuid.UUID) ([]byte, error) {
	return s.getBucketBlob(s.tagging, bucketID, meta.ErrNoSuchTagSet)
}
func (s *Store) DeleteBucketTagging(ctx context.Context, bucketID uuid.UUID) error {
	return s.deleteBucketBlob(s.tagging, bucketID)
}

func (s *Store) CreateMultipartUpload(ctx context.Context, mu *meta.MultipartUpload) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ups, ok := s.multiparts[mu.BucketID]
	if !ok {
		return meta.ErrBucketNotFound
	}
	cp := *mu
	ups[mu.UploadID] = &mpState{upload: &cp, parts: make(map[int]*meta.MultipartPart)}
	return nil
}

func (s *Store) GetMultipartUpload(ctx context.Context, bucketID uuid.UUID, uploadID string) (*meta.MultipartUpload, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ups, ok := s.multiparts[bucketID]
	if !ok {
		return nil, meta.ErrBucketNotFound
	}
	st, ok := ups[uploadID]
	if !ok {
		return nil, meta.ErrMultipartNotFound
	}
	cp := *st.upload
	return &cp, nil
}

func (s *Store) ListMultipartUploads(ctx context.Context, bucketID uuid.UUID, prefix string, limit int) ([]*meta.MultipartUpload, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ups, ok := s.multiparts[bucketID]
	if !ok {
		return nil, meta.ErrBucketNotFound
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	out := make([]*meta.MultipartUpload, 0, len(ups))
	for _, st := range ups {
		if prefix != "" && !strings.HasPrefix(st.upload.Key, prefix) {
			continue
		}
		cp := *st.upload
		out = append(out, &cp)
		if len(out) >= limit {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].UploadID < out[j].UploadID
	})
	return out, nil
}

func (s *Store) SavePart(ctx context.Context, bucketID uuid.UUID, uploadID string, part *meta.MultipartPart) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ups, ok := s.multiparts[bucketID]
	if !ok {
		return meta.ErrBucketNotFound
	}
	st, ok := ups[uploadID]
	if !ok {
		return meta.ErrMultipartNotFound
	}
	if st.upload.Status != "uploading" {
		return meta.ErrMultipartInProgress
	}
	cp := *part
	cp.Mtime = time.Now().UTC()
	st.parts[part.PartNumber] = &cp
	return nil
}

func (s *Store) ListParts(ctx context.Context, bucketID uuid.UUID, uploadID string) ([]*meta.MultipartPart, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ups, ok := s.multiparts[bucketID]
	if !ok {
		return nil, meta.ErrBucketNotFound
	}
	st, ok := ups[uploadID]
	if !ok {
		return nil, meta.ErrMultipartNotFound
	}
	out := make([]*meta.MultipartPart, 0, len(st.parts))
	for _, p := range st.parts {
		cp := *p
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PartNumber < out[j].PartNumber })
	return out, nil
}

func (s *Store) CompleteMultipartUpload(ctx context.Context, obj *meta.Object, uploadID string, parts []meta.CompletePart, versioned bool) ([]*data.Manifest, error) {
	s.mu.Lock()
	ups, ok := s.multiparts[obj.BucketID]
	if !ok {
		s.mu.Unlock()
		return nil, meta.ErrBucketNotFound
	}
	st, ok := ups[uploadID]
	if !ok {
		s.mu.Unlock()
		return nil, meta.ErrMultipartNotFound
	}
	if st.upload.Status != "uploading" {
		s.mu.Unlock()
		return nil, meta.ErrMultipartInProgress
	}
	st.upload.Status = "completing"

	used := make(map[int]bool, len(parts))
	var chunks []data.ChunkRef
	var totalSize int64
	for _, cp := range parts {
		p, ok := st.parts[cp.PartNumber]
		if !ok {
			s.mu.Unlock()
			return nil, meta.ErrMultipartPartMissing
		}
		if strings.Trim(cp.ETag, `"`) != p.ETag {
			s.mu.Unlock()
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
	if obj.VersionID == "" {
		obj.VersionID = gocql.TimeUUID().String()
	}
	obj.IsLatest = true

	bucket, ok := s.objects[obj.BucketID]
	if !ok {
		s.mu.Unlock()
		return nil, meta.ErrBucketNotFound
	}
	cp := *obj
	if versioned {
		bucket[obj.Key] = append([]*meta.Object{&cp}, bucket[obj.Key]...)
	} else {
		bucket[obj.Key] = []*meta.Object{&cp}
	}

	var orphans []*data.Manifest
	for num, p := range st.parts {
		if !used[num] && p.Manifest != nil {
			orphans = append(orphans, p.Manifest)
		}
	}
	delete(ups, uploadID)
	s.mu.Unlock()
	return orphans, nil
}

func (s *Store) AbortMultipartUpload(ctx context.Context, bucketID uuid.UUID, uploadID string) ([]*data.Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ups, ok := s.multiparts[bucketID]
	if !ok {
		return nil, meta.ErrBucketNotFound
	}
	st, ok := ups[uploadID]
	if !ok {
		return nil, meta.ErrMultipartNotFound
	}
	manifests := make([]*data.Manifest, 0, len(st.parts))
	for _, p := range st.parts {
		if p.Manifest != nil {
			manifests = append(manifests, p.Manifest)
		}
	}
	delete(ups, uploadID)
	return manifests, nil
}

func (s *Store) CreateIAMUser(ctx context.Context, u *meta.IAMUser) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.iamUsers[u.UserName]; ok {
		return meta.ErrIAMUserAlreadyExists
	}
	cp := *u
	s.iamUsers[u.UserName] = &cp
	return nil
}

func (s *Store) GetIAMUser(ctx context.Context, userName string) (*meta.IAMUser, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.iamUsers[userName]
	if !ok {
		return nil, meta.ErrIAMUserNotFound
	}
	cp := *u
	return &cp, nil
}

func (s *Store) ListIAMUsers(ctx context.Context, pathPrefix string) ([]*meta.IAMUser, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*meta.IAMUser, 0, len(s.iamUsers))
	for _, u := range s.iamUsers {
		if pathPrefix != "" && !strings.HasPrefix(u.Path, pathPrefix) {
			continue
		}
		cp := *u
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UserName < out[j].UserName })
	return out, nil
}

func (s *Store) DeleteIAMUser(ctx context.Context, userName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.iamUsers[userName]; !ok {
		return meta.ErrIAMUserNotFound
	}
	delete(s.iamUsers, userName)
	return nil
}

func (s *Store) CreateIAMAccessKey(ctx context.Context, ak *meta.IAMAccessKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *ak
	s.accessKeys[ak.AccessKeyID] = &cp
	return nil
}

func (s *Store) GetIAMAccessKey(ctx context.Context, accessKeyID string) (*meta.IAMAccessKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ak, ok := s.accessKeys[accessKeyID]
	if !ok {
		return nil, meta.ErrIAMAccessKeyNotFound
	}
	cp := *ak
	return &cp, nil
}

func (s *Store) ListIAMAccessKeys(ctx context.Context, userName string) ([]*meta.IAMAccessKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*meta.IAMAccessKey, 0)
	for _, ak := range s.accessKeys {
		if userName != "" && ak.UserName != userName {
			continue
		}
		cp := *ak
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccessKeyID < out[j].AccessKeyID })
	return out, nil
}

func (s *Store) DeleteIAMAccessKey(ctx context.Context, accessKeyID string) (*meta.IAMAccessKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ak, ok := s.accessKeys[accessKeyID]
	if !ok {
		return nil, meta.ErrIAMAccessKeyNotFound
	}
	cp := *ak
	delete(s.accessKeys, accessKeyID)
	return &cp, nil
}

func (s *Store) Close() error { return nil }
