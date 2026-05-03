package cassandra

import (
	"context"
	"errors"
	"time"

	"github.com/gocql/gocql"

	"github.com/danchupin/strata/internal/meta"
)

func (s *Store) CreateIAMAccessKey(ctx context.Context, ak *meta.IAMAccessKey) error {
	created := ak.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	batch := s.s.NewBatch(gocql.LoggedBatch).WithContext(ctx)
	batch.Query(
		`INSERT INTO access_keys (access_key, secret_key, owner, disabled, created_at, user_name)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		ak.AccessKeyID, ak.SecretAccessKey, ak.UserName, ak.Disabled, created, ak.UserName,
	)
	batch.Query(
		`INSERT INTO iam_access_keys_by_user (user_name, access_key) VALUES (?, ?)`,
		ak.UserName, ak.AccessKeyID,
	)
	return s.s.ExecuteBatch(batch)
}

func (s *Store) GetIAMAccessKey(ctx context.Context, accessKeyID string) (*meta.IAMAccessKey, error) {
	var (
		secret    string
		userName  string
		disabled  bool
		createdAt time.Time
	)
	err := s.s.Query(
		`SELECT secret_key, user_name, disabled, created_at FROM access_keys WHERE access_key=?`,
		accessKeyID,
	).WithContext(ctx).Scan(&secret, &userName, &disabled, &createdAt)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrIAMAccessKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return &meta.IAMAccessKey{
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secret,
		UserName:        userName,
		CreatedAt:       createdAt,
		Disabled:        disabled,
	}, nil
}

func (s *Store) ListIAMAccessKeys(ctx context.Context, userName string) ([]*meta.IAMAccessKey, error) {
	iter := s.s.Query(
		`SELECT access_key FROM iam_access_keys_by_user WHERE user_name=?`,
		userName,
	).WithContext(ctx).Iter()
	var (
		out         []*meta.IAMAccessKey
		accessKeyID string
	)
	for iter.Scan(&accessKeyID) {
		ak, err := s.GetIAMAccessKey(ctx, accessKeyID)
		if errors.Is(err, meta.ErrIAMAccessKeyNotFound) {
			continue
		}
		if err != nil {
			_ = iter.Close()
			return nil, err
		}
		out = append(out, ak)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateIAMAccessKeyDisabled flips the disabled column on the row addressed by
// accessKeyID. Uses an LWT (`UPDATE … IF EXISTS`) so any prior LWT-on-create
// path stays read-after-write coherent — same lesson as SetBucketVersioning.
// Returns the post-flip row.
func (s *Store) UpdateIAMAccessKeyDisabled(ctx context.Context, accessKeyID string, disabled bool) (*meta.IAMAccessKey, error) {
	applied, err := s.s.Query(
		`UPDATE access_keys SET disabled=? WHERE access_key=? IF EXISTS`,
		disabled, accessKeyID,
	).WithContext(ctx).ScanCAS()
	if err != nil {
		return nil, err
	}
	if !applied {
		return nil, meta.ErrIAMAccessKeyNotFound
	}
	return s.GetIAMAccessKey(ctx, accessKeyID)
}

func (s *Store) DeleteIAMAccessKey(ctx context.Context, accessKeyID string) (*meta.IAMAccessKey, error) {
	ak, err := s.GetIAMAccessKey(ctx, accessKeyID)
	if err != nil {
		return nil, err
	}
	batch := s.s.NewBatch(gocql.LoggedBatch).WithContext(ctx)
	batch.Query(
		`DELETE FROM access_keys WHERE access_key=?`,
		accessKeyID,
	)
	batch.Query(
		`DELETE FROM iam_access_keys_by_user WHERE user_name=? AND access_key=?`,
		ak.UserName, accessKeyID,
	)
	if err := s.s.ExecuteBatch(batch); err != nil {
		return nil, err
	}
	return ak, nil
}
