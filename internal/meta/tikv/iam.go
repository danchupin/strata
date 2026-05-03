// IAM users + access keys on TiKV (US-008).
//
// Users live under prefixIAMUser, addressed by userName (Cassandra LWT
// equivalent: pessimistic txn read-then-Set in CreateIAMUser).
//
// Access keys carry two rows in lockstep — the per-key record under
// prefixIAMAccessKey (read on every SigV4 verification, so it is a single
// optimistic Get) and the per-user index row under prefixIAMUserKeyIndex
// (range-scanned by ListIAMAccessKeys). Both rows are written/deleted in
// one pessimistic txn so the index can never desync from the record.
package tikv

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

// iamUserRow is the persisted shape for one IAM user. JSON keys are
// short to keep TiKV row payloads tight; the encoding is internal so the
// abbreviations are safe.
type iamUserRow struct {
	UserName  string    `json:"un"`
	UserID    string    `json:"ui"`
	Path      string    `json:"p,omitempty"`
	CreatedAt time.Time `json:"c"`
}

func encodeIAMUser(u *meta.IAMUser) ([]byte, error) {
	return json.Marshal(&iamUserRow{
		UserName:  u.UserName,
		UserID:    u.UserID,
		Path:      u.Path,
		CreatedAt: u.CreatedAt,
	})
}

func decodeIAMUser(raw []byte) (*meta.IAMUser, error) {
	var row iamUserRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	return &meta.IAMUser{
		UserName:  row.UserName,
		UserID:    row.UserID,
		Path:      row.Path,
		CreatedAt: row.CreatedAt,
	}, nil
}

// iamAccessKeyRow is the persisted shape for one access-key record.
type iamAccessKeyRow struct {
	AccessKeyID     string    `json:"ak"`
	SecretAccessKey string    `json:"sk"`
	UserName        string    `json:"un"`
	CreatedAt       time.Time `json:"c"`
	Disabled        bool      `json:"d,omitempty"`
}

func encodeIAMAccessKey(ak *meta.IAMAccessKey) ([]byte, error) {
	return json.Marshal(&iamAccessKeyRow{
		AccessKeyID:     ak.AccessKeyID,
		SecretAccessKey: ak.SecretAccessKey,
		UserName:        ak.UserName,
		CreatedAt:       ak.CreatedAt,
		Disabled:        ak.Disabled,
	})
}

func decodeIAMAccessKey(raw []byte) (*meta.IAMAccessKey, error) {
	var row iamAccessKeyRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	return &meta.IAMAccessKey{
		AccessKeyID:     row.AccessKeyID,
		SecretAccessKey: row.SecretAccessKey,
		UserName:        row.UserName,
		CreatedAt:       row.CreatedAt,
		Disabled:        row.Disabled,
	}, nil
}

// CreateIAMUser writes the per-user record under IAMUserKey, conflict on
// duplicate user name yields ErrIAMUserAlreadyExists. Pessimistic txn
// (LockKeys + Get + Set) mirrors CreateBucket's LWT-equivalent shape.
func (s *Store) CreateIAMUser(ctx context.Context, u *meta.IAMUser) (err error) {
	row := *u
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	payload, err := encodeIAMUser(&row)
	if err != nil {
		return err
	}
	key := IAMUserKey(u.UserName)
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, key); err != nil {
		return err
	}
	_, found, err := txn.Get(ctx, key)
	if err != nil {
		return err
	}
	if found {
		return meta.ErrIAMUserAlreadyExists
	}
	if err = txn.Set(key, payload); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// GetIAMUser is a single optimistic Get against IAMUserKey.
func (s *Store) GetIAMUser(ctx context.Context, userName string) (*meta.IAMUser, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	raw, found, err := txn.Get(ctx, IAMUserKey(userName))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, meta.ErrIAMUserNotFound
	}
	return decodeIAMUser(raw)
}

// ListIAMUsers range-scans the IAMUser prefix in lex (= userName ascending)
// order. Path-prefix filter applied in-process — IAM user cardinality is
// small enough that scan + filter is fine.
func (s *Store) ListIAMUsers(ctx context.Context, pathPrefix string) ([]*meta.IAMUser, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	start := IAMUserPrefix()
	pairs, err := txn.Scan(ctx, start, prefixEnd(start), 0)
	if err != nil {
		return nil, err
	}
	out := make([]*meta.IAMUser, 0, len(pairs))
	for _, p := range pairs {
		u, err := decodeIAMUser(p.Value)
		if err != nil {
			return nil, err
		}
		if pathPrefix != "" && !strings.HasPrefix(u.Path, pathPrefix) {
			continue
		}
		out = append(out, u)
	}
	return out, nil
}

// DeleteIAMUser removes the per-user record. Returns ErrIAMUserNotFound
// when the user does not exist. Pessimistic txn read-then-delete mirrors
// Cassandra's IF EXISTS shape.
func (s *Store) DeleteIAMUser(ctx context.Context, userName string) (err error) {
	key := IAMUserKey(userName)
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, key); err != nil {
		return err
	}
	_, found, err := txn.Get(ctx, key)
	if err != nil {
		return err
	}
	if !found {
		return meta.ErrIAMUserNotFound
	}
	if err = txn.Delete(key); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// CreateIAMAccessKey writes both the per-key record and the per-user
// index row in one pessimistic txn — mirrors Cassandra's LoggedBatch
// atomicity. Access-key IDs are gateway-minted opaque tokens (collision
// is not a realistic concern), so we do not raise an "already exists"
// error: the call is last-writer-wins, matching Cassandra's plain INSERT.
func (s *Store) CreateIAMAccessKey(ctx context.Context, ak *meta.IAMAccessKey) (err error) {
	row := *ak
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	payload, err := encodeIAMAccessKey(&row)
	if err != nil {
		return err
	}
	akKey := IAMAccessKeyKey(ak.AccessKeyID)
	idxKey := IAMUserAccessKeyKey(ak.UserName, ak.AccessKeyID)
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, akKey, idxKey); err != nil {
		return err
	}
	if err = txn.Set(akKey, payload); err != nil {
		return err
	}
	if err = txn.Set(idxKey, nil); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// GetIAMAccessKey is the SigV4 hot path: a single optimistic Get against
// IAMAccessKeyKey, no scan.
func (s *Store) GetIAMAccessKey(ctx context.Context, accessKeyID string) (*meta.IAMAccessKey, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	raw, found, err := txn.Get(ctx, IAMAccessKeyKey(accessKeyID))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, meta.ErrIAMAccessKeyNotFound
	}
	return decodeIAMAccessKey(raw)
}

// ListIAMAccessKeys range-scans the per-user secondary index, then Gets
// each row by access-key ID. Empty userName means "no user filter" —
// mirrors the memory backend by scanning the per-key prefix directly.
func (s *Store) ListIAMAccessKeys(ctx context.Context, userName string) ([]*meta.IAMAccessKey, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()

	if userName == "" {
		start := []byte(prefixIAMAccessKey)
		pairs, err := txn.Scan(ctx, start, prefixEnd(start), 0)
		if err != nil {
			return nil, err
		}
		out := make([]*meta.IAMAccessKey, 0, len(pairs))
		for _, p := range pairs {
			ak, err := decodeIAMAccessKey(p.Value)
			if err != nil {
				return nil, err
			}
			out = append(out, ak)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].AccessKeyID < out[j].AccessKeyID })
		return out, nil
	}

	idxPrefix := IAMUserAccessKeyPrefix(userName)
	pairs, err := txn.Scan(ctx, idxPrefix, prefixEnd(idxPrefix), 0)
	if err != nil {
		return nil, err
	}
	out := make([]*meta.IAMAccessKey, 0, len(pairs))
	for _, p := range pairs {
		// Index key is prefixIAMUserKeyIndex + escaped(userName) +
		// escaped(accessKeyID); strip the per-user prefix and decode the
		// remaining segment.
		body := p.Key[len(idxPrefix):]
		accessKeyID, _, err := readEscaped(body)
		if err != nil {
			return nil, err
		}
		raw, found, err := txn.Get(ctx, IAMAccessKeyKey(accessKeyID))
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		ak, err := decodeIAMAccessKey(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, ak)
	}
	return out, nil
}

// UpdateIAMAccessKeyDisabled flips the Disabled bit on the per-key record
// in a pessimistic txn (LockKeys + Get + Set). Returns the post-flip row.
// Returns ErrIAMAccessKeyNotFound when no row exists; the early-return path
// calls txn.Rollback() explicitly so the in-process memBackend used in unit
// tests does not leak the LockKeys lease.
func (s *Store) UpdateIAMAccessKeyDisabled(ctx context.Context, accessKeyID string, disabled bool) (ak *meta.IAMAccessKey, err error) {
	akKey := IAMAccessKeyKey(accessKeyID)
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return nil, err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, akKey); err != nil {
		return nil, err
	}
	raw, found, err := txn.Get(ctx, akKey)
	if err != nil {
		return nil, err
	}
	if !found {
		_ = txn.Rollback()
		return nil, meta.ErrIAMAccessKeyNotFound
	}
	row, err := decodeIAMAccessKey(raw)
	if err != nil {
		return nil, err
	}
	row.Disabled = disabled
	payload, err := encodeIAMAccessKey(row)
	if err != nil {
		return nil, err
	}
	if err = txn.Set(akKey, payload); err != nil {
		return nil, err
	}
	if err = txn.Commit(ctx); err != nil {
		return nil, err
	}
	return row, nil
}

// DeleteIAMAccessKey reads the row to learn UserName (so the secondary
// index can be cleaned), then deletes both rows in one pessimistic txn.
// Returns the deleted record so callers can audit-log the removed
// identity (mirrors Cassandra shape).
func (s *Store) DeleteIAMAccessKey(ctx context.Context, accessKeyID string) (ak *meta.IAMAccessKey, err error) {
	akKey := IAMAccessKeyKey(accessKeyID)
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return nil, err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, akKey); err != nil {
		return nil, err
	}
	raw, found, err := txn.Get(ctx, akKey)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, meta.ErrIAMAccessKeyNotFound
	}
	ak, err = decodeIAMAccessKey(raw)
	if err != nil {
		return nil, err
	}
	idxKey := IAMUserAccessKeyKey(ak.UserName, accessKeyID)
	if err = txn.LockKeys(ctx, idxKey); err != nil {
		return nil, err
	}
	if err = txn.Delete(akKey); err != nil {
		return nil, err
	}
	if err = txn.Delete(idxKey); err != nil {
		return nil, err
	}
	if err = txn.Commit(ctx); err != nil {
		return nil, err
	}
	return ak, nil
}
