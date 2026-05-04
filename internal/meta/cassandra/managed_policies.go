package cassandra

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gocql/gocql"

	"github.com/danchupin/strata/internal/meta"
)

// CreateManagedPolicy persists a fresh row in iam_managed_policies. Uses an
// LWT-style INSERT IF NOT EXISTS so a duplicate Arn surfaces
// ErrManagedPolicyAlreadyExists deterministically.
func (s *Store) CreateManagedPolicy(ctx context.Context, p *meta.ManagedPolicy) error {
	if p == nil || p.Arn == "" {
		return meta.ErrManagedPolicyNotFound
	}
	created := p.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	updated := p.UpdatedAt
	if updated.IsZero() {
		updated = created
	}
	applied, err := s.s.Query(
		`INSERT INTO iam_managed_policies (arn, name, path, description, document, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?) IF NOT EXISTS`,
		p.Arn, p.Name, p.Path, p.Description, p.Document, created, updated,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).MapScanCAS(map[string]any{})
	if err != nil {
		return err
	}
	if !applied {
		return meta.ErrManagedPolicyAlreadyExists
	}
	return nil
}

// GetManagedPolicy returns the row addressed by arn or ErrManagedPolicyNotFound.
func (s *Store) GetManagedPolicy(ctx context.Context, arn string) (*meta.ManagedPolicy, error) {
	var (
		name, path, description string
		document                []byte
		createdAt, updatedAt    time.Time
	)
	err := s.s.Query(
		`SELECT name, path, description, document, created_at, updated_at FROM iam_managed_policies WHERE arn=?`,
		arn,
	).WithContext(ctx).Scan(&name, &path, &description, &document, &createdAt, &updatedAt)
	if errors.Is(err, gocql.ErrNotFound) {
		return nil, meta.ErrManagedPolicyNotFound
	}
	if err != nil {
		return nil, err
	}
	return &meta.ManagedPolicy{
		Arn:         arn,
		Name:        name,
		Path:        path,
		Description: description,
		Document:    document,
		CreatedAt:   createdAt,
		UpdatedAt:   updatedAt,
	}, nil
}

// ListManagedPolicies scans the whole table; cardinality is small (operator-
// scope, dozens at most) so a single full scan + in-process path filter beats
// secondary indexes.
func (s *Store) ListManagedPolicies(ctx context.Context, pathPrefix string) ([]*meta.ManagedPolicy, error) {
	iter := s.s.Query(`SELECT arn, name, path, description, document, created_at, updated_at FROM iam_managed_policies`).
		WithContext(ctx).Iter()
	defer iter.Close()
	var (
		out                                []*meta.ManagedPolicy
		arn, name, pathStr, description    string
		document                           []byte
		createdAt, updatedAt               time.Time
	)
	for iter.Scan(&arn, &name, &pathStr, &description, &document, &createdAt, &updatedAt) {
		if pathPrefix != "" && !strings.HasPrefix(pathStr, pathPrefix) {
			continue
		}
		doc := append([]byte(nil), document...)
		out = append(out, &meta.ManagedPolicy{
			Arn:         arn,
			Name:        name,
			Path:        pathStr,
			Description: description,
			Document:    doc,
			CreatedAt:   createdAt,
			UpdatedAt:   updatedAt,
		})
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateManagedPolicyDocument overwrites the Document column and bumps
// UpdatedAt under an LWT so concurrent rotates serialise.
func (s *Store) UpdateManagedPolicyDocument(ctx context.Context, arn string, document []byte, updatedAt time.Time) error {
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	applied, err := s.s.Query(
		`UPDATE iam_managed_policies SET document=?, updated_at=? WHERE arn=? IF EXISTS`,
		document, updatedAt, arn,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(nil)
	if err != nil {
		return err
	}
	if !applied {
		return meta.ErrManagedPolicyNotFound
	}
	return nil
}

// DeleteManagedPolicy refuses to delete a row referenced from
// iam_policy_attachments. The inverse-index table makes the attachment check
// a single partition read instead of a full-scan ALLOW FILTERING.
func (s *Store) DeleteManagedPolicy(ctx context.Context, arn string) error {
	if _, err := s.GetManagedPolicy(ctx, arn); err != nil {
		return err
	}
	var probe string
	err := s.s.Query(
		`SELECT user_name FROM iam_policy_attachments WHERE policy_arn=? LIMIT 1`,
		arn,
	).WithContext(ctx).Scan(&probe)
	if err != nil && !errors.Is(err, gocql.ErrNotFound) {
		return err
	}
	if err == nil {
		return meta.ErrPolicyAttached
	}
	applied, err := s.s.Query(
		`DELETE FROM iam_managed_policies WHERE arn=? IF EXISTS`,
		arn,
	).WithContext(ctx).SerialConsistency(gocql.LocalSerial).ScanCAS(nil)
	if err != nil {
		return err
	}
	if !applied {
		return meta.ErrManagedPolicyNotFound
	}
	return nil
}

// AttachUserPolicy writes both the per-user row and the inverse-index row in
// one logged batch so the two views can never desync.
func (s *Store) AttachUserPolicy(ctx context.Context, userName, policyArn string) error {
	if _, err := s.GetIAMUser(ctx, userName); err != nil {
		return err
	}
	if _, err := s.GetManagedPolicy(ctx, policyArn); err != nil {
		return err
	}
	var probeUser string
	err := s.s.Query(
		`SELECT user_name FROM iam_user_policies WHERE user_name=? AND policy_arn=?`,
		userName, policyArn,
	).WithContext(ctx).Scan(&probeUser)
	if err != nil && !errors.Is(err, gocql.ErrNotFound) {
		return err
	}
	if err == nil {
		return meta.ErrUserPolicyAlreadyAttached
	}
	attachedAt := time.Now().UTC()
	batch := s.s.NewBatch(gocql.LoggedBatch).WithContext(ctx)
	batch.Query(
		`INSERT INTO iam_user_policies (user_name, policy_arn, attached_at) VALUES (?, ?, ?)`,
		userName, policyArn, attachedAt,
	)
	batch.Query(
		`INSERT INTO iam_policy_attachments (policy_arn, user_name, attached_at) VALUES (?, ?, ?)`,
		policyArn, userName, attachedAt,
	)
	return s.s.ExecuteBatch(batch)
}

// DetachUserPolicy removes both rows in a logged batch.
func (s *Store) DetachUserPolicy(ctx context.Context, userName, policyArn string) error {
	var probeUser string
	err := s.s.Query(
		`SELECT user_name FROM iam_user_policies WHERE user_name=? AND policy_arn=?`,
		userName, policyArn,
	).WithContext(ctx).Scan(&probeUser)
	if errors.Is(err, gocql.ErrNotFound) {
		return meta.ErrUserPolicyNotAttached
	}
	if err != nil {
		return err
	}
	batch := s.s.NewBatch(gocql.LoggedBatch).WithContext(ctx)
	batch.Query(
		`DELETE FROM iam_user_policies WHERE user_name=? AND policy_arn=?`,
		userName, policyArn,
	)
	batch.Query(
		`DELETE FROM iam_policy_attachments WHERE policy_arn=? AND user_name=?`,
		policyArn, userName,
	)
	return s.s.ExecuteBatch(batch)
}

// ListUserPolicies returns every attached policy ARN for userName, sorted by
// the table's clustering order on policy_arn (lexicographic ascending).
func (s *Store) ListUserPolicies(ctx context.Context, userName string) ([]string, error) {
	if _, err := s.GetIAMUser(ctx, userName); err != nil {
		return nil, err
	}
	iter := s.s.Query(
		`SELECT policy_arn FROM iam_user_policies WHERE user_name=?`,
		userName,
	).WithContext(ctx).Iter()
	defer iter.Close()
	var (
		out []string
		arn string
	)
	for iter.Scan(&arn) {
		out = append(out, arn)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// ListPolicyUsers returns every user_name attached to policyArn, scanned from
// the inverse-index partition iam_policy_attachments (PK policy_arn,
// user_name) in clustering order. ErrManagedPolicyNotFound when the policy
// itself does not exist.
func (s *Store) ListPolicyUsers(ctx context.Context, policyArn string) ([]string, error) {
	if _, err := s.GetManagedPolicy(ctx, policyArn); err != nil {
		return nil, err
	}
	iter := s.s.Query(
		`SELECT user_name FROM iam_policy_attachments WHERE policy_arn=?`,
		policyArn,
	).WithContext(ctx).Iter()
	defer iter.Close()
	var (
		out  []string
		user string
	)
	for iter.Scan(&user) {
		out = append(out, user)
	}
	if err := iter.Close(); err != nil {
		return nil, err
	}
	return out, nil
}
