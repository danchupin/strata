package s3

import (
	"context"
	"errors"
	"fmt"
	"strings"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/danchupin/strata/internal/data"
)

// nativeTransitionClasses maps Strata's storage-class string vocabulary to
// the SDK's TransitionStorageClass enum for classes the backend handles
// natively (US-014). Classes outside this map fall back to Strata's own
// lifecycle worker — translation is best-effort by design.
//
// REDUCED_REDUNDANCY is intentionally omitted: AWS deprecated it for new
// rules and several S3-compatible backends reject it on lifecycle PUT.
var nativeTransitionClasses = map[string]s3types.TransitionStorageClass{
	"STANDARD_IA":         s3types.TransitionStorageClassStandardIa,
	"ONEZONE_IA":          s3types.TransitionStorageClassOnezoneIa,
	"GLACIER_IR":          s3types.TransitionStorageClassGlacierIr,
	"GLACIER":             s3types.TransitionStorageClassGlacier,
	"DEEP_ARCHIVE":        s3types.TransitionStorageClassDeepArchive,
	"INTELLIGENT_TIERING": s3types.TransitionStorageClassIntelligentTiering,
}

// IsNativeTransitionClass reports whether the supplied Strata storage class
// is one the s3 backend can translate into a native backend lifecycle
// transition. The lifecycle worker uses this to skip transitions the backend
// already owns (US-014: avoid double-work).
func IsNativeTransitionClass(class string) bool {
	_, ok := nativeTransitionClasses[strings.ToUpper(class)]
	return ok
}

// PutBackendLifecycle translates the supplied Strata-shape rules into a
// backend BucketLifecycleConfiguration and applies it via the SDK
// PutBucketLifecycleConfiguration call.
//
// Each emitted backend rule has its Filter.Prefix scoped to bucketPrefix +
// rule.Prefix so multiple Strata buckets sharing the single backend bucket
// don't collide. bucketPrefix is the Strata bucket UUID followed by "/"
// (matches the BackendRef key prefix produced by PutChunks).
//
// Rules whose only action is a non-native transition are reported in
// skippedRuleIDs and dropped from the backend config — the lifecycle worker
// keeps owning those. A rule with a native-transition action AND extra
// strata-only actions emits the native parts; the worker still handles the
// rest. Expirations + AbortIncompleteMultipartUpload always translate.
//
// On full-skip (no rule produced any backend action) the backend's lifecycle
// is deleted instead of pushing an empty config — DeleteBucketLifecycle is
// the SDK-correct way to express "no rules". Returns the list of skipped
// rule IDs so the caller can WARN-log them.
func (b *Backend) PutBackendLifecycle(ctx context.Context, bucketPrefix string, rules []data.LifecycleRule) ([]string, error) {
	if b.client == nil {
		return nil, errors.ErrUnsupported
	}
	if bucketPrefix == "" {
		return nil, fmt.Errorf("s3: PutBackendLifecycle: bucketPrefix required")
	}

	var skipped []string
	var sdkRules []s3types.LifecycleRule
	for _, r := range rules {
		hasTransition := r.TransitionDays > 0 && r.TransitionStorageClass != ""
		nativeTransition, native := nativeTransitionClasses[strings.ToUpper(r.TransitionStorageClass)]
		hasExpiration := r.ExpirationDays > 0
		hasAbort := r.AbortIncompleteUploadDays > 0

		if hasTransition && !native {
			skipped = append(skipped, r.ID)
		}

		// Drop rules that have nothing to translate after native filtering.
		if !hasExpiration && !hasAbort && !(hasTransition && native) {
			continue
		}

		filterPrefix := bucketPrefix + r.Prefix
		filter := &s3types.LifecycleRuleFilter{Prefix: &filterPrefix}
		ruleID := r.ID
		sdkRule := s3types.LifecycleRule{
			ID:     &ruleID,
			Status: s3types.ExpirationStatusEnabled,
			Filter: filter,
		}
		if hasTransition && native {
			days := int32(r.TransitionDays)
			sdkRule.Transitions = []s3types.Transition{{
				Days:         &days,
				StorageClass: nativeTransition,
			}}
		}
		if hasExpiration {
			days := int32(r.ExpirationDays)
			sdkRule.Expiration = &s3types.LifecycleExpiration{Days: &days}
		}
		if hasAbort {
			days := int32(r.AbortIncompleteUploadDays)
			sdkRule.AbortIncompleteMultipartUpload = &s3types.AbortIncompleteMultipartUpload{
				DaysAfterInitiation: &days,
			}
		}
		sdkRules = append(sdkRules, sdkRule)
	}

	bucket := b.bucket
	if len(sdkRules) == 0 {
		opCtx, cancel := b.opCtx(ctx)
		defer cancel()
		_, err := b.client.DeleteBucketLifecycle(opCtx, &awss3.DeleteBucketLifecycleInput{Bucket: &bucket})
		if err != nil {
			return skipped, fmt.Errorf("s3: clear backend lifecycle %s: %w", bucket, err)
		}
		return skipped, nil
	}

	opCtx, cancel := b.opCtx(ctx)
	defer cancel()
	_, err := b.client.PutBucketLifecycleConfiguration(opCtx, &awss3.PutBucketLifecycleConfigurationInput{
		Bucket:                 &bucket,
		LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{Rules: sdkRules},
	})
	if err != nil {
		return skipped, fmt.Errorf("s3: put backend lifecycle %s: %w", bucket, err)
	}
	return skipped, nil
}

// DeleteBackendLifecycle clears the backend bucket's lifecycle configuration
// for the given Strata bucket. Today the s3 backend stores one bucket-level
// lifecycle (one Strata bucket per backend bucket is the typical deployment
// shape); the bucketPrefix arg is recorded for forward-compat when a single
// backend bucket hosts multiple Strata buckets and per-prefix rule scoping
// becomes load-bearing.
//
// Idempotent: a NoSuchLifecycleConfiguration response is treated as success.
func (b *Backend) DeleteBackendLifecycle(ctx context.Context, bucketPrefix string) error {
	if b.client == nil {
		return errors.ErrUnsupported
	}
	bucket := b.bucket
	opCtx, cancel := b.opCtx(ctx)
	defer cancel()
	_, err := b.client.DeleteBucketLifecycle(opCtx, &awss3.DeleteBucketLifecycleInput{Bucket: &bucket})
	if err != nil {
		// Backends differ in how they signal "nothing to delete"; swallow
		// the most common shapes so the call is idempotent.
		s := err.Error()
		if strings.Contains(s, "NoSuchLifecycleConfiguration") {
			return nil
		}
		return fmt.Errorf("s3: delete backend lifecycle %s: %w", bucket, err)
	}
	return nil
}

// Compile-time assertion that *Backend satisfies data.LifecycleBackend.
var _ data.LifecycleBackend = (*Backend)(nil)
