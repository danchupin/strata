package tikv

import (
	"bytes"
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// listScanBatchSize is how many KV pairs we pull per Scan round-trip when
// streaming ListObjects/ListObjectVersions. Generous default — page-size
// tuning is the gateway's job; keep this side simple.
const listScanBatchSize = 1024

// ScanObjects satisfies meta.RangeScanStore (US-012). The TiKV layout is a
// globally sorted byte-string keyspace, so ListObjects is already a single
// continuous range scan; ScanObjects is the same call surfaced under the
// optional capability interface so the gateway dispatch site can pick the
// efficient path via type assertion.
func (s *Store) ScanObjects(ctx context.Context, bucketID uuid.UUID, opts meta.ListOptions) (*meta.ListResult, error) {
	return s.ListObjects(ctx, bucketID, opts)
}

// ListObjects emits one row per user-space key (the latest non-delete-marker
// version) by issuing a single ordered range scan over the bucket's object
// prefix. The version-DESC suffix encoding (US-002) means lex-first row per
// key is the latest version, so we can dedupe by tracking the prior key.
//
// Where the Cassandra path fans out across N shard partitions and merges, the
// TiKV path is one continuous scan — that is the whole point of US-005.
func (s *Store) ListObjects(ctx context.Context, bucketID uuid.UUID, opts meta.ListOptions) (*meta.ListResult, error) {
	limit := opts.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}

	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()

	start := ObjectPrefixWithKey(bucketID, opts.Prefix)
	end := prefixEnd(ObjectPrefix(bucketID))

	if opts.Marker != "" {
		// Skip the marker key entirely — ListObjects emits keys strictly
		// greater than marker. Advance start past every row of the marker key.
		markerStart := append(ObjectPrefixWithKey(bucketID, opts.Marker), 0x00, 0x00)
		markerEnd := prefixEnd(markerStart)
		if bytes.Compare(markerEnd, start) > 0 {
			start = markerEnd
		}
	}

	res := &meta.ListResult{}
	seenPrefix := make(map[string]struct{})
	var lastKey string
	firstKey := true

	cursor := start
	for {
		pairs, err := txn.Scan(ctx, cursor, end, listScanBatchSize)
		if err != nil {
			return nil, err
		}
		if len(pairs) == 0 {
			break
		}
		for _, p := range pairs {
			_, key, _, decErr := DecodeObjectKey(p.Key)
			if decErr != nil {
				return nil, decErr
			}

			if !firstKey && key == lastKey {
				continue
			}
			firstKey = false
			lastKey = key

			if opts.Prefix != "" && !strings.HasPrefix(key, opts.Prefix) {
				if key > opts.Prefix {
					return res, nil
				}
				continue
			}

			if opts.Delimiter != "" {
				rest := key[len(opts.Prefix):]
				if idx := strings.Index(rest, opts.Delimiter); idx >= 0 {
					pfx := opts.Prefix + rest[:idx+len(opts.Delimiter)]
					if _, ok := seenPrefix[pfx]; !ok {
						if len(res.Objects)+len(res.CommonPrefixes) >= limit {
							res.Truncated = true
							res.NextMarker = pfx
							return res, nil
						}
						seenPrefix[pfx] = struct{}{}
						res.CommonPrefixes = append(res.CommonPrefixes, pfx)
					}
					continue
				}
			}

			obj, decObjErr := decodeObject(p.Value)
			if decObjErr != nil {
				return nil, decObjErr
			}
			if obj.IsDeleteMarker {
				continue
			}

			if len(res.Objects)+len(res.CommonPrefixes) >= limit {
				res.Truncated = true
				res.NextMarker = key
				return res, nil
			}
			obj.IsLatest = true
			res.Objects = append(res.Objects, obj)
		}
		if len(pairs) < listScanBatchSize {
			break
		}
		// Advance past the last seen key so the next Scan resumes there.
		last := pairs[len(pairs)-1].Key
		cursor = append(append([]byte(nil), last...), 0x00)
	}
	return res, nil
}

// ListObjectVersions emits every version row in the bucket ordered by
// (key ASC, version-DESC). The first row encountered for each key carries
// IsLatest=true; subsequent versions carry IsLatest=false. CommonPrefixes
// collapse keys that share a delimiter-bounded prefix.
func (s *Store) ListObjectVersions(ctx context.Context, bucketID uuid.UUID, opts meta.ListOptions) (*meta.ListVersionsResult, error) {
	limit := opts.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}

	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()

	start := ObjectPrefixWithKey(bucketID, opts.Prefix)
	end := prefixEnd(ObjectPrefix(bucketID))

	if opts.Marker != "" {
		// Include the marker key itself — ListObjectVersions emits keys
		// greater-or-equal-to marker (matches the Cassandra `key >= ?` shape).
		markerStart := append(ObjectPrefixWithKey(bucketID, opts.Marker), 0x00, 0x00)
		if bytes.Compare(markerStart, start) > 0 {
			start = markerStart
		}
	}

	res := &meta.ListVersionsResult{}
	seenPrefix := make(map[string]struct{})
	var lastKey string
	firstKey := true
	firstVersionForKey := true

	cursor := start
	for {
		pairs, err := txn.Scan(ctx, cursor, end, listScanBatchSize)
		if err != nil {
			return nil, err
		}
		if len(pairs) == 0 {
			break
		}
		for _, p := range pairs {
			_, key, _, decErr := DecodeObjectKey(p.Key)
			if decErr != nil {
				return nil, decErr
			}

			if firstKey || key != lastKey {
				firstVersionForKey = true
				lastKey = key
				firstKey = false
			}

			if opts.Prefix != "" && !strings.HasPrefix(key, opts.Prefix) {
				if key > opts.Prefix {
					return res, nil
				}
				continue
			}

			if opts.Delimiter != "" {
				rest := key[len(opts.Prefix):]
				if idx := strings.Index(rest, opts.Delimiter); idx >= 0 {
					pfx := opts.Prefix + rest[:idx+len(opts.Delimiter)]
					if _, ok := seenPrefix[pfx]; !ok {
						seenPrefix[pfx] = struct{}{}
						res.CommonPrefixes = append(res.CommonPrefixes, pfx)
					}
					continue
				}
			}

			obj, decObjErr := decodeObject(p.Value)
			if decObjErr != nil {
				return nil, decObjErr
			}
			obj.IsLatest = firstVersionForKey
			firstVersionForKey = false

			if len(res.Versions) >= limit {
				res.Truncated = true
				res.NextKeyMarker = obj.Key
				res.NextVersionID = obj.VersionID
				return res, nil
			}
			res.Versions = append(res.Versions, obj)
		}
		if len(pairs) < listScanBatchSize {
			break
		}
		last := pairs[len(pairs)-1].Key
		cursor = append(append([]byte(nil), last...), 0x00)
	}
	return res, nil
}
