package kms

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awskms "github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
)

const (
	// EnvAWSKMSRegion enables the AWS KMS provider via FromEnv. The cmd binary
	// supplies a KMSAPI client factory via WithAWSKMSClientFactory; without the
	// factory, FromEnv refuses to surface aws-kms so misconfiguration is caught
	// at startup.
	EnvAWSKMSRegion = "STRATA_KMS_AWS_REGION"
)

// KMSAPI is the subset of *awskms.Client used by AWSKMSProvider. Defining it
// as an interface lets unit tests swap a fake without compiling the real SDK
// transport. cmd/strata-gateway wires a real *awskms.Client via the standard
// AWS credential chain (env vars, shared config, IRSA, EC2/ECS roles).
type KMSAPI interface {
	GenerateDataKey(ctx context.Context, params *awskms.GenerateDataKeyInput, optFns ...func(*awskms.Options)) (*awskms.GenerateDataKeyOutput, error)
	Decrypt(ctx context.Context, params *awskms.DecryptInput, optFns ...func(*awskms.Options)) (*awskms.DecryptOutput, error)
}

// AWSKMSProvider implements Provider against AWS KMS via the SDK. KeyID flows
// through to the per-call request (matching the AWS SDK semantics where one
// client can serve any number of CMKs).
type AWSKMSProvider struct {
	Client KMSAPI
	Region string
}

// NewAWSKMSProvider builds a provider over the supplied KMSAPI. Callers
// pass a fully constructed SDK client (or a fake in tests).
func NewAWSKMSProvider(client KMSAPI, region string) (*AWSKMSProvider, error) {
	if client == nil {
		return nil, errors.New("kms aws: client required")
	}
	return &AWSKMSProvider{Client: client, Region: region}, nil
}

// GenerateDataKey calls KMS GenerateDataKey with KeySpec=AES_256 and returns
// the plaintext + the wrapped CiphertextBlob (opaque).
func (p *AWSKMSProvider) GenerateDataKey(ctx context.Context, keyID string) ([]byte, []byte, error) {
	if keyID == "" {
		return nil, nil, ErrMissingKeyID
	}
	out, err := p.Client.GenerateDataKey(ctx, &awskms.GenerateDataKeyInput{
		KeyId:   aws.String(keyID),
		KeySpec: kmstypes.DataKeySpecAes256,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("kms aws GenerateDataKey: %w", err)
	}
	if len(out.Plaintext) != DEKSize {
		return nil, nil, fmt.Errorf("kms aws: plaintext %d bytes, want %d", len(out.Plaintext), DEKSize)
	}
	if len(out.CiphertextBlob) == 0 {
		return nil, nil, errors.New("kms aws: empty CiphertextBlob")
	}
	return out.Plaintext, out.CiphertextBlob, nil
}

// UnwrapDEK calls KMS Decrypt and verifies that the KMS-reported KeyId matches
// the requested keyID. Mismatches surface as an explicit error so the gateway
// can map them to AccessDenied.
func (p *AWSKMSProvider) UnwrapDEK(ctx context.Context, keyID string, wrapped []byte) ([]byte, error) {
	if keyID == "" {
		return nil, ErrMissingKeyID
	}
	if len(wrapped) == 0 {
		return nil, errors.New("kms aws: empty wrapped DEK")
	}
	out, err := p.Client.Decrypt(ctx, &awskms.DecryptInput{
		CiphertextBlob: wrapped,
		KeyId:          aws.String(keyID),
	})
	if err != nil {
		return nil, fmt.Errorf("kms aws Decrypt: %w", err)
	}
	if out.KeyId != nil && !sameKeyARN(*out.KeyId, keyID) {
		return nil, fmt.Errorf("%w: ciphertext was wrapped under %s", ErrKeyIDMismatch, *out.KeyId)
	}
	if len(out.Plaintext) == 0 {
		return nil, errors.New("kms aws Decrypt: empty plaintext")
	}
	return out.Plaintext, nil
}

// ErrKeyIDMismatch is returned by UnwrapDEK when the KMS-reported key id does
// not match the caller-supplied id. The gateway maps this to AccessDenied.
var ErrKeyIDMismatch = errors.New("kms: wrapped DEK key id mismatch")

// sameKeyARN treats an alias name, key id, or full ARN as matching the same
// canonical key when one is the suffix of the other. AWS KMS Decrypt returns
// the ARN ("arn:aws:kms:<region>:<acct>:key/<id>"); the caller may pass the
// bare id, an alias, or the ARN itself.
func sameKeyARN(reported, requested string) bool {
	if reported == requested {
		return true
	}
	if strings.HasSuffix(reported, "/"+requested) {
		return true
	}
	if strings.HasSuffix(requested, "/"+reported) {
		return true
	}
	return false
}

// NewAWSKMSProviderFromEnv reads STRATA_KMS_AWS_REGION and returns ErrNoConfig
// when unset. The factory is required: it builds a real *awskms.Client via the
// standard AWS SDK credential chain (env vars, shared config, IRSA, EC2/ECS
// roles); cmd/strata-gateway supplies it.
func NewAWSKMSProviderFromEnv(factory func(region string) (KMSAPI, error)) (*AWSKMSProvider, error) {
	region := strings.TrimSpace(os.Getenv(EnvAWSKMSRegion))
	if region == "" {
		return nil, ErrNoConfig
	}
	if factory == nil {
		return nil, errors.New("kms aws: STRATA_KMS_AWS_REGION set but no client factory configured")
	}
	client, err := factory(region)
	if err != nil {
		return nil, fmt.Errorf("kms aws: client init: %w", err)
	}
	return NewAWSKMSProvider(client, region)
}
