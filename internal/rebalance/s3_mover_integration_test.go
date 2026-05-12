//go:build integration

package rebalance_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"

	"github.com/danchupin/strata/internal/data"
	s3backend "github.com/danchupin/strata/internal/data/s3"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/rebalance"
)

// TestRebalanceS3TwoClusters spins two minio testcontainers, builds an
// s3.Backend with both as operator-labelled clusters, plants 100
// chunks on c1, sets Placement {c1:0, c2:1}, runs an S3Mover.Move for
// every object, and asserts the chunks now live on c2, the manifests
// reference the new locator, and the old keys are queued in the GC
// table.
//
// Tagged `integration`; pulled into CI by `go test -tags integration`.
func TestRebalanceS3TwoClusters(t *testing.T) {
	ctx := context.Background()
	const (
		username = "minioadmin"
		password = "minioadmin"
	)

	c1Container, c1Endpoint := startMinio(t, ctx, username, password)
	t.Cleanup(func() { _ = c1Container.Terminate(context.Background()) })
	c2Container, c2Endpoint := startMinio(t, ctx, username, password)
	t.Cleanup(func() { _ = c2Container.Terminate(context.Background()) })

	const (
		bucketC1 = "rb-c1"
		bucketC2 = "rb-c2"
	)
	if err := createBucket(ctx, c1Endpoint, username, password, bucketC1); err != nil {
		t.Fatalf("create bucket c1: %v", err)
	}
	if err := createBucket(ctx, c2Endpoint, username, password, bucketC2); err != nil {
		t.Fatalf("create bucket c2: %v", err)
	}

	t.Setenv("AWS_ACCESS_KEY_ID", username)
	t.Setenv("AWS_SECRET_ACCESS_KEY", password)

	cfg := s3backend.Config{
		Clusters: map[string]s3backend.S3ClusterSpec{
			"c1": {ID: "c1", Endpoint: c1Endpoint, Region: "us-east-1", ForcePathStyle: true, Credentials: s3backend.CredentialsRef{Type: s3backend.CredentialsChain}},
			"c2": {ID: "c2", Endpoint: c2Endpoint, Region: "us-west-1", ForcePathStyle: true, Credentials: s3backend.CredentialsRef{Type: s3backend.CredentialsChain}},
		},
		Classes: map[string]s3backend.ClassSpec{
			"STANDARD":   {Cluster: "c1", Bucket: bucketC1},
			"STANDARD-B": {Cluster: "c2", Bucket: bucketC2},
		},
		SkipCredsCheck: true,
		SkipProbe:      true,
	}
	be, err := s3backend.New(cfg)
	if err != nil {
		t.Fatalf("s3.New: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })

	m := metamem.New()
	b, err := m.CreateBucket(ctx, "s3-rb", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	// Plant 25 objects on c1 via direct PUT; record planned moves.
	const objects = 25
	plan := make([]rebalance.Move, 0, objects)
	for i := 0; i < objects; i++ {
		body := make([]byte, 4096)
		if _, err := rand.Read(body); err != nil {
			t.Fatal(err)
		}
		key := fmt.Sprintf("k-%s", uuid.NewString())
		backendKey := uuid.NewString() + "/" + uuid.NewString()
		if err := putObject(ctx, c1Endpoint, username, password, bucketC1, backendKey, body); err != nil {
			t.Fatalf("plant object %d: %v", i, err)
		}
		if err := m.PutObject(ctx, &meta.Object{
			BucketID:     b.ID,
			Key:          key,
			Size:         int64(len(body)),
			ETag:         "deadbeef",
			StorageClass: "STANDARD",
			Mtime:        time.Now().UTC(),
			IsLatest:     true,
			Manifest: &data.Manifest{
				Class: "STANDARD",
				Size:  int64(len(body)),
				BackendRef: &data.BackendRef{
					Backend: "s3",
					Key:     backendKey,
					Size:    int64(len(body)),
					Cluster: "c1",
				},
			},
		}, false); err != nil {
			t.Fatalf("PutObject %d: %v", i, err)
		}
		plan = append(plan, rebalance.Move{
			Bucket:      b.Name,
			BucketID:    b.ID,
			ObjectKey:   key,
			ChunkIdx:    0,
			FromCluster: "c1",
			ToCluster:   "c2",
			SrcRef:      data.ChunkRef{Cluster: "c1", OID: backendKey, Size: int64(len(body))},
			Class:       "STANDARD",
		})
	}

	mover := &rebalance.S3Mover{
		Clusters: be.RebalanceClusters(),
		BucketBy: be.BucketOnCluster,
		Meta:     m,
		Region:   "default",
		Logger:   slog.Default(),
		Inflight: 4,
	}
	if err := mover.Move(ctx, plan); err != nil {
		t.Fatalf("Move: %v", err)
	}

	// Manifests now reference c2 and the new key on bucket-c2.
	res, err := m.ListObjects(ctx, b.ID, meta.ListOptions{Limit: objects})
	if err != nil {
		t.Fatalf("ListObjects: %v", err)
	}
	for _, o := range res.Objects {
		if got := o.Manifest.BackendRef.Cluster; got != "c2" {
			t.Errorf("object %s: backend cluster got %q want c2", o.Key, got)
		}
		// Verify the body lives on c2's bucket.
		body, err := getObject(ctx, c2Endpoint, username, password, bucketC2, o.Manifest.BackendRef.Key)
		if err != nil {
			t.Errorf("verify get %s on c2: %v", o.Key, err)
			continue
		}
		if len(body) == 0 {
			t.Errorf("verify get %s on c2: empty body", o.Key)
		}
	}

	// GC queue now holds `objects` entries, each pointing at c1 + the
	// source bucket.
	entries, err := m.ListGCEntries(ctx, "default", time.Now().Add(time.Hour), objects*2)
	if err != nil {
		t.Fatalf("ListGCEntries: %v", err)
	}
	if len(entries) != objects {
		t.Errorf("gc entries: got %d want %d", len(entries), objects)
	}
	for _, e := range entries {
		if e.Chunk.Cluster != "c1" {
			t.Errorf("gc entry cluster: got %q want c1", e.Chunk.Cluster)
		}
		if e.Chunk.Pool != bucketC1 {
			t.Errorf("gc entry pool: got %q want %q", e.Chunk.Pool, bucketC1)
		}
	}
}

func startMinio(t *testing.T, ctx context.Context, ak, sk string) (*tcminio.MinioContainer, string) {
	t.Helper()
	container, err := tcminio.Run(ctx, "minio/minio:latest",
		tcminio.WithUsername(ak),
		tcminio.WithPassword(sk),
	)
	if err != nil {
		t.Fatalf("start minio: %v", err)
	}
	hostPort, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	endpoint := hostPort
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "http://" + endpoint
	}
	return container, endpoint
}

func adminClient(endpoint, ak, sk string) *awss3.Client {
	awscfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(ak, sk, "")),
	)
	if err != nil {
		panic(err)
	}
	return awss3.NewFromConfig(awscfg, func(o *awss3.Options) {
		ep := endpoint
		o.BaseEndpoint = &ep
		o.UsePathStyle = true
	})
}

func createBucket(ctx context.Context, endpoint, ak, sk, bucket string) error {
	client := adminClient(endpoint, ak, sk)
	_, err := client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: &bucket})
	return err
}

func putObject(ctx context.Context, endpoint, ak, sk, bucket, key string, body []byte) error {
	client := adminClient(endpoint, ak, sk)
	_, err := client.PutObject(ctx, &awss3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   bytes.NewReader(body),
	})
	return err
}

func getObject(ctx context.Context, endpoint, ak, sk, bucket, key string) ([]byte, error) {
	client := adminClient(endpoint, ak, sk)
	out, err := client.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}
