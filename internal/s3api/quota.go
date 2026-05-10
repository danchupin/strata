package s3api

import (
	"context"

	"github.com/danchupin/strata/internal/meta"
)

// quotaWriteIntent describes the size impact of a single write that needs
// quota validation. AddBytes is the plaintext byte delta about to land on the
// bucket (positive for PUT / multipart Complete; the helper is not consulted
// for negative-delta paths). AddObjects is the row-count delta — 1 for a
// fresh PUT, 0 for an in-place overwrite of the same key (caller resolves
// "is this an overwrite" via Meta.GetObject before calling).
//
// PerObjectBytes is the size that the configured BucketQuota.MaxBytesPerObject
// must accommodate. For a single PUT it's Content-Length; for a multipart
// Complete it's the declared total of all parts; for individual UploadPart
// calls the value is zero (per-object cap is checked at Complete time).
type quotaWriteIntent struct {
	AddBytes       int64
	AddObjects     int64
	PerObjectBytes int64
}

// checkQuota enforces the BucketQuota + UserQuota caps configured for the
// destination bucket and its owner. Returns ErrQuotaExceeded (the gateway
// APIError-shaped sentinel) when the proposed write would breach any cap.
// "Zero ⇒ unlimited" matches AWS / RGW shape, so an unset field never blocks.
//
// Source-of-truth for live usage is meta.BucketStats — the denormalised
// counter US-004 maintains. User-scope checks fan out across the owner's
// bucket list and sum bucket_stats per CLAUDE.md "v1 user-scope check uses
// ListBuckets walk; denormalised user_bucket_count is a P3 follow-up".
func (s *Server) checkQuota(ctx context.Context, b *meta.Bucket, intent quotaWriteIntent) error {
	bq, bqOK, err := s.Meta.GetBucketQuota(ctx, b.ID)
	if err != nil {
		return err
	}
	if bqOK {
		if bq.MaxBytesPerObject > 0 && intent.PerObjectBytes > bq.MaxBytesPerObject {
			return meta.ErrQuotaExceeded
		}
		if bq.MaxBytes > 0 || bq.MaxObjects > 0 {
			stats, serr := s.Meta.GetBucketStats(ctx, b.ID)
			if serr != nil {
				return serr
			}
			if bq.MaxBytes > 0 && stats.UsedBytes+intent.AddBytes > bq.MaxBytes {
				return meta.ErrQuotaExceeded
			}
			if bq.MaxObjects > 0 && stats.UsedObjects+intent.AddObjects > bq.MaxObjects {
				return meta.ErrQuotaExceeded
			}
		}
	}
	if b.Owner == "" {
		return nil
	}
	uq, uqOK, err := s.Meta.GetUserQuota(ctx, b.Owner)
	if err != nil {
		return err
	}
	if !uqOK || uq.TotalMaxBytes <= 0 {
		return nil
	}
	totalUsed, err := s.userUsedBytes(ctx, b.Owner)
	if err != nil {
		return err
	}
	if totalUsed+intent.AddBytes > uq.TotalMaxBytes {
		return meta.ErrQuotaExceeded
	}
	return nil
}

// userUsedBytes sums BucketStats.UsedBytes across every bucket owned by user.
// Walks ListBuckets; v1 shape — see CLAUDE.md note on the P3 denormalised
// user_bucket_count follow-up.
func (s *Server) userUsedBytes(ctx context.Context, user string) (int64, error) {
	buckets, err := s.Meta.ListBuckets(ctx, user)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, ub := range buckets {
		stats, err := s.Meta.GetBucketStats(ctx, ub.ID)
		if err != nil {
			return 0, err
		}
		total += stats.UsedBytes
	}
	return total, nil
}

// checkUserBucketQuota enforces UserQuota.MaxBuckets at CreateBucket time.
// Caller passes the freshly-resolved owner (auth.FromContext(ctx).Owner).
// Returns ErrQuotaExceeded if creating one more bucket would breach the cap.
// MaxBuckets <= 0 means unlimited.
func (s *Server) checkUserBucketQuota(ctx context.Context, owner string) error {
	if owner == "" {
		return nil
	}
	uq, ok, err := s.Meta.GetUserQuota(ctx, owner)
	if err != nil {
		return err
	}
	if !ok || uq.MaxBuckets <= 0 {
		return nil
	}
	buckets, err := s.Meta.ListBuckets(ctx, owner)
	if err != nil {
		return err
	}
	if int32(len(buckets)) >= uq.MaxBuckets {
		return meta.ErrQuotaExceeded
	}
	return nil
}
