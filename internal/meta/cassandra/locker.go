package cassandra

import (
	"context"
	"errors"
	"time"

	"github.com/gocql/gocql"
)

type Locker struct {
	S *gocql.Session
}

func (l *Locker) Acquire(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	var existingHolder string
	applied, err := l.S.Query(
		`INSERT INTO worker_locks (name, holder) VALUES (?, ?) IF NOT EXISTS USING TTL ?`,
		name, holder, int(ttl.Seconds()),
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(nil, &existingHolder)
	if err != nil {
		return false, err
	}
	return applied, nil
}

func (l *Locker) Renew(ctx context.Context, name, holder string, ttl time.Duration) (bool, error) {
	var currentHolder string
	applied, err := l.S.Query(
		`UPDATE worker_locks USING TTL ? SET holder=? WHERE name=? IF holder=?`,
		int(ttl.Seconds()), holder, name, holder,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(&currentHolder)
	if err != nil {
		if errors.Is(err, gocql.ErrNotFound) {
			// Row expired between us holding it and the renew attempt.
			return false, nil
		}
		return false, err
	}
	return applied, nil
}

func (l *Locker) Release(ctx context.Context, name, holder string) error {
	return l.S.Query(
		`DELETE FROM worker_locks WHERE name=? IF holder=?`,
		name, holder,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).Exec()
}
