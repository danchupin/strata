package s3

import (
	"context"
	"fmt"
	"strings"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/danchupin/strata/internal/data"
)

// PutBackendCORS pushes the supplied CORS rules onto the backend bucket
// via the SDK PutBucketCors call (US-015). Empty rules clear the backend
// configuration via DeleteBackendCORS.
func (b *Backend) PutBackendCORS(ctx context.Context, rules []data.CORSRule) error {
	if len(rules) == 0 {
		return b.DeleteBackendCORS(ctx)
	}
	c, bucket, err := b.singleCluster(ctx)
	if err != nil {
		return err
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

	opCtx, cancel := opCtxFor(ctx, c.opTimeout)
	defer cancel()
	_, err = c.client.PutBucketCors(opCtx, &awss3.PutBucketCorsInput{
		Bucket:            &bucket,
		CORSConfiguration: &s3types.CORSConfiguration{CORSRules: sdkRules},
	})
	if err != nil {
		return fmt.Errorf("s3: put backend cors %s: %w", bucket, err)
	}
	return nil
}

// GetBackendCORS reads the backend bucket's CORS configuration.
// NoSuchCORSConfiguration is treated as "no rules configured" — returns
// (nil, nil).
func (b *Backend) GetBackendCORS(ctx context.Context) ([]data.CORSRule, error) {
	c, bucket, err := b.singleCluster(ctx)
	if err != nil {
		return nil, err
	}
	opCtx, cancel := opCtxFor(ctx, c.opTimeout)
	defer cancel()
	out, err := c.client.GetBucketCors(opCtx, &awss3.GetBucketCorsInput{Bucket: &bucket})
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
// Idempotent: NoSuchCORSConfiguration is treated as success.
func (b *Backend) DeleteBackendCORS(ctx context.Context) error {
	c, bucket, err := b.singleCluster(ctx)
	if err != nil {
		return err
	}
	opCtx, cancel := opCtxFor(ctx, c.opTimeout)
	defer cancel()
	_, err = c.client.DeleteBucketCors(opCtx, &awss3.DeleteBucketCorsInput{Bucket: &bucket})
	if err != nil {
		if isNoSuchCORS(err) {
			return nil
		}
		return fmt.Errorf("s3: delete backend cors %s: %w", bucket, err)
	}
	return nil
}

func isNoSuchCORS(err error) bool {
	return strings.Contains(err.Error(), "NoSuchCORSConfiguration")
}

// Compile-time assertion that *Backend satisfies data.CORSBackend.
var _ data.CORSBackend = (*Backend)(nil)
