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
// (15 minutes).
const DefaultPresignExpires = 15 * time.Minute

// PresignGetObject mints a presigned GET URL pointing at the backend
// object referenced by m.BackendRef.Key. US-003 will route per storage
// class; today singleCluster picks the only configured cluster.
func (b *Backend) PresignGetObject(ctx context.Context, m *data.Manifest, expires time.Duration) (string, error) {
	if m == nil || m.BackendRef == nil {
		if len(b.clusters) == 0 {
			return "", errors.ErrUnsupported
		}
		return "", errors.ErrUnsupported
	}
	if expires <= 0 {
		expires = DefaultPresignExpires
	}
	c, bucket, err := b.singleCluster(ctx)
	if err != nil {
		return "", err
	}
	key := m.BackendRef.Key
	in := &awss3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}
	if v := m.BackendRef.VersionID; v != "" {
		ver := v
		in.VersionId = &ver
	}
	presigner := awss3.NewPresignClient(c.client, func(o *awss3.PresignOptions) {
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
