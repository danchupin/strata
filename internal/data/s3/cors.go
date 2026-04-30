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

// PutBackendCORS pushes the supplied CORS rules onto the backend bucket via
// the SDK PutBucketCors call (US-015). Empty rules clear the backend
// configuration via DeleteBackendCORS instead of pushing an empty rule list
// (S3 rejects an empty CORSConfiguration).
//
// CORS in S3 is bucket-scoped — there's no per-prefix CORS protocol — so
// this surface does not take a bucketPrefix argument. Deployments that share
// one backend bucket across many Strata buckets will see last-writer-wins on
// the backend mirror; the Strata-stored config remains per-Strata-bucket and
// authoritative for the gateway response.
func (b *Backend) PutBackendCORS(ctx context.Context, rules []data.CORSRule) error {
	if b.client == nil {
		return errors.ErrUnsupported
	}
	if len(rules) == 0 {
		return b.DeleteBackendCORS(ctx)
	}

	sdkRules := make([]s3types.CORSRule, len(rules))
	for i, r := range rules {
		sdkRule := s3types.CORSRule{
			AllowedMethods: append([]string(nil), r.AllowedMethods...),
			AllowedOrigins: append([]string(nil), r.AllowedOrigins...),
			AllowedHeaders: append([]string(nil), r.AllowedHeaders...),
			ExposeHeaders:  append([]string(nil), r.ExposeHeaders...),
		}
		if r.ID != "" {
			id := r.ID
			sdkRule.ID = &id
		}
		if r.MaxAgeSeconds > 0 {
			age := int32(r.MaxAgeSeconds)
			sdkRule.MaxAgeSeconds = &age
		}
		sdkRules[i] = sdkRule
	}

	bucket := b.bucket
	opCtx, cancel := b.opCtx(ctx)
	defer cancel()
	_, err := b.client.PutBucketCors(opCtx, &awss3.PutBucketCorsInput{
		Bucket:            &bucket,
		CORSConfiguration: &s3types.CORSConfiguration{CORSRules: sdkRules},
	})
	if err != nil {
		return fmt.Errorf("s3: put backend cors %s: %w", bucket, err)
	}
	return nil
}

// GetBackendCORS reads the backend bucket's CORS configuration and converts
// it to the data.CORSRule shape. NoSuchCORSConfiguration is treated as
// "no rules configured" and returns (nil, nil) so callers can branch on
// emptiness without needing error inspection.
func (b *Backend) GetBackendCORS(ctx context.Context) ([]data.CORSRule, error) {
	if b.client == nil {
		return nil, errors.ErrUnsupported
	}
	bucket := b.bucket
	opCtx, cancel := b.opCtx(ctx)
	defer cancel()
	out, err := b.client.GetBucketCors(opCtx, &awss3.GetBucketCorsInput{Bucket: &bucket})
	if err != nil {
		if isNoSuchCORS(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("s3: get backend cors %s: %w", bucket, err)
	}
	rules := make([]data.CORSRule, len(out.CORSRules))
	for i, r := range out.CORSRules {
		cr := data.CORSRule{
			AllowedMethods: append([]string(nil), r.AllowedMethods...),
			AllowedOrigins: append([]string(nil), r.AllowedOrigins...),
			AllowedHeaders: append([]string(nil), r.AllowedHeaders...),
			ExposeHeaders:  append([]string(nil), r.ExposeHeaders...),
		}
		if r.ID != nil {
			cr.ID = *r.ID
		}
		if r.MaxAgeSeconds != nil {
			cr.MaxAgeSeconds = int(*r.MaxAgeSeconds)
		}
		rules[i] = cr
	}
	return rules, nil
}

// DeleteBackendCORS clears the backend bucket's CORS configuration.
// Idempotent: NoSuchCORSConfiguration responses are treated as success.
func (b *Backend) DeleteBackendCORS(ctx context.Context) error {
	if b.client == nil {
		return errors.ErrUnsupported
	}
	bucket := b.bucket
	opCtx, cancel := b.opCtx(ctx)
	defer cancel()
	_, err := b.client.DeleteBucketCors(opCtx, &awss3.DeleteBucketCorsInput{Bucket: &bucket})
	if err != nil {
		if isNoSuchCORS(err) {
			return nil
		}
		return fmt.Errorf("s3: delete backend cors %s: %w", bucket, err)
	}
	return nil
}

// isNoSuchCORS recognises the backend's "no CORS configured" response.
// The SDK does not surface a typed error for NoSuchCORSConfiguration —
// match on the error string emitted by the smithy generic API error path,
// same convention as the lifecycle backend's NoSuchLifecycleConfiguration.
func isNoSuchCORS(err error) bool {
	return strings.Contains(err.Error(), "NoSuchCORSConfiguration")
}

// Compile-time assertion that *Backend satisfies data.CORSBackend.
var _ data.CORSBackend = (*Backend)(nil)
