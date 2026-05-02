package memory

import (
	"context"
	"errors"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
)

// CredentialStore looks up access keys in the in-memory access_keys map and
// implements auth.CredentialsStore. Disabled rows are reported as missing so
// the gateway treats key revocation the same as deletion (matches
// cassandra.CredentialStore semantics).
type CredentialStore struct {
	s *Store
}

func NewCredentialStore(s *Store) *CredentialStore {
	return &CredentialStore{s: s}
}

func (c *CredentialStore) Lookup(ctx context.Context, accessKey string) (*auth.Credential, error) {
	ak, err := c.s.GetIAMAccessKey(ctx, accessKey)
	if errors.Is(err, meta.ErrIAMAccessKeyNotFound) {
		return nil, auth.ErrNoSuchCredential
	}
	if err != nil {
		return nil, err
	}
	if ak.Disabled {
		return nil, auth.ErrNoSuchCredential
	}
	return &auth.Credential{
		AccessKey: ak.AccessKeyID,
		Secret:    ak.SecretAccessKey,
		Owner:     ak.UserName,
	}, nil
}
