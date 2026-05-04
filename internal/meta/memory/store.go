package memory

import (
	"context"
	"maps"
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
	mu             sync.RWMutex
	buckets        map[string]*meta.Bucket
	objects        map[uuid.UUID]map[string][]*meta.Object
	multiparts     map[uuid.UUID]map[string]*mpState
	lifecycles     map[uuid.UUID][]byte
	cors           map[uuid.UUID][]byte
	policies       map[uuid.UUID][]byte
	pab            map[uuid.UUID][]byte
	ownership      map[uuid.UUID][]byte
	encryption     map[uuid.UUID][]byte
	objectLock     map[uuid.UUID][]byte
	notification   map[uuid.UUID][]byte
	website        map[uuid.UUID][]byte
	replication    map[uuid.UUID][]byte
	logging        map[uuid.UUID][]byte
	tagging        map[uuid.UUID][]byte
	inventoryConfigs map[uuid.UUID]map[string][]byte
	accessPoints     map[string]*meta.AccessPoint
	bucketGrants   map[uuid.UUID][]meta.Grant
	objectGrants   map[grantKey][]meta.Grant
	iamUsers       map[string]*meta.IAMUser
	accessKeys     map[string]*meta.IAMAccessKey
	managedPolicies map[string]*meta.ManagedPolicy
	// userPolicies maps userName -> policyArn -> attached_at.
	userPolicies   map[string]map[string]time.Time
	// policyUsers is the inverse index (policyArn -> set of userNames) used
	// by DeleteManagedPolicy to detect attachments without scanning.
	policyUsers    map[string]map[string]struct{}
	gc             map[string][]meta.GCEntry
	notifyQueue    map[uuid.UUID][]meta.NotificationEvent
	notifyDLQ      map[uuid.UUID][]meta.NotificationDLQEntry
	replicationQueue map[uuid.UUID][]meta.ReplicationEvent
	accessLogQueue map[uuid.UUID][]meta.AccessLogEntry
	auditLog       map[uuid.UUID][]auditEntry
	mpDone         map[completionKey]*completionEntry
	rewrapProgress map[uuid.UUID]*meta.RewrapProgress
	reshardJobs    map[uuid.UUID]*meta.ReshardJob
	// objectManifestRaw mirrors per-object manifest blobs in the on-the-wire
	// format (JSON or proto, per data.SetManifestFormat). Populated by
	// PutObject and mutated by UpdateObjectManifestRaw — used by the manifest
	// rewriter (US-049) to detect and convert pre-existing JSON rows.
	objectManifestRaw map[manifestKey][]byte
	adminJobs      map[string]*meta.AdminJob
	locker         *Locker
}

type manifestKey struct {
	BucketID  uuid.UUID
	Key       string
	VersionID string
}

type completionKey struct {
	BucketID uuid.UUID
	UploadID string
}

type completionEntry struct {
	rec       *meta.MultipartCompletion
	expiresAt time.Time
}

// nowFn is the clock used by completion-record TTL bookkeeping; tests override it.
var nowFn = func() time.Time { return time.Now() }

// SetClockForTest overrides the package-level clock used by completion TTLs.
// Tests must call ResetClockForTest after use.
func SetClockForTest(now func() time.Time) { nowFn = now }

// ResetClockForTest restores the default time.Now clock.
func ResetClockForTest() { nowFn = func() time.Time { return time.Now() } }

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
		inventoryConfigs: make(map[uuid.UUID]map[string][]byte),
		accessPoints:     make(map[string]*meta.AccessPoint),
		bucketGrants: make(map[uuid.UUID][]meta.Grant),
		objectGrants: make(map[grantKey][]meta.Grant),
		iamUsers:     make(map[string]*meta.IAMUser),
		accessKeys:   make(map[string]*meta.IAMAccessKey),
		managedPolicies: make(map[string]*meta.ManagedPolicy),
		userPolicies:    make(map[string]map[string]time.Time),
		policyUsers:     make(map[string]map[string]struct{}),
		gc:             make(map[string][]meta.GCEntry),
		notifyQueue:    make(map[uuid.UUID][]meta.NotificationEvent),
		notifyDLQ:      make(map[uuid.UUID][]meta.NotificationDLQEntry),
		replicationQueue: make(map[uuid.UUID][]meta.ReplicationEvent),
		accessLogQueue: make(map[uuid.UUID][]meta.AccessLogEntry),
		auditLog:       make(map[uuid.UUID][]auditEntry),
		mpDone:         make(map[completionKey]*completionEntry),
		rewrapProgress: make(map[uuid.UUID]*meta.RewrapProgress),
		reshardJobs:    make(map[uuid.UUID]*meta.ReshardJob),
		objectManifestRaw: make(map[manifestKey][]byte),
		adminJobs:      make(map[string]*meta.AdminJob),
		locker:         NewLocker(),
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

func (s *Store) EnqueueNotification(ctx context.Context, evt *meta.NotificationEvent) error {
	if evt == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *evt
	if len(evt.Payload) > 0 {
		cp.Payload = append([]byte(nil), evt.Payload...)
	}
	if cp.EventID == "" {
		cp.EventID = gocql.TimeUUID().String()
	}
	if cp.EventTime.IsZero() {
		cp.EventTime = time.Now().UTC()
	}
	s.notifyQueue[evt.BucketID] = append(s.notifyQueue[evt.BucketID], cp)
	return nil
}

func (s *Store) ListPendingNotifications(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.NotificationEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows := s.notifyQueue[bucketID]
	out := make([]meta.NotificationEvent, 0, len(rows))
	for _, e := range rows {
		cp := e
		if len(e.Payload) > 0 {
			cp.Payload = append([]byte(nil), e.Payload...)
		}
		out = append(out, cp)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *Store) AckNotification(ctx context.Context, evt meta.NotificationEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := s.notifyQueue[evt.BucketID]
	for i, e := range rows {
		if e.EventID == evt.EventID {
			s.notifyQueue[evt.BucketID] = append(rows[:i], rows[i+1:]...)
			return nil
		}
	}
	return nil
}

func (s *Store) EnqueueNotificationDLQ(ctx context.Context, entry *meta.NotificationDLQEntry) error {
	if entry == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *entry
	if len(entry.Payload) > 0 {
		cp.Payload = append([]byte(nil), entry.Payload...)
	}
	if cp.EnqueuedAt.IsZero() {
		cp.EnqueuedAt = time.Now().UTC()
	}
	s.notifyDLQ[entry.BucketID] = append(s.notifyDLQ[entry.BucketID], cp)
	return nil
}

func (s *Store) ListNotificationDLQ(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.NotificationDLQEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows := s.notifyDLQ[bucketID]
	out := make([]meta.NotificationDLQEntry, 0, len(rows))
	for _, e := range rows {
		cp := e
		if len(e.Payload) > 0 {
			cp.Payload = append([]byte(nil), e.Payload...)
		}
		out = append(out, cp)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *Store) EnqueueReplication(ctx context.Context, evt *meta.ReplicationEvent) error {
	if evt == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *evt
	if cp.EventID == "" {
		cp.EventID = gocql.TimeUUID().String()
	}
	if cp.EventTime.IsZero() {
		cp.EventTime = time.Now().UTC()
	}
	s.replicationQueue[evt.BucketID] = append(s.replicationQueue[evt.BucketID], cp)
	return nil
}

func (s *Store) ListPendingReplications(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.ReplicationEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows := s.replicationQueue[bucketID]
	out := make([]meta.ReplicationEvent, 0, len(rows))
	for _, e := range rows {
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *Store) SetObjectReplicationStatus(ctx context.Context, bucketID uuid.UUID, key, versionID, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.objects[bucketID]
	if !ok {
		return meta.ErrBucketNotFound
	}
	versions, ok := bucket[key]
	if !ok || len(versions) == 0 {
		return meta.ErrObjectNotFound
	}
	if versionID == "" {
		versions[0].ReplicationStatus = status
		return nil
	}
	for _, v := range versions {
		if v.VersionID == versionID {
			v.ReplicationStatus = status
			return nil
		}
	}
	return meta.ErrObjectNotFound
}

func (s *Store) EnqueueAccessLog(ctx context.Context, entry *meta.AccessLogEntry) error {
	if entry == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *entry
	if cp.EventID == "" {
		cp.EventID = gocql.TimeUUID().String()
	}
	if cp.Time.IsZero() {
		cp.Time = time.Now().UTC()
	}
	s.accessLogQueue[entry.BucketID] = append(s.accessLogQueue[entry.BucketID], cp)
	return nil
}

func (s *Store) ListPendingAccessLog(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.AccessLogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows := s.accessLogQueue[bucketID]
	out := make([]meta.AccessLogEntry, 0, len(rows))
	for _, e := range rows {
		out = append(out, e)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

type auditEntry struct {
	evt       meta.AuditEvent
	expiresAt time.Time
}

func (s *Store) EnqueueAudit(ctx context.Context, entry *meta.AuditEvent, ttl time.Duration) error {
	if entry == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *entry
	if cp.EventID == "" {
		cp.EventID = gocql.TimeUUID().String()
	}
	if cp.Time.IsZero() {
		cp.Time = nowFn().UTC()
	}
	row := auditEntry{evt: cp}
	if ttl > 0 {
		row.expiresAt = nowFn().Add(ttl)
	}
	s.auditLog[entry.BucketID] = append(s.auditLog[entry.BucketID], row)
	return nil
}

func (s *Store) ListAudit(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.AuditEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	now := nowFn()
	rows := s.auditLog[bucketID]
	kept := rows[:0]
	out := make([]meta.AuditEvent, 0, len(rows))
	for _, r := range rows {
		if !r.expiresAt.IsZero() && !now.Before(r.expiresAt) {
			continue
		}
		kept = append(kept, r)
		if len(out) < limit {
			out = append(out, r.evt)
		}
	}
	s.auditLog[bucketID] = kept
	return out, nil
}

func (s *Store) ListAuditFiltered(ctx context.Context, f meta.AuditFilter) ([]meta.AuditEvent, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	now := nowFn()
	var all []meta.AuditEvent
	collect := func(bid uuid.UUID, rows []auditEntry) {
		kept := rows[:0]
		for _, r := range rows {
			if !r.expiresAt.IsZero() && !now.Before(r.expiresAt) {
				continue
			}
			kept = append(kept, r)
			all = append(all, r.evt)
		}
		s.auditLog[bid] = kept
	}
	if f.BucketScoped {
		collect(f.BucketID, s.auditLog[f.BucketID])
	} else {
		for k, rows := range s.auditLog {
			collect(k, rows)
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

// ListSlowQueries serves the US-003 slow-queries debug endpoint. Filters
// in-process: rows with TotalTimeMS >= minMs whose Time falls within the
// trailing `since` window, sorted by TotalTimeMS desc.
func (s *Store) ListSlowQueries(ctx context.Context, since time.Duration, minMs int, pageToken string) ([]meta.AuditEvent, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	const limit = 100
	if since <= 0 {
		since = 15 * time.Minute
	}
	if minMs < 0 {
		minMs = 0
	}
	now := nowFn()
	cutoff := now.Add(-since)
	var all []meta.AuditEvent
	for bid, rows := range s.auditLog {
		kept := rows[:0]
		for _, r := range rows {
			if !r.expiresAt.IsZero() && !now.Before(r.expiresAt) {
				continue
			}
			kept = append(kept, r)
			if r.evt.TotalTimeMS < minMs {
				continue
			}
			if r.evt.Time.Before(cutoff) {
				continue
			}
			all = append(all, r.evt)
		}
		s.auditLog[bid] = kept
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

// auditDay normalises t to UTC midnight, the partition key for audit_log
// rows in both backends.
func auditDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func (s *Store) ListAuditPartitionsBefore(ctx context.Context, before time.Time) ([]meta.AuditPartition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := auditDay(before)
	now := nowFn()
	type key struct {
		bid uuid.UUID
		day time.Time
	}
	seen := map[key]string{}
	for bid, rows := range s.auditLog {
		kept := rows[:0]
		for _, r := range rows {
			if !r.expiresAt.IsZero() && !now.Before(r.expiresAt) {
				continue
			}
			kept = append(kept, r)
			d := auditDay(r.evt.Time)
			if !d.Before(cutoff) {
				continue
			}
			k := key{bid, d}
			if _, ok := seen[k]; !ok {
				seen[k] = r.evt.Bucket
			}
		}
		s.auditLog[bid] = kept
	}
	out := make([]meta.AuditPartition, 0, len(seen))
	for k, name := range seen {
		out = append(out, meta.AuditPartition{BucketID: k.bid, Bucket: name, Day: k.day})
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
	s.mu.Lock()
	defer s.mu.Unlock()
	d := auditDay(day)
	now := nowFn()
	rows := s.auditLog[bucketID]
	kept := rows[:0]
	var out []meta.AuditEvent
	for _, r := range rows {
		if !r.expiresAt.IsZero() && !now.Before(r.expiresAt) {
			continue
		}
		kept = append(kept, r)
		if !auditDay(r.evt.Time).Equal(d) {
			continue
		}
		out = append(out, r.evt)
	}
	s.auditLog[bucketID] = kept
	sort.Slice(out, func(i, j int) bool { return out[i].EventID < out[j].EventID })
	return out, nil
}

func (s *Store) DeleteAuditPartition(ctx context.Context, bucketID uuid.UUID, day time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := auditDay(day)
	rows := s.auditLog[bucketID]
	kept := rows[:0]
	for _, r := range rows {
		if auditDay(r.evt.Time).Equal(d) {
			continue
		}
		kept = append(kept, r)
	}
	s.auditLog[bucketID] = kept
	return nil
}

func (s *Store) AckAccessLog(ctx context.Context, entry meta.AccessLogEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := s.accessLogQueue[entry.BucketID]
	for i, e := range rows {
		if e.EventID == entry.EventID {
			s.accessLogQueue[entry.BucketID] = append(rows[:i], rows[i+1:]...)
			return nil
		}
	}
	return nil
}

func (s *Store) AckReplication(ctx context.Context, evt meta.ReplicationEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := s.replicationQueue[evt.BucketID]
	for i, e := range rows {
		if e.EventID == evt.EventID {
			s.replicationQueue[evt.BucketID] = append(rows[:i], rows[i+1:]...)
			return nil
		}
	}
	return nil
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
		ShardCount:   64,
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

func (s *Store) SetBucketMfaDelete(ctx context.Context, name, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[name]
	if !ok {
		return meta.ErrBucketNotFound
	}
	b.MfaDelete = state
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
	if !versioned {
		o.VersionID = meta.NullVersionID
		o.IsNull = true
	} else if o.IsNull {
		o.VersionID = meta.NullVersionID
	} else if o.VersionID == "" {
		o.VersionID = gocql.TimeUUID().String()
	}
	raw, err := data.EncodeManifest(o.Manifest)
	if err != nil {
		return err
	}
	cp := *o
	if !versioned {
		bucket[o.Key] = []*meta.Object{&cp}
		s.objectManifestRaw[manifestKey{o.BucketID, o.Key, cp.VersionID}] = raw
		return nil
	}
	if o.IsNull {
		filtered := bucket[o.Key][:0]
		for _, v := range bucket[o.Key] {
			if v.VersionID == meta.NullVersionID {
				continue
			}
			filtered = append(filtered, v)
		}
		bucket[o.Key] = append([]*meta.Object{&cp}, filtered...)
		s.objectManifestRaw[manifestKey{o.BucketID, o.Key, cp.VersionID}] = raw
		return nil
	}
	bucket[o.Key] = append([]*meta.Object{&cp}, bucket[o.Key]...)
	s.objectManifestRaw[manifestKey{o.BucketID, o.Key, cp.VersionID}] = raw
	return nil
}

// DeleteObjectNullReplacement implements US-029: a Suspended-mode unversioned
// DELETE atomically removes the prior null-versioned row (if any) and writes
// a fresh null-versioned delete marker. Other (TimeUUID) versions are kept.
func (s *Store) DeleteObjectNullReplacement(ctx context.Context, bucketID uuid.UUID, key string) (*meta.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.objects[bucketID]
	if !ok {
		return nil, meta.ErrBucketNotFound
	}
	versions := bucket[key]
	filtered := versions[:0]
	for _, v := range versions {
		if v.VersionID == meta.NullVersionID {
			continue
		}
		filtered = append(filtered, v)
	}
	marker := &meta.Object{
		BucketID:       bucketID,
		Key:            key,
		VersionID:      meta.NullVersionID,
		IsLatest:       true,
		IsDeleteMarker: true,
		IsNull:         true,
		Mtime:          time.Now().UTC(),
	}
	bucket[key] = append([]*meta.Object{marker}, filtered...)
	return marker, nil
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
	versionID = meta.ResolveVersionID(versionID)
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
	versionID = meta.ResolveVersionID(versionID)
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

// ScanObjects satisfies meta.RangeScanStore. The in-process tree-map iterates
// keys in sorted order natively, so we can serve a single-shot range scan
// instead of a fan-out + heap-merge — matches the TiKV path's shape.
func (s *Store) ScanObjects(ctx context.Context, bucketID uuid.UUID, opts meta.ListOptions) (*meta.ListResult, error) {
	return s.ListObjects(ctx, bucketID, opts)
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

// AllObjectVersions returns every stored version across every key in the
// bucket as deep copies, with IsLatest set to true on the head of each key's
// version chain. Test-only escape hatch for invariant checks that need the
// full picture without paging through ListObjectVersions (whose 1000-row
// hard cap makes it unfit for the race-harness verification pass).
func (s *Store) AllObjectVersions(bucketID uuid.UUID) []*meta.Object {
	s.mu.RLock()
	defer s.mu.RUnlock()
	bucket, ok := s.objects[bucketID]
	if !ok {
		return nil
	}
	var out []*meta.Object
	for _, vs := range bucket {
		for i, v := range vs {
			cp := *v
			cp.IsLatest = (i == 0)
			out = append(out, &cp)
		}
	}
	return out
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
	versionID = meta.ResolveVersionID(versionID)
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

func (s *Store) SetBucketInventoryConfig(ctx context.Context, bucketID uuid.UUID, configID string, blob []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[bucketID]; !ok {
		return meta.ErrBucketNotFound
	}
	m, ok := s.inventoryConfigs[bucketID]
	if !ok {
		m = make(map[string][]byte)
		s.inventoryConfigs[bucketID] = m
	}
	m[configID] = append([]byte(nil), blob...)
	return nil
}

func (s *Store) GetBucketInventoryConfig(ctx context.Context, bucketID uuid.UUID, configID string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.inventoryConfigs[bucketID]
	if !ok {
		return nil, meta.ErrNoSuchInventoryConfig
	}
	blob, ok := m[configID]
	if !ok {
		return nil, meta.ErrNoSuchInventoryConfig
	}
	return append([]byte(nil), blob...), nil
}

func (s *Store) DeleteBucketInventoryConfig(ctx context.Context, bucketID uuid.UUID, configID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.inventoryConfigs[bucketID]; ok {
		delete(m, configID)
		if len(m) == 0 {
			delete(s.inventoryConfigs, bucketID)
		}
	}
	return nil
}

func (s *Store) ListBucketInventoryConfigs(ctx context.Context, bucketID uuid.UUID) (map[string][]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src, ok := s.inventoryConfigs[bucketID]
	if !ok {
		return map[string][]byte{}, nil
	}
	out := make(map[string][]byte, len(src))
	for id, blob := range src {
		out[id] = append([]byte(nil), blob...)
	}
	return out, nil
}

func (s *Store) CreateAccessPoint(ctx context.Context, ap *meta.AccessPoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accessPoints[ap.Name]; ok {
		return meta.ErrAccessPointAlreadyExists
	}
	cp := *ap
	cp.Policy = append([]byte(nil), ap.Policy...)
	cp.PublicAccessBlock = append([]byte(nil), ap.PublicAccessBlock...)
	s.accessPoints[ap.Name] = &cp
	return nil
}

func (s *Store) GetAccessPoint(ctx context.Context, name string) (*meta.AccessPoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ap, ok := s.accessPoints[name]
	if !ok {
		return nil, meta.ErrAccessPointNotFound
	}
	cp := *ap
	cp.Policy = append([]byte(nil), ap.Policy...)
	cp.PublicAccessBlock = append([]byte(nil), ap.PublicAccessBlock...)
	return &cp, nil
}

func (s *Store) GetAccessPointByAlias(ctx context.Context, alias string) (*meta.AccessPoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ap := range s.accessPoints {
		if ap.Alias == alias {
			cp := *ap
			cp.Policy = append([]byte(nil), ap.Policy...)
			cp.PublicAccessBlock = append([]byte(nil), ap.PublicAccessBlock...)
			return &cp, nil
		}
	}
	return nil, meta.ErrAccessPointNotFound
}

func (s *Store) DeleteAccessPoint(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accessPoints[name]; !ok {
		return meta.ErrAccessPointNotFound
	}
	delete(s.accessPoints, name)
	return nil
}

func (s *Store) ListAccessPoints(ctx context.Context, bucketID uuid.UUID) ([]*meta.AccessPoint, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*meta.AccessPoint, 0, len(s.accessPoints))
	for _, ap := range s.accessPoints {
		if bucketID != uuid.Nil && ap.BucketID != bucketID {
			continue
		}
		cp := *ap
		cp.Policy = append([]byte(nil), ap.Policy...)
		cp.PublicAccessBlock = append([]byte(nil), ap.PublicAccessBlock...)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
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
	var ciphertextSize int64
	partChunks := make([]int, 0, len(parts))
	partSizes := make([]int64, 0, len(parts))
	partChecksums := make([]map[string]string, 0, len(parts))
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
		partChunkCount := 0
		if p.Manifest != nil {
			chunks = append(chunks, p.Manifest.Chunks...)
			partChunkCount = len(p.Manifest.Chunks)
			for _, c := range p.Manifest.Chunks {
				ciphertextSize += c.Size
			}
		}
		partChunks = append(partChunks, partChunkCount)
		partSizes = append(partSizes, p.Size)
		partChecksums = append(partChecksums, p.Checksums)
		totalSize += p.Size
		used[cp.PartNumber] = true
	}

	obj.Manifest = &data.Manifest{
		Class:         obj.StorageClass,
		Size:          ciphertextSize,
		ChunkSize:     data.DefaultChunkSize,
		ETag:          obj.ETag,
		Chunks:        chunks,
		PartChunks:    partChunks,
		PartChecksums: partChecksums,
	}
	obj.Size = totalSize
	obj.PartSizes = partSizes
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

func (s *Store) RecordMultipartCompletion(ctx context.Context, rec *meta.MultipartCompletion, ttl time.Duration) error {
	if rec == nil {
		return nil
	}
	cp := *rec
	if rec.Headers != nil {
		cp.Headers = make(map[string]string, len(rec.Headers))
		maps.Copy(cp.Headers, rec.Headers)
	}
	if rec.Body != nil {
		cp.Body = append([]byte(nil), rec.Body...)
	}
	expires := nowFn().Add(ttl)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mpDone[completionKey{BucketID: rec.BucketID, UploadID: rec.UploadID}] = &completionEntry{rec: &cp, expiresAt: expires}
	return nil
}

func (s *Store) GetMultipartCompletion(ctx context.Context, bucketID uuid.UUID, uploadID string) (*meta.MultipartCompletion, error) {
	key := completionKey{BucketID: bucketID, UploadID: uploadID}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.mpDone[key]
	if !ok {
		return nil, meta.ErrMultipartCompletionNotFound
	}
	if !nowFn().Before(entry.expiresAt) {
		delete(s.mpDone, key)
		return nil, meta.ErrMultipartCompletionNotFound
	}
	out := *entry.rec
	if entry.rec.Headers != nil {
		out.Headers = make(map[string]string, len(entry.rec.Headers))
		maps.Copy(out.Headers, entry.rec.Headers)
	}
	if entry.rec.Body != nil {
		out.Body = append([]byte(nil), entry.rec.Body...)
	}
	return &out, nil
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

func (s *Store) UpdateIAMAccessKeyDisabled(ctx context.Context, accessKeyID string, disabled bool) (*meta.IAMAccessKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ak, ok := s.accessKeys[accessKeyID]
	if !ok {
		return nil, meta.ErrIAMAccessKeyNotFound
	}
	ak.Disabled = disabled
	cp := *ak
	return &cp, nil
}

func cloneManagedPolicy(p *meta.ManagedPolicy) *meta.ManagedPolicy {
	cp := *p
	if p.Document != nil {
		cp.Document = append([]byte(nil), p.Document...)
	}
	return &cp
}

func (s *Store) CreateManagedPolicy(ctx context.Context, p *meta.ManagedPolicy) error {
	if p == nil || p.Arn == "" {
		return meta.ErrManagedPolicyNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.managedPolicies[p.Arn]; ok {
		return meta.ErrManagedPolicyAlreadyExists
	}
	row := *p
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = row.CreatedAt
	}
	if row.Document != nil {
		row.Document = append([]byte(nil), row.Document...)
	}
	s.managedPolicies[p.Arn] = &row
	return nil
}

func (s *Store) GetManagedPolicy(ctx context.Context, arn string) (*meta.ManagedPolicy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.managedPolicies[arn]
	if !ok {
		return nil, meta.ErrManagedPolicyNotFound
	}
	return cloneManagedPolicy(p), nil
}

func (s *Store) ListManagedPolicies(ctx context.Context, pathPrefix string) ([]*meta.ManagedPolicy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*meta.ManagedPolicy, 0, len(s.managedPolicies))
	for _, p := range s.managedPolicies {
		if pathPrefix != "" && !strings.HasPrefix(p.Path, pathPrefix) {
			continue
		}
		out = append(out, cloneManagedPolicy(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Arn < out[j].Arn })
	return out, nil
}

func (s *Store) UpdateManagedPolicyDocument(ctx context.Context, arn string, document []byte, updatedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.managedPolicies[arn]
	if !ok {
		return meta.ErrManagedPolicyNotFound
	}
	p.Document = append([]byte(nil), document...)
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	p.UpdatedAt = updatedAt
	return nil
}

func (s *Store) DeleteManagedPolicy(ctx context.Context, arn string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.managedPolicies[arn]; !ok {
		return meta.ErrManagedPolicyNotFound
	}
	if attached, ok := s.policyUsers[arn]; ok && len(attached) > 0 {
		return meta.ErrPolicyAttached
	}
	delete(s.managedPolicies, arn)
	delete(s.policyUsers, arn)
	return nil
}

func (s *Store) AttachUserPolicy(ctx context.Context, userName, policyArn string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.iamUsers[userName]; !ok {
		return meta.ErrIAMUserNotFound
	}
	if _, ok := s.managedPolicies[policyArn]; !ok {
		return meta.ErrManagedPolicyNotFound
	}
	if attached, ok := s.userPolicies[userName]; ok {
		if _, dup := attached[policyArn]; dup {
			return meta.ErrUserPolicyAlreadyAttached
		}
	}
	if s.userPolicies[userName] == nil {
		s.userPolicies[userName] = make(map[string]time.Time)
	}
	s.userPolicies[userName][policyArn] = time.Now().UTC()
	if s.policyUsers[policyArn] == nil {
		s.policyUsers[policyArn] = make(map[string]struct{})
	}
	s.policyUsers[policyArn][userName] = struct{}{}
	return nil
}

func (s *Store) DetachUserPolicy(ctx context.Context, userName, policyArn string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	attached, ok := s.userPolicies[userName]
	if !ok {
		return meta.ErrUserPolicyNotAttached
	}
	if _, dup := attached[policyArn]; !dup {
		return meta.ErrUserPolicyNotAttached
	}
	delete(attached, policyArn)
	if len(attached) == 0 {
		delete(s.userPolicies, userName)
	}
	if users, ok := s.policyUsers[policyArn]; ok {
		delete(users, userName)
		if len(users) == 0 {
			delete(s.policyUsers, policyArn)
		}
	}
	return nil
}

func (s *Store) ListUserPolicies(ctx context.Context, userName string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.iamUsers[userName]; !ok {
		return nil, meta.ErrIAMUserNotFound
	}
	attached := s.userPolicies[userName]
	out := make([]string, 0, len(attached))
	for arn := range attached {
		out = append(out, arn)
	}
	sort.Strings(out)
	return out, nil
}

func (s *Store) ListPolicyUsers(ctx context.Context, policyArn string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.managedPolicies[policyArn]; !ok {
		return nil, meta.ErrManagedPolicyNotFound
	}
	users := s.policyUsers[policyArn]
	out := make([]string, 0, len(users))
	for u := range users {
		out = append(out, u)
	}
	sort.Strings(out)
	return out, nil
}

func (s *Store) UpdateObjectSSEWrap(ctx context.Context, bucketID uuid.UUID, key, versionID string, wrapped []byte, keyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.objects[bucketID]
	if !ok {
		return meta.ErrBucketNotFound
	}
	versions, ok := bucket[key]
	if !ok || len(versions) == 0 {
		return meta.ErrObjectNotFound
	}
	for _, v := range versions {
		if versionID == "" || v.VersionID == versionID {
			v.SSEKey = append([]byte(nil), wrapped...)
			v.SSEKeyID = keyID
			return nil
		}
	}
	return meta.ErrObjectNotFound
}

func (s *Store) UpdateMultipartUploadSSEWrap(ctx context.Context, bucketID uuid.UUID, uploadID string, wrapped []byte, keyID string) error {
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
	st.upload.SSEKey = append([]byte(nil), wrapped...)
	st.upload.SSEKeyID = keyID
	return nil
}

// GetObjectManifestRaw returns the raw, persisted manifest blob for the
// given object version (US-049). Returns ErrObjectNotFound when no row
// exists for the version.
func (s *Store) GetObjectManifestRaw(ctx context.Context, bucketID uuid.UUID, key, versionID string) ([]byte, error) {
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
	resolved := meta.ResolveVersionID(versionID)
	for _, v := range versions {
		if resolved == "" || v.VersionID == resolved {
			raw := s.objectManifestRaw[manifestKey{bucketID, key, v.VersionID}]
			return append([]byte(nil), raw...), nil
		}
	}
	return nil, meta.ErrObjectNotFound
}

// UpdateObjectManifestRaw overwrites the raw manifest blob for the given
// object version and re-decodes it into the live *data.Manifest pointer so
// subsequent GetObject reads observe the same logical state. Used by the
// manifest rewriter (US-049).
func (s *Store) UpdateObjectManifestRaw(ctx context.Context, bucketID uuid.UUID, key, versionID string, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, ok := s.objects[bucketID]
	if !ok {
		return meta.ErrBucketNotFound
	}
	versions, ok := bucket[key]
	if !ok || len(versions) == 0 {
		return meta.ErrObjectNotFound
	}
	resolved := meta.ResolveVersionID(versionID)
	for _, v := range versions {
		if resolved == "" || v.VersionID == resolved {
			m, err := data.DecodeManifest(raw)
			if err != nil {
				return err
			}
			v.Manifest = m
			s.objectManifestRaw[manifestKey{bucketID, key, v.VersionID}] = append([]byte(nil), raw...)
			return nil
		}
	}
	return meta.ErrObjectNotFound
}

func (s *Store) SetRewrapProgress(ctx context.Context, p *meta.RewrapProgress) error {
	if p == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *p
	cp.UpdatedAt = time.Now().UTC()
	s.rewrapProgress[p.BucketID] = &cp
	return nil
}

func (s *Store) GetRewrapProgress(ctx context.Context, bucketID uuid.UUID) (*meta.RewrapProgress, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rp, ok := s.rewrapProgress[bucketID]
	if !ok {
		return nil, meta.ErrNoRewrapProgress
	}
	cp := *rp
	return &cp, nil
}

// findBucketByID resolves a bucket by its UUID. The memory backend keys
// `buckets` by name, so this is an O(N) scan; acceptable for the test backend.
func (s *Store) findBucketByID(bucketID uuid.UUID) *meta.Bucket {
	for _, b := range s.buckets {
		if b.ID == bucketID {
			return b
		}
	}
	return nil
}

func (s *Store) StartReshard(ctx context.Context, bucketID uuid.UUID, target int) (*meta.ReshardJob, error) {
	if !meta.IsValidShardCount(target) {
		return nil, meta.ErrReshardInvalidTarget
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.findBucketByID(bucketID)
	if b == nil {
		return nil, meta.ErrBucketNotFound
	}
	if target <= b.ShardCount {
		return nil, meta.ErrReshardInvalidTarget
	}
	if _, ok := s.reshardJobs[bucketID]; ok {
		return nil, meta.ErrReshardInProgress
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
	cp := *job
	s.reshardJobs[bucketID] = &cp
	b.TargetShardCount = target
	out := *job
	return &out, nil
}

func (s *Store) GetReshardJob(ctx context.Context, bucketID uuid.UUID) (*meta.ReshardJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.reshardJobs[bucketID]
	if !ok {
		return nil, meta.ErrReshardNotFound
	}
	cp := *j
	return &cp, nil
}

func (s *Store) UpdateReshardJob(ctx context.Context, job *meta.ReshardJob) error {
	if job == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.reshardJobs[job.BucketID]; !ok {
		return meta.ErrReshardNotFound
	}
	cp := *job
	cp.UpdatedAt = time.Now().UTC()
	s.reshardJobs[job.BucketID] = &cp
	return nil
}

func (s *Store) CompleteReshard(ctx context.Context, bucketID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.reshardJobs[bucketID]
	if !ok {
		return meta.ErrReshardNotFound
	}
	b := s.findBucketByID(bucketID)
	if b == nil {
		return meta.ErrBucketNotFound
	}
	b.ShardCount = job.Target
	b.TargetShardCount = 0
	delete(s.reshardJobs, bucketID)
	return nil
}

func (s *Store) ListReshardJobs(ctx context.Context) ([]*meta.ReshardJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*meta.ReshardJob, 0, len(s.reshardJobs))
	for _, j := range s.reshardJobs {
		cp := *j
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bucket < out[j].Bucket })
	return out, nil
}

// CreateAdminJob persists a fresh admin job row keyed on job.ID. Returns
// ErrAdminJobAlreadyExists if a row already exists.
func (s *Store) CreateAdminJob(ctx context.Context, job *meta.AdminJob) error {
	if job == nil || job.ID == "" {
		return meta.ErrAdminJobNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.adminJobs[job.ID]; ok {
		return meta.ErrAdminJobAlreadyExists
	}
	cp := *job
	s.adminJobs[job.ID] = &cp
	return nil
}

// GetAdminJob returns the row addressed by id; ErrAdminJobNotFound otherwise.
func (s *Store) GetAdminJob(ctx context.Context, id string) (*meta.AdminJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.adminJobs[id]
	if !ok {
		return nil, meta.ErrAdminJobNotFound
	}
	cp := *j
	return &cp, nil
}

// UpdateAdminJob overwrites State/Message/Deleted/UpdatedAt/FinishedAt for
// the existing row. ErrAdminJobNotFound when no row exists. Kind/Bucket/
// StartedAt are immutable post-create; this method ignores any new value.
func (s *Store) UpdateAdminJob(ctx context.Context, job *meta.AdminJob) error {
	if job == nil || job.ID == "" {
		return meta.ErrAdminJobNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.adminJobs[job.ID]
	if !ok {
		return meta.ErrAdminJobNotFound
	}
	cur.State = job.State
	cur.Message = job.Message
	cur.Deleted = job.Deleted
	cur.UpdatedAt = job.UpdatedAt
	cur.FinishedAt = job.FinishedAt
	return nil
}

func (s *Store) Close() error { return nil }

// Compile-time guarantees that *Store satisfies both meta.Store and the
// optional meta.RangeScanStore capability surface (US-012).
var (
	_ meta.Store          = (*Store)(nil)
	_ meta.RangeScanStore = (*Store)(nil)
)
