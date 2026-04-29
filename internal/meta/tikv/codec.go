package tikv

import (
	"encoding/json"

	"github.com/danchupin/strata/internal/meta"
)

// encodeBucket serialises a meta.Bucket into the byte slice we persist under
// BucketKey(name). JSON keeps the encoding human-readable for ops debugging
// and additive — old gateways decode rows written by newer ones with zero
// values for unknown fields.
func encodeBucket(b *meta.Bucket) ([]byte, error) {
	return json.Marshal(b)
}

// decodeBucket reverses encodeBucket.
func decodeBucket(raw []byte) (*meta.Bucket, error) {
	var b meta.Bucket
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	if b.Versioning == "" {
		b.Versioning = meta.VersioningDisabled
	}
	return &b, nil
}
