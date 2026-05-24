//go:build integration

package kms_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awskms "github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/danchupin/strata/internal/crypto/kms"
)

// TestAWSKMSProviderAgainstLocalStack spins up LocalStack with the KMS
// service enabled, creates a CMK, and exercises the full
// GenerateDataKey → UnwrapDEK round-trip via the real
// *awskms.Client. The image tag is pinned to localstack/localstack:3.x
// per the PRD acceptance criterion (latest is unstable).
//
// Runs only under `go test -tags integration`.
func TestAWSKMSProviderAgainstLocalStack(t *testing.T) {
	ctx := context.Background()

	const port = "4566"
	req := testcontainers.ContainerRequest{
		Image:        "localstack/localstack:3.8",
		ExposedPorts: []string{port + "/tcp"},
		Env: map[string]string{
			"SERVICES": "kms",
		},
		WaitingFor: wait.ForHTTP("/_localstack/health").
			WithPort(port + "/tcp").
			WithStartupTimeout(90 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start localstack: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })

	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	mapped, err := c.MappedPort(ctx, port)
	if err != nil {
		t.Fatalf("mapped port: %v", err)
	}
	endpoint := fmt.Sprintf("http://%s:%s", host, mapped.Port())

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		t.Fatalf("aws cfg: %v", err)
	}
	client := awskms.NewFromConfig(cfg, func(o *awskms.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	})

	// CreateKey + record the key id.
	createOut, err := client.CreateKey(ctx, &awskms.CreateKeyInput{
		Description: aws.String("strata per-bucket signing key test"),
		KeySpec:     kmstypes.KeySpecSymmetricDefault,
		KeyUsage:    kmstypes.KeyUsageTypeEncryptDecrypt,
	})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	keyID := aws.ToString(createOut.KeyMetadata.KeyId)
	if keyID == "" {
		t.Fatalf("CreateKey returned empty key id: %+v", createOut.KeyMetadata)
	}

	provider, err := kms.NewAWSKMSProvider(client, "us-east-1")
	if err != nil {
		t.Fatalf("NewAWSKMSProvider: %v", err)
	}

	dek, wrapped, err := provider.GenerateDataKey(ctx, keyID)
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	if len(dek) != 32 {
		t.Fatalf("DEK plaintext: got %d bytes, want 32", len(dek))
	}
	if len(wrapped) == 0 {
		t.Fatalf("wrapped DEK: empty")
	}

	round, err := provider.UnwrapDEK(ctx, keyID, wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if string(round) != string(dek) {
		t.Fatalf("UnwrapDEK roundtrip mismatch: %x vs %x", round, dek)
	}

	// Wrong key id → must surface as ErrKeyIDMismatch on a real KMS too.
	other, err := client.CreateKey(ctx, &awskms.CreateKeyInput{
		KeySpec:  kmstypes.KeySpecSymmetricDefault,
		KeyUsage: kmstypes.KeyUsageTypeEncryptDecrypt,
	})
	if err != nil {
		t.Fatalf("second CreateKey: %v", err)
	}
	otherID := aws.ToString(other.KeyMetadata.KeyId)
	if _, err := provider.UnwrapDEK(ctx, otherID, wrapped); err == nil {
		t.Fatalf("UnwrapDEK with mismatched key id: expected error, got nil")
	}
}
