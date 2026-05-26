// S3 Access Points on TiKV (US-004 of ralph/tikv-stubs).
//
// One access-point row materialises into three index entries kept in
// lockstep so every lookup is a bounded number of single-row Gets and the
// per-bucket list is a single range scan — no ALLOW FILTERING-equivalent
// global scan. Layout:
//
//   - AccessPointKey(name)                       → full *meta.AccessPoint blob
//   - AccessPointAliasKey(alias)                 → name pointer (for GetAccessPointByAlias)
//   - AccessPointByBucketKey(bucketID, name)     → empty (for ListAccessPoints)
//
// Writes go through one pessimistic txn (LockKeys + Get + Set) so a
// duplicate Name surfaces ErrAccessPointAlreadyExists deterministically
// and the three views never desync. Pure reads (GetAccessPoint,
// ListAccessPoints) use a single optimistic txn — no LockKeys.
package tikv

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// accessPointRow is the persisted shape for one access point. JSON keys
// are short to keep TiKV row payloads tight; the encoding is internal so
// the abbreviations are safe.
type accessPointRow struct {
	Name              string    `json:"n"`
	BucketID          uuid.UUID `json:"bi"`
	Bucket            string    `json:"b"`
	Alias             string    `json:"a"`
	NetworkOrigin     string    `json:"no,omitempty"`
	VPCID             string    `json:"vp,omitempty"`
	Policy            []byte    `json:"p,omitempty"`
	PublicAccessBlock []byte    `json:"pab,omitempty"`
	CreatedAt         time.Time `json:"c"`
}

func encodeAccessPoint(ap *meta.AccessPoint) ([]byte, error) {
	return json.Marshal(&accessPointRow{
		Name:              ap.Name,
		BucketID:          ap.BucketID,
		Bucket:            ap.Bucket,
		Alias:             ap.Alias,
		NetworkOrigin:     ap.NetworkOrigin,
		VPCID:             ap.VPCID,
		Policy:            ap.Policy,
		PublicAccessBlock: ap.PublicAccessBlock,
		CreatedAt:         ap.CreatedAt,
	})
}

func decodeAccessPoint(raw []byte) (*meta.AccessPoint, error) {
	var row accessPointRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	return &meta.AccessPoint{
		Name:              row.Name,
		BucketID:          row.BucketID,
		Bucket:            row.Bucket,
		Alias:             row.Alias,
		NetworkOrigin:     row.NetworkOrigin,
		VPCID:             row.VPCID,
		Policy:            append([]byte(nil), row.Policy...),
		PublicAccessBlock: append([]byte(nil), row.PublicAccessBlock...),
		CreatedAt:         row.CreatedAt,
	}, nil
}

// CreateAccessPoint writes the by-name row + alias-pointer row + by-bucket
// index row in one pessimistic txn so the three views are atomic. A
// duplicate Name returns meta.ErrAccessPointAlreadyExists.
func (s *Store) CreateAccessPoint(ctx context.Context, ap *meta.AccessPoint) (err error) {
	ctx, finish := s.observer.Start(ctx, "CreateAccessPoint", "access_points")
	defer func() { finish(err) }()
	row := *ap
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	payload, err := encodeAccessPoint(&row)
	if err != nil {
		return err
	}
	nameKey := AccessPointKey(row.Name)
	aliasKey := AccessPointAliasKey(row.Alias)
	bktKey := AccessPointByBucketKey(row.BucketID, row.Name)
	txn, err := s.beginPessimistic(ctx)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, nameKey, aliasKey, bktKey); err != nil {
		return err
	}
	_, found, err := txn.Get(ctx, nameKey)
	if err != nil {
		return err
	}
	if found {
		_ = txn.Rollback()
		return meta.ErrAccessPointAlreadyExists
	}
	if err = txn.Set(nameKey, payload); err != nil {
		return err
	}
	if err = txn.Set(aliasKey, []byte(row.Name)); err != nil {
		return err
	}
	// Real TiKV rejects nil OR empty-len values with "can not set nil value"
	// — see txnkv/transaction/txn.go `Set`. Write a single sentinel byte
	// (0x01); readers never decode the value, just check existence.
	if err = txn.Set(bktKey, []byte{1}); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// GetAccessPoint is a single optimistic Get against AccessPointKey.
func (s *Store) GetAccessPoint(ctx context.Context, name string) (ap *meta.AccessPoint, err error) {
	ctx, finish := s.observer.Start(ctx, "GetAccessPoint", "access_points")
	defer func() { finish(err) }()
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	raw, found, err := txn.Get(ctx, AccessPointKey(name))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, meta.ErrAccessPointNotFound
	}
	return decodeAccessPoint(raw)
}

// GetAccessPointByAlias resolves alias → name via the alias-pointer row,
// then snapshot-reads the by-name row to return the full record. If a
// concurrent DeleteAccessPoint removed the by-name row between the two
// reads (alias row already gone too, but a torn observer might see one
// side), the second Get returns ErrAccessPointNotFound — the caller
// observes a linearizable "deleted" state regardless of which arm of the
// race they hit.
func (s *Store) GetAccessPointByAlias(ctx context.Context, alias string) (ap *meta.AccessPoint, err error) {
	ctx, finish := s.observer.Start(ctx, "GetAccessPointByAlias", "access_points")
	defer func() { finish(err) }()
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	rawName, found, err := txn.Get(ctx, AccessPointAliasKey(alias))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, meta.ErrAccessPointNotFound
	}
	name := string(rawName)
	raw, found, err := txn.Get(ctx, AccessPointKey(name))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, meta.ErrAccessPointNotFound
	}
	return decodeAccessPoint(raw)
}

// DeleteAccessPoint reads the by-name row to learn the alias + bucketID
// (so the dependent rows can be addressed), then deletes the by-name,
// alias-pointer, and by-bucket rows in one pessimistic txn. Returns
// meta.ErrAccessPointNotFound on absent name.
func (s *Store) DeleteAccessPoint(ctx context.Context, name string) (err error) {
	ctx, finish := s.observer.Start(ctx, "DeleteAccessPoint", "access_points")
	defer func() { finish(err) }()
	nameKey := AccessPointKey(name)
	txn, err := s.beginPessimistic(ctx)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, nameKey); err != nil {
		return err
	}
	raw, found, err := txn.Get(ctx, nameKey)
	if err != nil {
		return err
	}
	if !found {
		_ = txn.Rollback()
		return meta.ErrAccessPointNotFound
	}
	row, err := decodeAccessPoint(raw)
	if err != nil {
		return err
	}
	aliasKey := AccessPointAliasKey(row.Alias)
	bktKey := AccessPointByBucketKey(row.BucketID, row.Name)
	if err = txn.LockKeys(ctx, aliasKey, bktKey); err != nil {
		return err
	}
	if err = txn.Delete(nameKey); err != nil {
		return err
	}
	if err = txn.Delete(aliasKey); err != nil {
		return err
	}
	if err = txn.Delete(bktKey); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// ListAccessPoints range-scans the appropriate index in one optimistic
// txn. bucketID == uuid.Nil scans the global by-name prefix and decodes
// each payload directly. A specific bucketID scans the per-bucket index
// (single-partition equivalent on TiKV) and follow-up-Gets each by-name
// row to assemble the full record. Results are sorted by Name ascending
// to match the Cassandra impl.
func (s *Store) ListAccessPoints(ctx context.Context, bucketID uuid.UUID) (out []*meta.AccessPoint, err error) {
	ctx, finish := s.observer.Start(ctx, "ListAccessPoints", "access_points")
	defer func() { finish(err) }()
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()

	if bucketID == uuid.Nil {
		start := AccessPointPrefix()
		pairs, serr := txn.Scan(ctx, start, prefixEnd(start), 0)
		if serr != nil {
			return nil, serr
		}
		out = make([]*meta.AccessPoint, 0, len(pairs))
		for _, p := range pairs {
			ap, derr := decodeAccessPoint(p.Value)
			if derr != nil {
				return nil, derr
			}
			out = append(out, ap)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out, nil
	}

	idxPrefix := AccessPointByBucketPrefix(bucketID)
	pairs, err := txn.Scan(ctx, idxPrefix, prefixEnd(idxPrefix), 0)
	if err != nil {
		return nil, err
	}
	out = make([]*meta.AccessPoint, 0, len(pairs))
	for _, p := range pairs {
		body := p.Key[len(idxPrefix):]
		name, _, derr := readEscaped(body)
		if derr != nil {
			return nil, derr
		}
		raw, found, gerr := txn.Get(ctx, AccessPointKey(name))
		if gerr != nil {
			return nil, gerr
		}
		if !found {
			continue
		}
		ap, derr := decodeAccessPoint(raw)
		if derr != nil {
			return nil, derr
		}
		out = append(out, ap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
