//go:build integration

package kms_test

import (
	"context"
	"errors"
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

// TestAWSKMSProviderRotationCycle exercises the full US-002 rotation
// flow against LocalStack: mint CMK_A, GenerateDataKey under CMK_A,
// stash wrapped_A → simulate rotation by minting CMK_B and re-generating
// → assert old wrapped_A still decrypts under CMK_A (rotation does NOT
// invalidate prior wrappings) AND wrapped_B fails to decrypt under
// CMK_A via ErrKeyIDMismatch (no cross-CMK unwrap).
func TestAWSKMSProviderRotationCycle(t *testing.T) {
	ctx := context.Background()

	const port = "4566"
	req := testcontainers.ContainerRequest{
		Image:        "localstack/localstack:3.8",
		ExposedPorts: []string{port + "/tcp"},
		Env:          map[string]string{"SERVICES": "kms"},
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

	createKey := func(desc string) string {
		out, err := client.CreateKey(ctx, &awskms.CreateKeyInput{
			Description: aws.String(desc),
			KeySpec:     kmstypes.KeySpecSymmetricDefault,
			KeyUsage:    kmstypes.KeyUsageTypeEncryptDecrypt,
		})
		if err != nil {
			t.Fatalf("CreateKey %q: %v", desc, err)
		}
		return aws.ToString(out.KeyMetadata.KeyId)
	}

	keyA := createKey("strata signing key A (pre-rotation)")
	keyB := createKey("strata signing key B (post-rotation)")
	provider, err := kms.NewAWSKMSProvider(client, "us-east-1")
	if err != nil {
		t.Fatalf("NewAWSKMSProvider: %v", err)
	}

	dekA, wrappedA, err := provider.GenerateDataKey(ctx, keyA)
	if err != nil {
		t.Fatalf("GenerateDataKey A: %v", err)
	}
	dekB, wrappedB, err := provider.GenerateDataKey(ctx, keyB)
	if err != nil {
		t.Fatalf("GenerateDataKey B: %v", err)
	}
	if string(dekA) == string(dekB) {
		t.Fatalf("fresh rotations produced identical DEKs")
	}

	// Pre-rotation envelope must still decrypt under its original CMK.
	roundA, err := provider.UnwrapDEK(ctx, keyA, wrappedA)
	if err != nil {
		t.Fatalf("UnwrapDEK A: %v", err)
	}
	if string(roundA) != string(dekA) {
		t.Fatalf("pre-rotation DEK mismatch")
	}

	// Post-rotation envelope must decrypt under its new CMK.
	roundB, err := provider.UnwrapDEK(ctx, keyB, wrappedB)
	if err != nil {
		t.Fatalf("UnwrapDEK B: %v", err)
	}
	if string(roundB) != string(dekB) {
		t.Fatalf("post-rotation DEK mismatch")
	}

	// Cross-CMK unwrap MUST refuse — operator cannot accidentally
	// authenticate against rotated material under the wrong key id.
	if _, err := provider.UnwrapDEK(ctx, keyA, wrappedB); err == nil || !errors.Is(err, kms.ErrKeyIDMismatch) {
		t.Fatalf("UnwrapDEK wrappedB under keyA: want ErrKeyIDMismatch, got %v", err)
	}
}
