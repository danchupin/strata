package cassandra

import (
	"context"
	"errors"

	"github.com/gocql/gocql"

	"github.com/danchupin/strata/internal/auth"
)

// CredentialStore looks up access keys in the Cassandra access_keys table and
// implements auth.CredentialsStore. Disabled rows are reported as missing so
// the gateway treats key revocation the same as deletion. Writes go through
// meta.Store.CreateIAMAccessKey/DeleteIAMAccessKey so the per-user index table
// stays consistent with the primary row.
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
