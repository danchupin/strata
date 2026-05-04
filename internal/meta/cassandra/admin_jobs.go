package cassandra

import (
	"context"
	"errors"
	"time"

	"github.com/gocql/gocql"

	"github.com/danchupin/strata/internal/meta"
)

// CreateAdminJob inserts a new admin job row IF NOT EXISTS. Used by the
// /admin/v1/buckets/{bucket}/force-empty endpoint (US-002) to persist a
// long-running drain job. Returns ErrAdminJobAlreadyExists on collision so
// concurrent retries surface a deterministic error rather than racing two
// drain goroutines.
func (s *Store) CreateAdminJob(ctx context.Context, job *meta.AdminJob) error {
	if job == nil || job.ID == "" {
		return meta.ErrAdminJobNotFound
	}
	applied, err := s.s.Query(
		`INSERT INTO admin_jobs (id, kind, bucket, state, message, deleted, started_at, updated_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) IF NOT EXISTS`,
		job.ID, job.Kind, job.Bucket, job.State, job.Message, job.Deleted,
		job.StartedAt, job.UpdatedAt, job.FinishedAt,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).MapScanCAS(map[string]any{})
	if err != nil {
		return err
	}
	if !applied {
		return meta.ErrAdminJobAlreadyExists
	}
	return nil
}

// GetAdminJob returns the row addressed by id, or ErrAdminJobNotFound.
func (s *Store) GetAdminJob(ctx context.Context, id string) (*meta.AdminJob, error) {
	var (
		kind, bucket, state, message string
		deleted                      int64
		startedAt, updatedAt         time.Time
		finishedAt                   time.Time
	)
	err := s.s.Query(
		`SELECT kind, bucket, state, message, deleted, started_at, updated_at, finished_at
		 FROM admin_jobs WHERE id=?`,
		id,
	).WithContext(ctx).Scan(&kind, &bucket, &state, &message, &deleted, &startedAt, &updatedAt, &finishedAt)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrAdminJobNotFound
	}
	if err != nil {
		return nil, err
	}
	return &meta.AdminJob{
		ID:         id,
		Kind:       kind,
		Bucket:     bucket,
		State:      state,
		Message:    message,
		Deleted:    deleted,
		StartedAt:  startedAt,
		UpdatedAt:  updatedAt,
		FinishedAt: finishedAt,
	}, nil
}

// UpdateAdminJob overwrites the mutable columns. State/Message/Deleted
// flow forward; StartedAt/Kind/Bucket are immutable post-create.
func (s *Store) UpdateAdminJob(ctx context.Context, job *meta.AdminJob) error {
	if job == nil || job.ID == "" {
		return meta.ErrAdminJobNotFound
	}
	applied, err := s.s.Query(
		`UPDATE admin_jobs SET state=?, message=?, deleted=?, updated_at=?, finished_at=?
		 WHERE id=? IF EXISTS`,
		job.State, job.Message, job.Deleted, job.UpdatedAt, job.FinishedAt, job.ID,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(nil)
	if err != nil {
		return err
	}
	if !applied {
		return meta.ErrAdminJobNotFound
	}
	return nil
}
