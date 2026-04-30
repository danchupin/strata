package s3

import (
	"context"
	"errors"
	"fmt"
	"time"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/danchupin/strata/internal/data"
)

// DefaultPresignExpires is the default lifetime applied when the caller
// passes expires <= 0 to PresignGetObject. Matches the SDK's own default
// (15 minutes) so behaviour is consistent across clients.
const DefaultPresignExpires = 15 * time.Minute

// PresignGetObject mints a presigned GET URL pointing at the backend
// object referenced by m.BackendRef.Key. The URL is signed with the
// Backend's resolved credentials (static creds from Config or whatever
// the SDK default chain produced) and is valid for `expires`.
//
// The backend's own bucket + endpoint are baked into the URL — the client
// fetches the bytes directly from the backend, bypassing the Strata
// gateway. The gateway pre-checks IAM before calling this method (US-016
// AC: "Strata pre-checks before issuing the URL"); this surface does no
// further authorisation.
//
// Manifests without a BackendRef (rados-shape, legacy) cannot be
// presigned — return errors.ErrUnsupported so the gateway falls back to
// serving the bytes itself.
func (b *Backend) PresignGetObject(ctx context.Context, m *data.Manifest, expires time.Duration) (string, error) {
	if b.client == nil {
		return "", errors.ErrUnsupported
	}
	if m == nil || m.BackendRef == nil {
		return "", errors.ErrUnsupported
	}
	if expires <= 0 {
		expires = DefaultPresignExpires
	}
	bucket := b.bucket
	key := m.BackendRef.Key
	in := &awss3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}
	// Forward the recorded VersionID so a presigned URL against a
	// versioning-enabled backend resolves to the same version Strata
	// recorded at write time, not whatever happens to be latest at
	// fetch time.
	if v := m.BackendRef.VersionID; v != "" {
		ver := v
		in.VersionId = &ver
	}
	presigner := awss3.NewPresignClient(b.client, func(o *awss3.PresignOptions) {
		o.Expires = expires
	})
	out, err := presigner.PresignGetObject(ctx, in)
	if err != nil {
		return "", fmt.Errorf("s3: presign get %s: %w", key, err)
	}
	return out.URL, nil
}

// Compile-time assertion that *Backend satisfies data.PresignBackend.
var _ data.PresignBackend = (*Backend)(nil)
