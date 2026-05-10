package tikv

import (
	"context"
	"strings"
	"time"

	"github.com/gocql/gocql"
	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// Multipart upload status values. Mirrors Cassandra's `multipart_uploads.status`
// column. The "uploading" → "completing" flip in CompleteMultipartUpload is the
// LWT-equivalent serialisation point — only one concurrent caller observes
// status='uploading' and proceeds.
const (
	multipartStatusUploading  = "uploading"
	multipartStatusCompleting = "completing"
)

// CreateMultipartUpload writes the upload status row under
// MultipartKey(bucketID, uploadID). The Status column is forced to
// "uploading" regardless of what the caller passed so the CompleteMultipartUpload
// CAS-flip has a single source of truth.
func (s *Store) CreateMultipartUpload(ctx context.Context, mu *meta.MultipartUpload) (err error) {
	row := *mu
	row.Status = multipartStatusUploading
	if row.InitiatedAt.IsZero() {
		row.InitiatedAt = time.Now().UTC()
	}
	payload, err := encodeMultipart(&row)
	if err != nil {
		return err
	}
	key := MultipartKey(mu.BucketID, mu.UploadID)
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, key); err != nil {
		return err
	}
	if err = txn.Set(key, payload); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// GetMultipartUpload is a single Get against the upload status row.
func (s *Store) GetMultipartUpload(ctx context.Context, bucketID uuid.UUID, uploadID string) (*meta.MultipartUpload, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	raw, found, err := txn.Get(ctx, MultipartKey(bucketID, uploadID))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, meta.ErrMultipartNotFound
	}
	return decodeMultipart(raw)
}

// ListMultipartUploads is a range scan over the per-bucket multipart prefix.
// The optional key prefix is applied in-process — concurrent multipart counts
// are bounded by the gateway's part-storage budget so the in-process filter is
// fine.
func (s *Store) ListMultipartUploads(ctx context.Context, bucketID uuid.UUID, prefix string, limit int) ([]*meta.MultipartUpload, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()

	start := MultipartPrefix(bucketID)
	pairs, err := txn.Scan(ctx, start, prefixEnd(start), 0)
	if err != nil {
		return nil, err
	}
	out := make([]*meta.MultipartUpload, 0, len(pairs))
	for _, p := range pairs {
		mu, err := decodeMultipart(p.Value)
		if err != nil {
			return nil, err
		}
		if prefix != "" && !strings.HasPrefix(mu.Key, prefix) {
			continue
		}
		out = append(out, mu)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// SavePart writes one MultipartPart row under MultipartPartKey(...). Mirrors
// the memory backend's contract — the upload row must exist and be in
// "uploading" state, otherwise return ErrMultipartNotFound /
// ErrMultipartInProgress. The part-row write itself is unconditional within
// the txn (last writer wins on partNumber re-upload, matching S3 semantics).
func (s *Store) SavePart(ctx context.Context, bucketID uuid.UUID, uploadID string, part *meta.MultipartPart) (err error) {
	uploadKey := MultipartKey(bucketID, uploadID)
	partKey := MultipartPartKey(bucketID, uploadID, part.PartNumber)

	row := *part
	if row.Mtime.IsZero() {
		row.Mtime = time.Now().UTC()
	}
	payload, err := encodePart(&row)
	if err != nil {
		return err
	}

	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)

	if err = txn.LockKeys(ctx, uploadKey, partKey); err != nil {
		return err
	}
	raw, found, err := txn.Get(ctx, uploadKey)
	if err != nil {
		return err
	}
	if !found {
		return meta.ErrMultipartNotFound
	}
	mu, err := decodeMultipart(raw)
	if err != nil {
		return err
	}
	if mu.Status != multipartStatusUploading {
		return meta.ErrMultipartInProgress
	}
	if err = txn.Set(partKey, payload); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// ListParts is a per-upload range scan over MultipartPartPrefix. Parts are
// returned in ascending PartNumber order — the 4-byte big-endian partNumber
// suffix encoding (see keys.go) makes this free.
func (s *Store) ListParts(ctx context.Context, bucketID uuid.UUID, uploadID string) ([]*meta.MultipartPart, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()

	if _, found, err := txn.Get(ctx, MultipartKey(bucketID, uploadID)); err != nil {
		return nil, err
	} else if !found {
		return nil, meta.ErrMultipartNotFound
	}

	start := MultipartPartPrefix(bucketID, uploadID)
	pairs, err := txn.Scan(ctx, start, prefixEnd(start), 0)
	if err != nil {
		return nil, err
	}
	out := make([]*meta.MultipartPart, 0, len(pairs))
	for _, p := range pairs {
		part, err := decodePart(p.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, part)
	}
	return out, nil
}

// CompleteMultipartUpload is the LWT-equivalent flip + materialise. The
// pessimistic txn locks the upload row, asserts status=='uploading', flips it
// to 'completing' (so a concurrent retry observes ErrMultipartInProgress), then
// reads/validates each requested part, builds the final object payload, writes
// the object row, and deletes the multipart upload + part rows. Orphan part
// manifests (parts saved but not listed in CompleteMultipartUpload) are
// returned for the caller to GC.
func (s *Store) CompleteMultipartUpload(ctx context.Context, obj *meta.Object, uploadID string, parts []meta.CompletePart, versioned bool) (orphans []*data.Manifest, err error) {
	uploadKey := MultipartKey(obj.BucketID, uploadID)

	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return nil, err
	}
	defer rollbackOnError(txn, &err)

	if err = txn.LockKeys(ctx, uploadKey); err != nil {
		return nil, err
	}
	raw, found, err := txn.Get(ctx, uploadKey)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, meta.ErrMultipartNotFound
	}
	mu, err := decodeMultipart(raw)
	if err != nil {
		return nil, err
	}
	if mu.Status != multipartStatusUploading {
		return nil, meta.ErrMultipartInProgress
	}
	mu.Status = multipartStatusCompleting
	flipped, err := encodeMultipart(mu)
	if err != nil {
		return nil, err
	}
	if err = txn.Set(uploadKey, flipped); err != nil {
		return nil, err
	}

	partsStart := MultipartPartPrefix(obj.BucketID, uploadID)
	partPairs, err := txn.Scan(ctx, partsStart, prefixEnd(partsStart), 0)
	if err != nil {
		return nil, err
	}
	stored := make(map[int]*meta.MultipartPart, len(partPairs))
	for _, p := range partPairs {
		part, derr := decodePart(p.Value)
		if derr != nil {
			err = derr
			return nil, err
		}
		stored[part.PartNumber] = part
	}

	used := make(map[int]bool, len(parts))
	var chunks []data.ChunkRef
	var totalSize int64
	var ciphertextSize int64
	partChunkCounts := make([]int, 0, len(parts))
	partRanges := make([]data.PartRange, 0, len(parts))
	partSizes := make([]int64, 0, len(parts))
	partChecksums := make([]map[string]string, 0, len(parts))
	for _, cp := range parts {
		p, ok := stored[cp.PartNumber]
		if !ok {
			err = meta.ErrMultipartPartMissing
			return nil, err
		}
		if strings.Trim(cp.ETag, `"`) != p.ETag {
			err = meta.ErrMultipartETagMismatch
			return nil, err
		}
		partChunkCount := 0
		if p.Manifest != nil {
			chunks = append(chunks, p.Manifest.Chunks...)
			partChunkCount = len(p.Manifest.Chunks)
			for _, c := range p.Manifest.Chunks {
				ciphertextSize += c.Size
			}
		}
		partChunkCounts = append(partChunkCounts, partChunkCount)
		partRanges = append(partRanges, meta.BuildPartRange(cp.PartNumber, totalSize, p))
		partSizes = append(partSizes, p.Size)
		partChecksums = append(partChecksums, p.Checksums)
		totalSize += p.Size
		used[cp.PartNumber] = true
	}

	obj.Manifest = &data.Manifest{
		Class:           obj.StorageClass,
		Size:            ciphertextSize,
		ChunkSize:       data.DefaultChunkSize,
		ETag:            obj.ETag,
		Chunks:          chunks,
		PartChunks:      partRanges,
		PartChunkCounts: partChunkCounts,
		PartChecksums:   partChecksums,
	}
	obj.Size = totalSize
	obj.PartSizes = partSizes
	obj.Mtime = time.Now().UTC()

	switch {
	case !versioned:
		obj.VersionID = meta.NullVersionID
		obj.IsNull = true
	case obj.VersionID == "":
		obj.VersionID = gocql.TimeUUID().String()
	}
	obj.IsLatest = true

	objKey, err := ObjectKey(obj.BucketID, obj.Key, obj.VersionID)
	if err != nil {
		return nil, err
	}
	if err = txn.LockKeys(ctx, objKey); err != nil {
		return nil, err
	}
	// Capture prior latest (if any) before deleting versions, so the
	// bucket_stats bump can subtract the replaced bytes on unversioned
	// completion.
	var prior *meta.Object
	if !versioned {
		prefix := append(ObjectPrefixWithKey(obj.BucketID, obj.Key), 0x00, 0x00)
		end := prefixEnd(prefix)
		objPairs, perr := txn.Scan(ctx, prefix, end, 0)
		if perr != nil {
			err = perr
			return nil, err
		}
		if len(objPairs) > 0 {
			p, derr := decodeObject(objPairs[0].Value)
			if derr != nil {
				err = derr
				return nil, err
			}
			prior = p
		}
		for _, p := range objPairs {
			if err = txn.Delete(p.Key); err != nil {
				return nil, err
			}
		}
	}
	objPayload, err := encodeObject(obj)
	if err != nil {
		return nil, err
	}
	if err = txn.Set(objKey, objPayload); err != nil {
		return nil, err
	}

	for _, p := range partPairs {
		if err = txn.Delete(p.Key); err != nil {
			return nil, err
		}
	}
	if err = txn.Delete(uploadKey); err != nil {
		return nil, err
	}

	if err = txn.Commit(ctx); err != nil {
		return nil, err
	}

	deltaBytes, deltaObjects := bucketStatsDelta(prior, obj)
	if deltaBytes != 0 || deltaObjects != 0 {
		if _, berr := s.BumpBucketStats(ctx, obj.BucketID, deltaBytes, deltaObjects); berr != nil {
			return orphans, berr
		}
	}

	for num, p := range stored {
		if !used[num] && p.Manifest != nil {
			orphans = append(orphans, p.Manifest)
		}
	}
	return orphans, nil
}

// AbortMultipartUpload deletes the upload row + every part row in one
// pessimistic txn. Idempotent — second abort returns ErrMultipartNotFound,
// matching memory/Cassandra. Returns the manifests of every part so the
// caller can GC the storage chunks.
func (s *Store) AbortMultipartUpload(ctx context.Context, bucketID uuid.UUID, uploadID string) (manifests []*data.Manifest, err error) {
	uploadKey := MultipartKey(bucketID, uploadID)

	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return nil, err
	}
	defer rollbackOnError(txn, &err)

	if err = txn.LockKeys(ctx, uploadKey); err != nil {
		return nil, err
	}
	_, found, err := txn.Get(ctx, uploadKey)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, meta.ErrMultipartNotFound
	}

	partsStart := MultipartPartPrefix(bucketID, uploadID)
	partPairs, err := txn.Scan(ctx, partsStart, prefixEnd(partsStart), 0)
	if err != nil {
		return nil, err
	}
	for _, p := range partPairs {
		part, derr := decodePart(p.Value)
		if derr != nil {
			err = derr
			return nil, err
		}
		if part.Manifest != nil {
			manifests = append(manifests, part.Manifest)
		}
		if err = txn.Delete(p.Key); err != nil {
			return nil, err
		}
	}
	if err = txn.Delete(uploadKey); err != nil {
		return nil, err
	}
	if err = txn.Commit(ctx); err != nil {
		return nil, err
	}
	return manifests, nil
}

// RecordMultipartCompletion stores the idempotency record for a successful
// CompleteMultipartUpload. TiKV has no native TTL; the row payload carries
// ExpiresAt and GetMultipartCompletion lazily expires on read.
func (s *Store) RecordMultipartCompletion(ctx context.Context, rec *meta.MultipartCompletion, ttl time.Duration) (err error) {
	if rec == nil {
		return nil
	}
	row := *rec
	expiresAt := time.Now().UTC().Add(ttl)
	payload, err := encodeMultipartCompletion(&row, expiresAt)
	if err != nil {
		return err
	}
	key := MultipartCompletionKey(rec.BucketID, rec.UploadID)
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, key); err != nil {
		return err
	}
	if err = txn.Set(key, payload); err != nil {
		return err
	}
	return txn.Commit(ctx)
}

// GetMultipartCompletion returns the persisted CompleteMultipartUpload reply
// body for the given uploadID, or ErrMultipartCompletionNotFound when the row
// is missing or has aged past its ExpiresAt.
func (s *Store) GetMultipartCompletion(ctx context.Context, bucketID uuid.UUID, uploadID string) (*meta.MultipartCompletion, error) {
	txn, err := s.kv.Begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer txn.Rollback()
	raw, found, err := txn.Get(ctx, MultipartCompletionKey(bucketID, uploadID))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, meta.ErrMultipartCompletionNotFound
	}
	rec, expiresAt, err := decodeMultipartCompletion(raw)
	if err != nil {
		return nil, err
	}
	if !time.Now().UTC().Before(expiresAt) {
		return nil, meta.ErrMultipartCompletionNotFound
	}
	return rec, nil
}

// UpdateMultipartUploadSSEWrap rewraps the per-upload SSE DEK. Used by
// strata-admin rewrap to rotate the master key. Returns
// ErrMultipartNotFound when the upload row is gone (matches memory).
func (s *Store) UpdateMultipartUploadSSEWrap(ctx context.Context, bucketID uuid.UUID, uploadID string, wrapped []byte, keyID string) (err error) {
	key := MultipartKey(bucketID, uploadID)
	txn, err := s.kv.Begin(ctx, true)
	if err != nil {
		return err
	}
	defer rollbackOnError(txn, &err)
	if err = txn.LockKeys(ctx, key); err != nil {
		return err
	}
	raw, found, err := txn.Get(ctx, key)
	if err != nil {
		return err
	}
	if !found {
		return meta.ErrMultipartNotFound
	}
	mu, err := decodeMultipart(raw)
	if err != nil {
		return err
	}
	mu.SSEKey = append([]byte(nil), wrapped...)
	mu.SSEKeyID = keyID
	payload, err := encodeMultipart(mu)
	if err != nil {
		return err
	}
	if err = txn.Set(key, payload); err != nil {
		return err
	}
	return txn.Commit(ctx)
}
