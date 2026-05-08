// Worker queue + DLQ + access-log buffer surfaces (US-010).
//
// Five timestamp-ordered FIFO queues live under their own top-level prefixes:
//
//	prefixGCQueue          — chunk-deletion intents (per-region)
//	prefixNotifyQueue      — S3 event notifications (per-bucket)
//	prefixNotifyDLQ        — notify-worker dead-letter (per-bucket)
//	prefixReplicationQueue — cross-region replication intents (per-bucket)
//	prefixAccessLogQueue   — server-access-log rows (per-bucket)
//
// Every queue key is `<prefix><partition><tsNano8-BE><eventID-escaped>` (see
// keys.go::queueKey). The big-endian ts8 segment makes a forward range scan
// claim oldest first; the escaped event-id segment breaks ties when two
// events land in the same nanosecond. Ack recomputes the key from the
// returned event's timestamp + EventID and Deletes the row — no per-row
// secondary index needed.
//
// Row payloads are JSON-encoded structs with abbreviated field names; the
// encoding is internal so abbreviations are safe. New fields land
// additively with `json:",omitempty"` (mirrors the bucket / object / audit
// codec convention in this package).
package tikv

import (
	"context"
	"encoding/json"
	"time"

	"github.com/gocql/gocql"
	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// queueListLimitDefault mirrors the cap memory + cassandra backends apply
// when the caller passes limit ≤ 0 or > 1000. Keeps a misuse from pulling
// the whole queue.
const queueListLimitDefault = 1000

// ----------------------------------------------------------------------------
// GC queue (chunk-deletion intents)
// ----------------------------------------------------------------------------

// gcRow is the persisted shape of one gc_queue row. ChunkRef fields are
// flattened in so the row decodes to a meta.GCEntry without round-tripping
// through a nested struct.
type gcRow struct {
	OID        string    `json:"o"`
	Pool       string    `json:"p,omitempty"`
	Cluster    string    `json:"c,omitempty"`
	Namespace  string    `json:"n,omitempty"`
	Size       int64     `json:"sz,omitempty"`
	EnqueuedAt time.Time `json:"e"`
}

func (s *Store) EnqueueChunkDeletion(ctx context.Context, region string, chunks []data.ChunkRef) error {
	if len(chunks) == 0 {
		return nil
	}
	now := time.Now().UTC()
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	for _, c := range chunks {
		row := gcRow{
			OID:        c.OID,
			Pool:       c.Pool,
			Cluster:    c.Cluster,
			Namespace:  c.Namespace,
			Size:       c.Size,
			EnqueuedAt: now,
		}
		raw, mErr := json.Marshal(&row)
		if mErr != nil {
			err = mErr
			return err
		}
		key := GCQueueKey(region, uint64(now.UnixNano()), c.OID)
		if err = txn.Set(key, raw); err != nil {
			return err
		}
	}
	return txn.Commit(ctx)
}

func (s *Store) ListGCEntries(ctx context.Context, region string, before time.Time, limit int) ([]meta.GCEntry, error) {
	return s.ListGCEntriesShard(ctx, region, 0, 1, before, limit)
}

// ListGCEntriesShard is a transitional implementation that scans the entire
// legacy `gc/<region>/` prefix and filters by `fnv32a(oid) % 1024 %
// shardCount == shardID` post-fetch. US-003 replaces this with a per-prefix
// scan over the new `gc/<region>/<shardID2BE>/` key shape so each shard
// reads exactly its 1024/shardCount logical-shard prefixes.
func (s *Store) ListGCEntriesShard(ctx context.Context, region string, shardID, shardCount int, before time.Time, limit int) ([]meta.GCEntry, error) {
	if shardCount <= 0 {
		shardCount = 1
	}
	if shardID < 0 || shardID >= shardCount {
		return nil, nil
	}
	if limit <= 0 || limit > queueListLimitDefault {
		limit = queueListLimitDefault
	}
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	prefix := GCQueuePrefix(region)
	pairs, err := txn.Scan(ctx, prefix, prefixEnd(prefix), 0)
	if err != nil {
		return nil, err
	}
	out := make([]meta.GCEntry, 0, len(pairs))
	for _, p := range pairs {
		var r gcRow
		if uErr := json.Unmarshal(p.Value, &r); uErr != nil {
			return nil, uErr
		}
		if r.EnqueuedAt.After(before) {
			continue
		}
		sid := meta.GCShardID(r.OID)
		if sid%shardCount != shardID {
			continue
		}
		out = append(out, meta.GCEntry{
			Chunk: data.ChunkRef{
				Cluster:   r.Cluster,
				Pool:      r.Pool,
				Namespace: r.Namespace,
				OID:       r.OID,
				Size:      r.Size,
			},
			EnqueuedAt: r.EnqueuedAt,
			ShardID:    sid,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *Store) AckGCEntry(ctx context.Context, region string, e meta.GCEntry) error {
	key := GCQueueKey(region, uint64(e.EnqueuedAt.UnixNano()), e.Chunk.OID)
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.Delete(key); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// ----------------------------------------------------------------------------
// Notification queue
// ----------------------------------------------------------------------------

type notifyRow struct {
	BucketID   string    `json:"b"`
	Bucket     string    `json:"bn,omitempty"`
	Key        string    `json:"k,omitempty"`
	EventID    string    `json:"e"`
	EventName  string    `json:"en,omitempty"`
	EventTime  time.Time `json:"t"`
	ConfigID   string    `json:"ci,omitempty"`
	TargetType string    `json:"tt,omitempty"`
	TargetARN  string    `json:"ta,omitempty"`
	Payload    []byte    `json:"p,omitempty"`
}

func encodeNotify(evt *meta.NotificationEvent) ([]byte, error) {
	row := notifyRow{
		BucketID:   evt.BucketID.String(),
		Bucket:     evt.Bucket,
		Key:        evt.Key,
		EventID:    evt.EventID,
		EventName:  evt.EventName,
		EventTime:  evt.EventTime,
		ConfigID:   evt.ConfigID,
		TargetType: evt.TargetType,
		TargetARN:  evt.TargetARN,
		Payload:    evt.Payload,
	}
	return json.Marshal(&row)
}

func decodeNotify(raw []byte) (meta.NotificationEvent, error) {
	var row notifyRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return meta.NotificationEvent{}, err
	}
	bid, err := uuidFromString(row.BucketID)
	if err != nil {
		return meta.NotificationEvent{}, err
	}
	return meta.NotificationEvent{
		BucketID:   bid,
		Bucket:     row.Bucket,
		Key:        row.Key,
		EventID:    row.EventID,
		EventName:  row.EventName,
		EventTime:  row.EventTime,
		ConfigID:   row.ConfigID,
		TargetType: row.TargetType,
		TargetARN:  row.TargetARN,
		Payload:    append([]byte(nil), row.Payload...),
	}, nil
}

func (s *Store) EnqueueNotification(ctx context.Context, evt *meta.NotificationEvent) error {
	if evt == nil {
		return nil
	}
	if evt.EventID == "" {
		evt.EventID = gocql.TimeUUID().String()
	}
	if evt.EventTime.IsZero() {
		evt.EventTime = time.Now().UTC()
	}
	raw, err := encodeNotify(evt)
	if err != nil {
		return err
	}
	key := NotifyQueueKey(evt.BucketID, uint64(evt.EventTime.UnixNano()), evt.EventID)
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.Set(key, raw); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

func (s *Store) ListPendingNotifications(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.NotificationEvent, error) {
	if limit <= 0 || limit > queueListLimitDefault {
		limit = queueListLimitDefault
	}
	prefix := NotifyQueuePrefix(bucketID)
	pairs, err := s.scanQueue(ctx, prefix, limit)
	if err != nil {
		return nil, err
	}
	out := make([]meta.NotificationEvent, 0, len(pairs))
	for _, p := range pairs {
		evt, dErr := decodeNotify(p.Value)
		if dErr != nil {
			return nil, dErr
		}
		out = append(out, evt)
	}
	return out, nil
}

func (s *Store) AckNotification(ctx context.Context, evt meta.NotificationEvent) error {
	if evt.EventID == "" {
		return nil
	}
	key := NotifyQueueKey(evt.BucketID, uint64(evt.EventTime.UnixNano()), evt.EventID)
	return s.deleteQueueRow(ctx, key)
}

// ----------------------------------------------------------------------------
// Notification DLQ
// ----------------------------------------------------------------------------

type notifyDLQRow struct {
	notifyRow
	Attempts   int       `json:"at,omitempty"`
	Reason     string    `json:"rsn,omitempty"`
	EnqueuedAt time.Time `json:"eq"`
}

func (s *Store) EnqueueNotificationDLQ(ctx context.Context, entry *meta.NotificationDLQEntry) error {
	if entry == nil {
		return nil
	}
	if entry.EventID == "" {
		entry.EventID = gocql.TimeUUID().String()
	}
	if entry.EnqueuedAt.IsZero() {
		entry.EnqueuedAt = time.Now().UTC()
	}
	row := notifyDLQRow{
		notifyRow: notifyRow{
			BucketID:   entry.BucketID.String(),
			Bucket:     entry.Bucket,
			Key:        entry.Key,
			EventID:    entry.EventID,
			EventName:  entry.EventName,
			EventTime:  entry.EventTime,
			ConfigID:   entry.ConfigID,
			TargetType: entry.TargetType,
			TargetARN:  entry.TargetARN,
			Payload:    entry.Payload,
		},
		Attempts:   entry.Attempts,
		Reason:     entry.Reason,
		EnqueuedAt: entry.EnqueuedAt,
	}
	raw, err := json.Marshal(&row)
	if err != nil {
		return err
	}
	key := NotifyDLQKey(entry.BucketID, uint64(entry.EnqueuedAt.UnixNano()), entry.EventID)
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.Set(key, raw); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

func (s *Store) ListNotificationDLQ(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.NotificationDLQEntry, error) {
	if limit <= 0 || limit > queueListLimitDefault {
		limit = queueListLimitDefault
	}
	prefix := NotifyDLQPrefix(bucketID)
	pairs, err := s.scanQueue(ctx, prefix, limit)
	if err != nil {
		return nil, err
	}
	out := make([]meta.NotificationDLQEntry, 0, len(pairs))
	for _, p := range pairs {
		var row notifyDLQRow
		if uErr := json.Unmarshal(p.Value, &row); uErr != nil {
			return nil, uErr
		}
		bid, pErr := uuidFromString(row.BucketID)
		if pErr != nil {
			return nil, pErr
		}
		out = append(out, meta.NotificationDLQEntry{
			NotificationEvent: meta.NotificationEvent{
				BucketID:   bid,
				Bucket:     row.Bucket,
				Key:        row.Key,
				EventID:    row.EventID,
				EventName:  row.EventName,
				EventTime:  row.EventTime,
				ConfigID:   row.ConfigID,
				TargetType: row.TargetType,
				TargetARN:  row.TargetARN,
				Payload:    append([]byte(nil), row.Payload...),
			},
			Attempts:   row.Attempts,
			Reason:     row.Reason,
			EnqueuedAt: row.EnqueuedAt,
		})
	}
	return out, nil
}

// ----------------------------------------------------------------------------
// Replication queue
// ----------------------------------------------------------------------------

type replicationRow struct {
	BucketID            string    `json:"b"`
	Bucket              string    `json:"bn,omitempty"`
	Key                 string    `json:"k,omitempty"`
	VersionID           string    `json:"v,omitempty"`
	EventID             string    `json:"e"`
	EventName           string    `json:"en,omitempty"`
	EventTime           time.Time `json:"t"`
	RuleID              string    `json:"ri,omitempty"`
	DestinationBucket   string    `json:"db,omitempty"`
	DestinationEndpoint string    `json:"de,omitempty"`
	StorageClass        string    `json:"sc,omitempty"`
}

func (s *Store) EnqueueReplication(ctx context.Context, evt *meta.ReplicationEvent) error {
	if evt == nil {
		return nil
	}
	if evt.EventID == "" {
		evt.EventID = gocql.TimeUUID().String()
	}
	if evt.EventTime.IsZero() {
		evt.EventTime = time.Now().UTC()
	}
	row := replicationRow{
		BucketID:            evt.BucketID.String(),
		Bucket:              evt.Bucket,
		Key:                 evt.Key,
		VersionID:           evt.VersionID,
		EventID:             evt.EventID,
		EventName:           evt.EventName,
		EventTime:           evt.EventTime,
		RuleID:              evt.RuleID,
		DestinationBucket:   evt.DestinationBucket,
		DestinationEndpoint: evt.DestinationEndpoint,
		StorageClass:        evt.StorageClass,
	}
	raw, err := json.Marshal(&row)
	if err != nil {
		return err
	}
	key := ReplicationQueueKey(evt.BucketID, uint64(evt.EventTime.UnixNano()), evt.EventID)
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.Set(key, raw); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

func (s *Store) ListPendingReplications(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.ReplicationEvent, error) {
	if limit <= 0 || limit > queueListLimitDefault {
		limit = queueListLimitDefault
	}
	prefix := ReplicationQueuePrefix(bucketID)
	pairs, err := s.scanQueue(ctx, prefix, limit)
	if err != nil {
		return nil, err
	}
	out := make([]meta.ReplicationEvent, 0, len(pairs))
	for _, p := range pairs {
		var row replicationRow
		if uErr := json.Unmarshal(p.Value, &row); uErr != nil {
			return nil, uErr
		}
		bid, pErr := uuidFromString(row.BucketID)
		if pErr != nil {
			return nil, pErr
		}
		out = append(out, meta.ReplicationEvent{
			BucketID:            bid,
			Bucket:              row.Bucket,
			Key:                 row.Key,
			VersionID:           row.VersionID,
			EventID:             row.EventID,
			EventName:           row.EventName,
			EventTime:           row.EventTime,
			RuleID:              row.RuleID,
			DestinationBucket:   row.DestinationBucket,
			DestinationEndpoint: row.DestinationEndpoint,
			StorageClass:        row.StorageClass,
		})
	}
	return out, nil
}

func (s *Store) AckReplication(ctx context.Context, evt meta.ReplicationEvent) error {
	if evt.EventID == "" {
		return nil
	}
	key := ReplicationQueueKey(evt.BucketID, uint64(evt.EventTime.UnixNano()), evt.EventID)
	return s.deleteQueueRow(ctx, key)
}

// ----------------------------------------------------------------------------
// Access-log buffer
// ----------------------------------------------------------------------------

type accessLogRow struct {
	BucketID     string    `json:"b"`
	Bucket       string    `json:"bn,omitempty"`
	EventID      string    `json:"e"`
	Time         time.Time `json:"t"`
	RequestID    string    `json:"rq,omitempty"`
	Principal    string    `json:"pr,omitempty"`
	SourceIP     string    `json:"ip,omitempty"`
	Op           string    `json:"op,omitempty"`
	Key          string    `json:"k,omitempty"`
	Status       int       `json:"st,omitempty"`
	BytesSent    int64     `json:"bs,omitempty"`
	ObjectSize   int64     `json:"os,omitempty"`
	TotalTimeMS  int       `json:"tt,omitempty"`
	TurnAroundMS int       `json:"ta,omitempty"`
	Referrer     string    `json:"rf,omitempty"`
	UserAgent    string    `json:"ua,omitempty"`
	VersionID    string    `json:"v,omitempty"`
}

func (s *Store) EnqueueAccessLog(ctx context.Context, entry *meta.AccessLogEntry) error {
	if entry == nil {
		return nil
	}
	if entry.EventID == "" {
		entry.EventID = gocql.TimeUUID().String()
	}
	if entry.Time.IsZero() {
		entry.Time = time.Now().UTC()
	}
	row := accessLogRow{
		BucketID:     entry.BucketID.String(),
		Bucket:       entry.Bucket,
		EventID:      entry.EventID,
		Time:         entry.Time,
		RequestID:    entry.RequestID,
		Principal:    entry.Principal,
		SourceIP:     entry.SourceIP,
		Op:           entry.Op,
		Key:          entry.Key,
		Status:       entry.Status,
		BytesSent:    entry.BytesSent,
		ObjectSize:   entry.ObjectSize,
		TotalTimeMS:  entry.TotalTimeMS,
		TurnAroundMS: entry.TurnAroundMS,
		Referrer:     entry.Referrer,
		UserAgent:    entry.UserAgent,
		VersionID:    entry.VersionID,
	}
	raw, err := json.Marshal(&row)
	if err != nil {
		return err
	}
	key := AccessLogQueueKey(entry.BucketID, uint64(entry.Time.UnixNano()), entry.EventID)
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.Set(key, raw); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

func (s *Store) ListPendingAccessLog(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.AccessLogEntry, error) {
	if limit <= 0 || limit > queueListLimitDefault {
		limit = queueListLimitDefault
	}
	prefix := AccessLogQueuePrefix(bucketID)
	pairs, err := s.scanQueue(ctx, prefix, limit)
	if err != nil {
		return nil, err
	}
	out := make([]meta.AccessLogEntry, 0, len(pairs))
	for _, p := range pairs {
		var row accessLogRow
		if uErr := json.Unmarshal(p.Value, &row); uErr != nil {
			return nil, uErr
		}
		bid, pErr := uuidFromString(row.BucketID)
		if pErr != nil {
			return nil, pErr
		}
		out = append(out, meta.AccessLogEntry{
			BucketID:     bid,
			Bucket:       row.Bucket,
			EventID:      row.EventID,
			Time:         row.Time,
			RequestID:    row.RequestID,
			Principal:    row.Principal,
			SourceIP:     row.SourceIP,
			Op:           row.Op,
			Key:          row.Key,
			Status:       row.Status,
			BytesSent:    row.BytesSent,
			ObjectSize:   row.ObjectSize,
			TotalTimeMS:  row.TotalTimeMS,
			TurnAroundMS: row.TurnAroundMS,
			Referrer:     row.Referrer,
			UserAgent:    row.UserAgent,
			VersionID:    row.VersionID,
		})
	}
	return out, nil
}

func (s *Store) AckAccessLog(ctx context.Context, entry meta.AccessLogEntry) error {
	if entry.EventID == "" {
		return nil
	}
	key := AccessLogQueueKey(entry.BucketID, uint64(entry.Time.UnixNano()), entry.EventID)
	return s.deleteQueueRow(ctx, key)
}

// ----------------------------------------------------------------------------
// Shared helpers
// ----------------------------------------------------------------------------

// scanQueue pulls up to limit rows from the per-(prefix,partition) range in
// ascending key order — the big-endian ts8 segment makes that "oldest first"
// FIFO claim order. The optimistic txn is rolled back unconditionally
// (read-only).
func (s *Store) scanQueue(ctx context.Context, prefix []byte, limit int) ([]kvPair, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	pairs, err := txn.Scan(ctx, prefix, prefixEnd(prefix), limit)
	if err != nil {
		return nil, err
	}
	return pairs, nil
}

// deleteQueueRow is the optimistic single-row delete used by every Ack
// helper. Optimistic is fine because Ack is the terminal action on a row —
// concurrent claimers (when the worker fleet eventually grows that surface)
// will be coordinated above this layer.
func (s *Store) deleteQueueRow(ctx context.Context, key []byte) error {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.Delete(key); err != nil {
		return err
	}
	return txn.Commit(ctx)
}
