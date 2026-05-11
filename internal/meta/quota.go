package meta

import (
	"encoding/json"
	"fmt"
)

// EncodeBucketQuota serialises a BucketQuota to the on-disk blob shape used
// by every backend. JSON-shaped so future fields (e.g. per-storage-class
// caps) can land additively without an ALTER.
func EncodeBucketQuota(q BucketQuota) ([]byte, error) {
	return json.Marshal(q)
}

// DecodeBucketQuota reverses EncodeBucketQuota.
func DecodeBucketQuota(blob []byte) (BucketQuota, error) {
	var q BucketQuota
	if len(blob) == 0 {
		return q, nil
	}
	if err := json.Unmarshal(blob, &q); err != nil {
		return BucketQuota{}, fmt.Errorf("decode bucket quota: %w", err)
	}
	return q, nil
}

// EncodeUserQuota serialises a UserQuota to the on-disk blob shape.
func EncodeUserQuota(q UserQuota) ([]byte, error) {
	return json.Marshal(q)
}

// DecodeUserQuota reverses EncodeUserQuota.
func DecodeUserQuota(blob []byte) (UserQuota, error) {
	var q UserQuota
	if len(blob) == 0 {
		return q, nil
	}
	if err := json.Unmarshal(blob, &q); err != nil {
		return UserQuota{}, fmt.Errorf("decode user quota: %w", err)
	}
	return q, nil
}

// EncodeBucketPlacement serialises a placement map to the on-disk blob
// shape used by every backend (US-001 placement-rebalance).
func EncodeBucketPlacement(p map[string]int) ([]byte, error) {
	return json.Marshal(p)
}

// DecodeBucketPlacement reverses EncodeBucketPlacement. An empty / missing
// blob yields a nil map so callers can branch on `policy == nil` for the
// "no policy configured" case.
func DecodeBucketPlacement(blob []byte) (map[string]int, error) {
	if len(blob) == 0 {
		return nil, nil
	}
	var p map[string]int
	if err := json.Unmarshal(blob, &p); err != nil {
		return nil, fmt.Errorf("decode bucket placement: %w", err)
	}
	if len(p) == 0 {
		return nil, nil
	}
	return p, nil
}
