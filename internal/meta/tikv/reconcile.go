// Reconcile job queue on TiKV (US-002 metadata-data-reconcile).
//
// Mirrors the admin_jobs.go shape (full-struct JSON body keyed on the
// server-minted UUID id) rather than reshard.go's key-elides-id shape — a
// reconcile job is a standalone queued work item, not a per-bucket singleton.
//
//   - StartReconcile validates the policy, mints an id, and Sets the row.
//   - GetReconcileJob is a single optimistic Get over ReconcileJobKey(id).
//   - UpdateReconcileJob is a pessimistic read-modify-write so concurrent
//     worker batches serialise on the per-job row (the Cursor + counter
//     watermark must not lose an update under a leader handover).
//   - ListReconcileJobs is one ordered range scan over the global prefix,
//     filtered to queued|running (the worker's drain set).
package tikv

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// StartReconcile queues a data-tier reconcile pass. A plain Set is safe
// here: the id is a freshly minted UUID, so there is no concurrent
// create-create race on the same key (unlike per-bucket reshard).
func (s *Store) StartReconcile(ctx context.Context, cluster, pool, namespace, bucket, policy string) (_ *meta.ReconcileJob, err error) {
	ctx, finish := s.observer.Start(ctx, "StartReconcile", "reconcile_jobs")
	defer func() { finish(err) }()
	if bucket == "" {
		if !meta.IsValidReconcilePolicy(policy) {
			return nil, meta.ErrReconcileInvalidPolicy
		}
	} else if !meta.IsValidDanglingPolicy(policy) {
		return nil, meta.ErrReconcileInvalidPolicy
	}
	now := time.Now().UTC()
	job := &meta.ReconcileJob{
		ID:        uuid.NewString(),
		Cluster:   cluster,
		Pool:      pool,
		Namespace: namespace,
		Bucket:    bucket,
		Policy:    policy,
		State:     meta.ReconcileStateQueued,
		CreatedAt: now,
		UpdatedAt: now,
	}
	payload, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}
	txn, err := s.beginPessimistic(ctx)
	if err != nil {
		return nil, err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.Set(ReconcileJobKey(job.ID), payload); err != nil {
		return nil, err
	}
	if err = txn.Commit(ctx); err != nil {
		return nil, err
	}
	out := *job
	return &out, nil
}

// GetReconcileJob is an optimistic Get on the per-id key. ErrReconcileNotFound
// when no row exists.
func (s *Store) GetReconcileJob(ctx context.Context, id string) (job *meta.ReconcileJob, err error) {
	ctx, finish := s.observer.Start(ctx, "GetReconcileJob", "reconcile_jobs")
	defer func() { finish(err) }()
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	raw, found, err := txn.Get(ctx, ReconcileJobKey(id))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, meta.ErrReconcileNotFound
	}
	var out meta.ReconcileJob
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateReconcileJob overwrites the mutable columns under a pessimistic txn so
// concurrent updates serialise on the row.
func (s *Store) UpdateReconcileJob(ctx context.Context, job *meta.ReconcileJob) (err error) {
	ctx, finish := s.observer.Start(ctx, "UpdateReconcileJob", "reconcile_jobs")
	defer func() { finish(err) }()
	if job == nil || job.ID == "" {
		return meta.ErrReconcileNotFound
	}
	key := ReconcileJobKey(job.ID)
	txn, err := s.beginPessimistic(ctx)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, key); err != nil {
		return err
	}
	if _, found, gerr := txn.Get(ctx, key); gerr != nil {
		err = gerr
		return err
	} else if !found {
		// Release the lease before the non-error early return so the
		// in-process memBackend used by tests does not deadlock the next
		// caller (gotcha in CLAUDE.md).
		_ = txn.Rollback()
		return meta.ErrReconcileNotFound
	}
	cp := *job
	cp.UpdatedAt = time.Now().UTC()
	payload, err := json.Marshal(&cp)
	if err != nil {
		return err
	}
	if err = txn.Set(key, payload); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// ListReconcileJobs is a single ordered range scan over the global reconcile
// prefix, filtered to queued|running. done/error rows persist for status
// polling and are skipped here.
func (s *Store) ListReconcileJobs(ctx context.Context) (out []*meta.ReconcileJob, err error) {
	ctx, finish := s.observer.Start(ctx, "ListReconcileJobs", "reconcile_jobs")
	defer func() { finish(err) }()
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	prefix := ReconcileJobsPrefix()
	pairs, err := txn.Scan(ctx, prefix, prefixEnd(prefix), 0)
	if err != nil {
		return nil, err
	}
	out = make([]*meta.ReconcileJob, 0, len(pairs))
	for _, p := range pairs {
		var job meta.ReconcileJob
		if derr := json.Unmarshal(p.Value, &job); derr != nil {
			return nil, derr
		}
		if job.State != meta.ReconcileStateQueued && job.State != meta.ReconcileStateRunning {
			continue
		}
		jb := job
		out = append(out, &jb)
	}
	return out, nil
}
