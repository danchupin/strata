// IAM managed policies + user-policy attachments on TiKV (US-010).
//
// Managed policies live at ManagedPolicyKey(arn). User-policy attachments
// carry two rows in lockstep — the per-user index row at UserPolicyKey
// (range-scanned by ListUserPolicies) and the inverse-index row at
// PolicyUserKey (probed by DeleteManagedPolicy to detect attachments without
// a global scan). Both rows are written/deleted in one pessimistic txn so
// the inverse view never desyncs.
package tikv

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/danchupin/strata/internal/meta"
)

// managedPolicyRow is the persisted shape for one managed policy. JSON keys
// are short to keep TiKV row payloads tight.
type managedPolicyRow struct {
	Arn         string    `json:"a"`
	Name        string    `json:"n"`
	Path        string    `json:"p,omitempty"`
	Description string    `json:"d,omitempty"`
	Document    []byte    `json:"doc,omitempty"`
	CreatedAt   time.Time `json:"c"`
	UpdatedAt   time.Time `json:"u"`
}

func encodeManagedPolicy(p *meta.ManagedPolicy) ([]byte, error) {
	return json.Marshal(&managedPolicyRow{
		Arn:         p.Arn,
		Name:        p.Name,
		Path:        p.Path,
		Description: p.Description,
		Document:    p.Document,
		CreatedAt:   p.CreatedAt,
		UpdatedAt:   p.UpdatedAt,
	})
}

func decodeManagedPolicy(raw []byte) (*meta.ManagedPolicy, error) {
	var row managedPolicyRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	return &meta.ManagedPolicy{
		Arn:         row.Arn,
		Name:        row.Name,
		Path:        row.Path,
		Description: row.Description,
		Document:    row.Document,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}, nil
}

// CreateManagedPolicy writes the per-policy record. Pessimistic txn
// (LockKeys + Get + Set) so a duplicate Arn surfaces
// ErrManagedPolicyAlreadyExists deterministically.
func (s *Store) CreateManagedPolicy(ctx context.Context, p *meta.ManagedPolicy) (err error) {
	if p == nil || p.Arn == "" {
		return meta.ErrManagedPolicyNotFound
	}
	row := *p
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = row.CreatedAt
	}
	payload, err := encodeManagedPolicy(&row)
	if err != nil {
		return err
	}
	key := ManagedPolicyKey(p.Arn)
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
		_ = txn.Rollback()
		return meta.ErrManagedPolicyAlreadyExists
	}
	if err = txn.Set(key, payload); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// GetManagedPolicy is a single optimistic Get against ManagedPolicyKey.
func (s *Store) GetManagedPolicy(ctx context.Context, arn string) (*meta.ManagedPolicy, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	raw, found, err := txn.Get(ctx, ManagedPolicyKey(arn))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, meta.ErrManagedPolicyNotFound
	}
	return decodeManagedPolicy(raw)
}

// ListManagedPolicies range-scans the global ManagedPolicy prefix; pathPrefix
// filter applied in-process — operator-scope cardinality is small.
func (s *Store) ListManagedPolicies(ctx context.Context, pathPrefix string) ([]*meta.ManagedPolicy, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	pairs, err := txn.Scan(ctx, ManagedPolicyPrefix(), prefixEnd(ManagedPolicyPrefix()), 0)
	if err != nil {
		return nil, err
	}
	out := make([]*meta.ManagedPolicy, 0, len(pairs))
	for _, kv := range pairs {
		p, err := decodeManagedPolicy(kv.Value)
		if err != nil {
			return nil, err
		}
		if pathPrefix != "" && !strings.HasPrefix(p.Path, pathPrefix) {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Arn < out[j].Arn })
	return out, nil
}

// UpdateManagedPolicyDocument overwrites the Document blob and bumps
// UpdatedAt under a pessimistic txn so concurrent rotates serialise.
func (s *Store) UpdateManagedPolicyDocument(ctx context.Context, arn string, document []byte, updatedAt time.Time) (err error) {
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	key := ManagedPolicyKey(arn)
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
		return meta.ErrManagedPolicyNotFound
	}
	cur, err := decodeManagedPolicy(raw)
	if err != nil {
		return err
	}
	cur.Document = append([]byte(nil), document...)
	cur.UpdatedAt = updatedAt
	payload, err := encodeManagedPolicy(cur)
	if err != nil {
		return err
	}
	if err = txn.Set(key, payload); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// DeleteManagedPolicy refuses to delete a row referenced from the inverse
// PolicyUser index. The inverse-index probe is a single-row scan (limit 1)
// instead of a global ALLOW FILTERING-equivalent.
func (s *Store) DeleteManagedPolicy(ctx context.Context, arn string) (err error) {
	key := ManagedPolicyKey(arn)
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
		return meta.ErrManagedPolicyNotFound
	}
	idxStart := PolicyUserPrefix(arn)
	pairs, err := txn.Scan(ctx, idxStart, prefixEnd(idxStart), 1)
	if err != nil {
		return err
	}
	if len(pairs) > 0 {
		_ = txn.Rollback()
		return meta.ErrPolicyAttached
	}
	if err = txn.Delete(key); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// AttachUserPolicy writes the per-user row + inverse-index row in one
// pessimistic txn so the two views never desync. ErrIAMUserNotFound /
// ErrManagedPolicyNotFound on missing referents,
// ErrUserPolicyAlreadyAttached on duplicate.
func (s *Store) AttachUserPolicy(ctx context.Context, userName, policyArn string) (err error) {
	userKey := IAMUserKey(userName)
	policyKey := ManagedPolicyKey(policyArn)
	upKey := UserPolicyKey(userName, policyArn)
	puKey := PolicyUserKey(policyArn, userName)
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, userKey, policyKey, upKey, puKey); err != nil {
		return err
	}
	if _, found, gerr := txn.Get(ctx, userKey); gerr != nil {
		err = gerr
		return err
	} else if !found {
		_ = txn.Rollback()
		return meta.ErrIAMUserNotFound
	}
	if _, found, gerr := txn.Get(ctx, policyKey); gerr != nil {
		err = gerr
		return err
	} else if !found {
		_ = txn.Rollback()
		return meta.ErrManagedPolicyNotFound
	}
	if _, found, gerr := txn.Get(ctx, upKey); gerr != nil {
		err = gerr
		return err
	} else if found {
		_ = txn.Rollback()
		return meta.ErrUserPolicyAlreadyAttached
	}
	attachedAt, err := time.Now().UTC().MarshalBinary()
	if err != nil {
		return err
	}
	if err = txn.Set(upKey, attachedAt); err != nil {
		return err
	}
	if err = txn.Set(puKey, attachedAt); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// DetachUserPolicy removes both rows in one pessimistic txn.
func (s *Store) DetachUserPolicy(ctx context.Context, userName, policyArn string) (err error) {
	upKey := UserPolicyKey(userName, policyArn)
	puKey := PolicyUserKey(policyArn, userName)
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, upKey, puKey); err != nil {
		return err
	}
	_, found, err := txn.Get(ctx, upKey)
	if err != nil {
		return err
	}
	if !found {
		_ = txn.Rollback()
		return meta.ErrUserPolicyNotAttached
	}
	if err = txn.Delete(upKey); err != nil {
		return err
	}
	if err = txn.Delete(puKey); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// ListUserPolicies range-scans the per-user attachment prefix in lex order
// (= policyArn ascending). ErrIAMUserNotFound when the user does not exist.
func (s *Store) ListUserPolicies(ctx context.Context, userName string) ([]string, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	if _, found, err := txn.Get(ctx, IAMUserKey(userName)); err != nil {
		return nil, err
	} else if !found {
		return nil, meta.ErrIAMUserNotFound
	}
	idxPrefix := UserPolicyPrefix(userName)
	pairs, err := txn.Scan(ctx, idxPrefix, prefixEnd(idxPrefix), 0)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		body := p.Key[len(idxPrefix):]
		policyArn, _, err := readEscaped(body)
		if err != nil {
			return nil, err
		}
		out = append(out, policyArn)
	}
	sort.Strings(out)
	return out, nil
}
