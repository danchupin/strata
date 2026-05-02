package cassandra

import (
	"context"
	"errors"
	"time"

	"github.com/gocql/gocql"
	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

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
