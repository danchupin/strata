package cassandra

import (
	"context"
	"errors"
	"time"

	"github.com/gocql/gocql"

	"github.com/danchupin/strata/internal/auth"
)

// CredentialStore looks up access keys in the Cassandra access_keys table and
// implements auth.CredentialsStore. Disabled rows are reported as missing so
// the gateway treats key revocation the same as deletion.
type CredentialStore struct {
	s *gocql.Session
}

func NewCredentialStore(s *gocql.Session) *CredentialStore {
	return &CredentialStore{s: s}
}

func (c *CredentialStore) Lookup(ctx context.Context, accessKey string) (*auth.Credential, error) {
	var (
		secret   string
		owner    string
		disabled bool
	)
	err := c.s.Query(
		`SELECT secret_key, owner, disabled FROM access_keys WHERE access_key = ?`,
		accessKey,
	).WithContext(ctx).Scan(&secret, &owner, &disabled)
	if err != nil {
		if errors.Is(err, gocql.ErrNotFound) {
			return nil, auth.ErrNoSuchCredential
		}
		return nil, err
	}
	if disabled {
		return nil, auth.ErrNoSuchCredential
	}
	return &auth.Credential{
		AccessKey: accessKey,
		Secret:    secret,
		Owner:     owner,
	}, nil
}

// Put inserts or replaces a credential row. Used by IAM admin endpoints
// (US-005/US-007) and by tests; not part of auth.CredentialsStore.
func (c *CredentialStore) Put(ctx context.Context, cred *auth.Credential, disabled bool) error {
	return c.s.Query(
		`INSERT INTO access_keys (access_key, secret_key, owner, disabled, created_at) VALUES (?, ?, ?, ?, ?)`,
		cred.AccessKey, cred.Secret, cred.Owner, disabled, time.Now().UTC(),
	).WithContext(ctx).Exec()
}

// Delete removes a credential row. Caller is responsible for invalidating any
// MultiStore cache entry tied to the access key.
func (c *CredentialStore) Delete(ctx context.Context, accessKey string) error {
	return c.s.Query(
		`DELETE FROM access_keys WHERE access_key = ?`,
		accessKey,
	).WithContext(ctx).Exec()
}
