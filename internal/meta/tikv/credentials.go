package tikv

import (
	"context"
	"errors"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/meta"
)

// CredentialStore looks up IAM access keys via Store.GetIAMAccessKey and
// satisfies auth.CredentialsStore. Mirrors memory + cassandra CredentialStore
// semantics — disabled rows surface as ErrNoSuchCredential so revoked keys are
// treated identically to deleted ones from the SigV4 middleware's perspective.
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
