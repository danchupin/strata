package cassandra

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gocql/gocql"

	"github.com/danchupin/strata/internal/meta"
)

func (s *Store) CreateIAMUser(ctx context.Context, u *meta.IAMUser) error {
	created := u.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	applied, err := s.s.Query(
		`INSERT INTO iam_users (user_name, user_id, path, created_at)
		 VALUES (?, ?, ?, ?) IF NOT EXISTS`,
		u.UserName, u.UserID, u.Path, created,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(nil, nil, nil, nil)
	if err != nil {
		return err
	}
	if !applied {
		return meta.ErrIAMUserAlreadyExists
	}
	return nil
}

func (s *Store) GetIAMUser(ctx context.Context, userName string) (*meta.IAMUser, error) {
	var (
		userID    string
		path      string
		createdAt time.Time
	)
	err := s.s.Query(
		`SELECT user_id, path, created_at FROM iam_users WHERE user_name=?`,
		userName,
	).WithContext(ctx).Scan(&userID, &path, &createdAt)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrIAMUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &meta.IAMUser{
		UserName:  userName,
		UserID:    userID,
		Path:      path,
		CreatedAt: createdAt,
	}, nil
}

func (s *Store) ListIAMUsers(ctx context.Context, pathPrefix string) ([]*meta.IAMUser, error) {
	iter := s.s.Query(`SELECT user_name, user_id, path, created_at FROM iam_users`).
		WithContext(ctx).Iter()
	defer iter.Close()
	var (
		out                       []*meta.IAMUser
		userName, userID, pathStr string
		createdAt                 time.Time
	)
	for iter.Scan(&userName, &userID, &pathStr, &createdAt) {
		if pathPrefix != "" && !strings.HasPrefix(pathStr, pathPrefix) {
			continue
		}
		out = append(out, &meta.IAMUser{
			UserName:  userName,
			UserID:    userID,
			Path:      pathStr,
			CreatedAt: createdAt,
		})
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) DeleteIAMUser(ctx context.Context, userName string) error {
	applied, err := s.s.Query(
		`DELETE FROM iam_users WHERE user_name=? IF EXISTS`,
		userName,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(nil)
	if err != nil {
		return err
	}
	if !applied {
		return meta.ErrIAMUserNotFound
	}
	return nil
}
