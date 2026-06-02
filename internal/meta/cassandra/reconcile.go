package cassandra

import (
	"context"
	"time"

	"github.com/gocql/gocql"
	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// reconcileJobCols is the full column projection of reconcile_jobs (minus the
// id partition key), kept in lockstep with the INSERT in StartReconcile and
// the Scan order in scanReconcileJob / ListReconcileJobs.
const reconcileJobCols = `cluster, pool, namespace, policy, cursor, state, message,
	scanned, orphans_found, orphans_gc, orphans_report, absent_backref, errors, created_at, updated_at`

// StartReconcile inserts a fresh reconcile_jobs row. A plain INSERT (no LWT) is
// safe: the id is a freshly minted UUID so there is no create-create race on
// the key — unlike the per-bucket reshard singleton.
func (s *Store) StartReconcile(ctx context.Context, cluster, pool, namespace, policy string) (*meta.ReconcileJob, error) {
	if !meta.IsValidReconcilePolicy(policy) {
		return nil, meta.ErrReconcileInvalidPolicy
	}
	now := time.Now().UTC()
	job := &meta.ReconcileJob{
		ID:        uuid.NewString(),
		Cluster:   cluster,
		Pool:      pool,
		Namespace: namespace,
		Policy:    policy,
		State:     meta.ReconcileStateQueued,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.s.Query(
		`INSERT INTO reconcile_jobs (id, `+reconcileJobCols+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.ID, job.Cluster, job.Pool, job.Namespace, job.Policy, job.Cursor, job.State, job.Message,
		job.Scanned, job.OrphansFound, job.OrphansGC, job.OrphansReport, job.AbsentBackref, job.Errors,
		job.CreatedAt, job.UpdatedAt,
	).WithContext(ctx).Exec(); err != nil {
		return nil, err
	}
	return job, nil
}

func (s *Store) GetReconcileJob(ctx context.Context, id string) (*meta.ReconcileJob, error) {
	return s.scanReconcileJob(ctx, id)
}

// UpdateReconcileJob overwrites the mutable columns. Plain UPDATE IF EXISTS so
// a vanished row surfaces ErrReconcileNotFound; counters/cursor are not read at
// quorum across LWT boundaries (single-writer worker), so no LWT-on-LWT concern.
func (s *Store) UpdateReconcileJob(ctx context.Context, job *meta.ReconcileJob) error {
	if job == nil || job.ID == "" {
		return meta.ErrReconcileNotFound
	}
	now := time.Now().UTC()
	applied, err := s.s.Query(
		`UPDATE reconcile_jobs SET cursor=?, state=?, message=?, scanned=?, orphans_found=?,
		 orphans_gc=?, orphans_report=?, absent_backref=?, errors=?, updated_at=? WHERE id=? IF EXISTS`,
		job.Cursor, job.State, job.Message, job.Scanned, job.OrphansFound,
		job.OrphansGC, job.OrphansReport, job.AbsentBackref, job.Errors, now, job.ID,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(nil)
	if err != nil {
		return err
	}
	if !applied {
		return meta.ErrReconcileNotFound
	}
	return nil
}

// ListReconcileJobs returns only queued|running rows (the worker's drain set).
// The state filter is applied in-process — the table is tiny so the
// full-partition scan is cheap and avoids an ALLOW FILTERING secondary index.
func (s *Store) ListReconcileJobs(ctx context.Context) ([]*meta.ReconcileJob, error) {
	iter := s.s.Query(
		`SELECT id, ` + reconcileJobCols + ` FROM reconcile_jobs`,
	).WithContext(ctx).Iter()
	var out []*meta.ReconcileJob
	for {
		job, ok := scanReconcileRow(iter)
		if !ok {
			break
		}
		if job.State != meta.ReconcileStateQueued && job.State != meta.ReconcileStateRunning {
			continue
		}
		out = append(out, job)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) scanReconcileJob(ctx context.Context, id string) (*meta.ReconcileJob, error) {
	iter := s.s.Query(
		`SELECT id, `+reconcileJobCols+` FROM reconcile_jobs WHERE id=?`, id,
	).WithContext(ctx).Iter()
	job, ok := scanReconcileRow(iter)
	if err := iter.Close(); err != nil {
		return nil, err
	}
	if !ok {
		return nil, meta.ErrReconcileNotFound
	}
	return job, nil
}

// scanReconcileRow decodes one reconcile_jobs row from iter, returning ok=false
// when the iterator is drained. Errors surface via the caller's iter.Close().
func scanReconcileRow(iter *gocql.Iter) (*meta.ReconcileJob, bool) {
	var (
		id, cluster, pool, namespace, policy, cursor, state, message string
		scanned, orphansFound, orphansGC, orphansReport              int64
		absentBackref, errs                                          int64
		createdAt, updatedAt                                         time.Time
	)
	if !iter.Scan(&id, &cluster, &pool, &namespace, &policy, &cursor, &state, &message,
		&scanned, &orphansFound, &orphansGC, &orphansReport, &absentBackref, &errs, &createdAt, &updatedAt) {
		return nil, false
	}
	return &meta.ReconcileJob{
		ID:            id,
		Cluster:       cluster,
		Pool:          pool,
		Namespace:     namespace,
		Policy:        policy,
		Cursor:        cursor,
		State:         state,
		Message:       message,
		Scanned:       scanned,
		OrphansFound:  orphansFound,
		OrphansGC:     orphansGC,
		OrphansReport: orphansReport,
		AbsentBackref: absentBackref,
		Errors:        errs,
		CreatedAt:     createdAt,
		UpdatedAt:     updatedAt,
	}, true
}
