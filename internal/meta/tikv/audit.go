// Audit log surface (US-009). TiKV stores rows under
// AuditLogKey(bucketID, dayEpoch, eventID); the (bucket, day) prefix is
// recoverable from the key so partition reads/deletes scan a tight range
// and the audit-export worker (US-046) walks aged partitions cheaply.
//
// TiKV has no native TTL — every row carries an ExpiresAt stamp. Reads
// lazy-skip expired rows so a delayed sweep tick never surfaces stale
// data; the sweeper goroutine (sweeper.go) eager-deletes them in the
// background. Mirrors Cassandra's `USING TTL` shape from the read side.
package tikv

import (
	"context"
	"sort"
	"time"

	"github.com/gocql/gocql"
	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// auditDayEpoch normalises t to UTC midnight and returns the days-since-
// 1970 day index used as the partition discriminator in AuditLogKey.
func auditDayEpoch(t time.Time) uint32 {
	t = t.UTC()
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return uint32(d.Unix() / 86400)
}

// auditDayFromEpoch reverses auditDayEpoch — returns the UTC midnight
// time.Time for the given day index.
func auditDayFromEpoch(e uint32) time.Time {
	return time.Unix(int64(e)*86400, 0).UTC()
}

// EnqueueAudit appends one audit row. ttl > 0 stamps an ExpiresAt that
// the sweeper / readers honour; ttl == 0 means "no expiry" (operator-
// driven retention via DeleteAuditPartition).
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
	if entry.Bucket == "" {
		entry.Bucket = "-"
	}
	day := auditDayEpoch(entry.Time)
	key := AuditLogKey(entry.BucketID, day, entry.EventID)
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().UTC().Add(ttl)
	}
	payload, err := encodeAudit(entry, expiresAt)
	if err != nil {
		return err
	}
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.Set(key, payload); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// scanAuditRange scans the supplied [start, end) and returns the decoded
// non-expired rows. Pagination is one batched range scan — audit rows
// are bounded by retention so the working set per partition stays small.
func (s *Store) scanAuditRange(ctx context.Context, start, end []byte) ([]meta.AuditEvent, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	pairs, err := txn.Scan(ctx, start, end, 0)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	out := make([]meta.AuditEvent, 0, len(pairs))
	for _, p := range pairs {
		evt, expiresAt, err := decodeAudit(p.Value)
		if err != nil {
			return nil, err
		}
		if !expiresAt.IsZero() && !now.Before(expiresAt) {
			continue
		}
		out = append(out, evt)
	}
	return out, nil
}

// ListAudit returns up to limit recent audit rows for a bucket — used by
// the operator IAM ?audit endpoint when called bucket-scoped without
// further filters. The default 30-day window matches the default
// retention so a fresh inspection still catches everything not yet
// swept.
func (s *Store) ListAudit(ctx context.Context, bucketID uuid.UUID, limit int) ([]meta.AuditEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	prefix := AuditLogBucketPrefix(bucketID)
	all, err := s.scanAuditRange(ctx, prefix, prefixEnd(prefix))
	if err != nil {
		return nil, err
	}
	// AuditLogKey orders ascending by (day, eventID). The expected client
	// shape is "newest first"; reverse-sort by Time desc + EventID desc to
	// match the memory/Cassandra contract.
	sort.Slice(all, func(i, j int) bool {
		if !all[i].Time.Equal(all[j].Time) {
			return all[i].Time.After(all[j].Time)
		}
		return all[i].EventID > all[j].EventID
	})
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// ListAuditFiltered serves the [iam root]-gated /?audit endpoint. The
// shape mirrors Cassandra: time-window default of "last 30 days" when
// either bound is zero; Continuation is the EventID of the last row on
// the previous page; the next page's pagination token is the EventID of
// the last row of the page when len(out) >= limit.
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
	var rows []meta.AuditEvent
	if f.BucketScoped {
		prefix := AuditLogBucketPrefix(f.BucketID)
		got, err := s.scanAuditRange(ctx, prefix, prefixEnd(prefix))
		if err != nil {
			return nil, "", err
		}
		rows = got
	} else {
		got, err := s.scanAuditRange(ctx, []byte(prefixAuditLog), prefixEnd([]byte(prefixAuditLog)))
		if err != nil {
			return nil, "", err
		}
		rows = got
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].Time.Equal(rows[j].Time) {
			return rows[i].Time.After(rows[j].Time)
		}
		return rows[i].EventID > rows[j].EventID
	})
	out := make([]meta.AuditEvent, 0, limit)
	started := f.Continuation == ""
	for _, e := range rows {
		if !f.Start.IsZero() && e.Time.Before(f.Start) {
			continue
		}
		if !f.End.IsZero() && e.Time.After(f.End) {
			continue
		}
		if !start.IsZero() && e.Time.Before(start) {
			continue
		}
		if !end.IsZero() && e.Time.After(end) {
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

// ListSlowQueries serves the US-003 slow-queries debug endpoint. The
// TiKV layout has no native predicate index — the implementation scans
// the audit prefix and filters in-process for rows whose TotalTimeMS
// >= minMs and whose Time falls within the trailing `since` window.
// Rows are sorted by TotalTimeMS desc; pagination uses EventID of the
// last row as the next-page token.
func (s *Store) ListSlowQueries(ctx context.Context, since time.Duration, minMs int, pageToken string) ([]meta.AuditEvent, string, error) {
	const limit = 100
	if since <= 0 {
		since = 15 * time.Minute
	}
	if minMs < 0 {
		minMs = 0
	}
	rows, err := s.scanAuditRange(ctx, []byte(prefixAuditLog), prefixEnd([]byte(prefixAuditLog)))
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	cutoff := now.Add(-since)
	filtered := rows[:0]
	for _, r := range rows {
		if r.TotalTimeMS < minMs {
			continue
		}
		if r.Time.Before(cutoff) {
			continue
		}
		filtered = append(filtered, r)
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].TotalTimeMS != filtered[j].TotalTimeMS {
			return filtered[i].TotalTimeMS > filtered[j].TotalTimeMS
		}
		if !filtered[i].Time.Equal(filtered[j].Time) {
			return filtered[i].Time.After(filtered[j].Time)
		}
		return filtered[i].EventID > filtered[j].EventID
	})
	out := make([]meta.AuditEvent, 0, limit)
	started := pageToken == ""
	for _, e := range filtered {
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

// ListAuditPartitionsBefore returns every (bucket, day) partition whose
// day is strictly older than the UTC day containing `before`. The
// audit-export worker uses this to enumerate fully-aged partitions
// ready for export+delete; it is independent of the retention sweeper
// (which deletes by per-row ExpiresAt, not by partition age).
func (s *Store) ListAuditPartitionsBefore(ctx context.Context, before time.Time) ([]meta.AuditPartition, error) {
	cutoffEpoch := auditDayEpoch(before)
	rows, err := s.scanAuditRange(ctx, []byte(prefixAuditLog), prefixEnd([]byte(prefixAuditLog)))
	if err != nil {
		return nil, err
	}
	type key struct {
		bid uuid.UUID
		day uint32
	}
	seen := map[key]string{}
	for _, e := range rows {
		d := auditDayEpoch(e.Time)
		if d >= cutoffEpoch {
			continue
		}
		k := key{e.BucketID, d}
		if _, ok := seen[k]; !ok {
			seen[k] = e.Bucket
		}
	}
	out := make([]meta.AuditPartition, 0, len(seen))
	for k, name := range seen {
		if name == "" {
			name = "-"
		}
		out = append(out, meta.AuditPartition{
			BucketID: k.bid,
			Bucket:   name,
			Day:      auditDayFromEpoch(k.day),
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

// ReadAuditPartition returns every row in a single (bucket, day)
// partition, sorted ascending by EventID for deterministic export.
func (s *Store) ReadAuditPartition(ctx context.Context, bucketID uuid.UUID, day time.Time) ([]meta.AuditEvent, error) {
	dayEpoch := auditDayEpoch(day)
	prefix := AuditLogDayPrefix(bucketID, dayEpoch)
	rows, err := s.scanAuditRange(ctx, prefix, prefixEnd(prefix))
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].EventID < rows[j].EventID })
	return rows, nil
}

// DeleteAuditPartition drops every row in the given (bucket, day)
// partition. Issued by the audit-export worker after a successful
// upload of the partition's contents.
func (s *Store) DeleteAuditPartition(ctx context.Context, bucketID uuid.UUID, day time.Time) error {
	dayEpoch := auditDayEpoch(day)
	prefix := AuditLogDayPrefix(bucketID, dayEpoch)
	_, err := s.deleteAuditRange(ctx, prefix, prefixEnd(prefix), nil)
	return err
}

// deleteAuditRange enumerates [start, end) and deletes every row whose
// keep predicate (when supplied) returns false. A nil keep deletes
// every row in the range. Returns the number of rows deleted; sweeper
// callers use it to drive the strata_meta_tikv_audit_sweep_deleted_total
// counter. Performed in batched txns so a partition with millions of
// rows does not exceed transaction-size limits.
func (s *Store) deleteAuditRange(ctx context.Context, start, end []byte, keep func(meta.AuditEvent, time.Time) bool) (int, error) {
	const batch = 256
	cursor := append([]byte(nil), start...)
	deleted := 0
	for {
		select {
		case <-ctx.Done():
			return deleted, ctx.Err()
		default:
		}
		txn, err := s.kv.Begin(ctx, false)
		if err != nil {
			return deleted, err
		}
		pairs, err := txn.Scan(ctx, cursor, end, batch)
		if err != nil {
			_ = txn.Rollback()
			return deleted, err
		}
		if len(pairs) == 0 {
			_ = txn.Rollback()
			return deleted, nil
		}
		anyDeleted := false
		for _, p := range pairs {
			if keep != nil {
				evt, expiresAt, decErr := decodeAudit(p.Value)
				if decErr != nil {
					continue
				}
				if keep(evt, expiresAt) {
					continue
				}
			}
			if err := txn.Delete(p.Key); err != nil {
				_ = txn.Rollback()
				return deleted, err
			}
			deleted++
			anyDeleted = true
		}
		if anyDeleted {
			if err := txn.Commit(ctx); err != nil {
				return deleted, err
			}
		} else {
			_ = txn.Rollback()
		}
		if len(pairs) < batch {
			return deleted, nil
		}
		// Advance cursor past the last scanned key (append 0x00 — the
		// next-key successor — so the next batch picks up strictly later
		// rows).
		last := pairs[len(pairs)-1].Key
		cursor = append(append([]byte(nil), last...), 0x00)
	}
}
