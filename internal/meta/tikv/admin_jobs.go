package tikv

import (
	"context"
	"encoding/json"

	"github.com/danchupin/strata/internal/meta"
)

// CreateAdminJob persists an admin job row at AdminJobKey(job.ID). Uses a
// pessimistic txn + LockKeys + Get on the row so a concurrent CreateAdminJob
// for the same ID surfaces ErrAdminJobAlreadyExists deterministically rather
// than racing with a plain Put.
func (s *Store) CreateAdminJob(ctx context.Context, job *meta.AdminJob) (err error) {
	if job == nil || job.ID == "" {
		return meta.ErrAdminJobNotFound
	}
	key := AdminJobKey(job.ID)
	txn, err := s.kv.Begin(ctx, true)
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
	} else if found {
		// Release the lease before returning a non-error early-result so
		// the in-process memBackend used by tests does not deadlock the
		// next caller (gotcha noted in CLAUDE.md).
		_ = txn.Rollback()
		return meta.ErrAdminJobAlreadyExists
	}
	payload, err := json.Marshal(job)
	if err != nil {
		return err
	}
	if err = txn.Set(key, payload); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// GetAdminJob is an optimistic Get on the per-id key. ErrAdminJobNotFound
// when no row exists.
func (s *Store) GetAdminJob(ctx context.Context, id string) (*meta.AdminJob, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	raw, found, err := txn.Get(ctx, AdminJobKey(id))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, meta.ErrAdminJobNotFound
	}
	var out meta.AdminJob
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateAdminJob overwrites the mutable columns under a pessimistic txn so
// concurrent updates serialise on the row.
func (s *Store) UpdateAdminJob(ctx context.Context, job *meta.AdminJob) (err error) {
	if job == nil || job.ID == "" {
		return meta.ErrAdminJobNotFound
	}
	key := AdminJobKey(job.ID)
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, key); err != nil {
		return err
	}
	raw, found, err := txn.Get(ctx, key)
	if err != nil {
		return err
	}
	if !found {
		return meta.ErrAdminJobNotFound
	}
	var cur meta.AdminJob
	if err = json.Unmarshal(raw, &cur); err != nil {
		return err
	}
	cur.State = job.State
	cur.Message = job.Message
	cur.Deleted = job.Deleted
	cur.UpdatedAt = job.UpdatedAt
	cur.FinishedAt = job.FinishedAt
	payload, err := json.Marshal(&cur)
	if err != nil {
		return err
	}
	if err = txn.Set(key, payload); err != nil {
		return err
	}
	return txn.Commit(ctx)
}
