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
	"github.com/testcontainers/testcontainers-go"
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

// TestRebalanceS3ConcurrentClientWrites proves the documented mover CAS
// invariant under racing client writes (US-010). For half the objects a
// client PUT rewrites the manifest's BackendRef.Key on the SAME class +
// source cluster between plan emission and the mover's CAS — exactly the
// lost-update race. Each such object MUST keep the client's locator (the
// mover loses the CAS via buildUpdatedBackendManifest's locator mismatch)
// and the mover's freshly-copied target object MUST be queued in the GC
// table — never leaked, never silently overwriting the client write. The
// untouched half moves normally: manifest → c2, body readable on c2, old
// c1 key in GC. The whole population must resolve to exactly one live
// locator per object with every loser's bytes reclaimed.
//
// This does NOT duplicate TestRebalanceS3TwoClusters (the single-pass,
// no-contention move): the new dimension is the concurrent client write
// and the CAS-loser-to-GC reclaim.
func TestRebalanceS3ConcurrentClientWrites(t *testing.T) {
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
		bucketC1 = "rc-c1"
		bucketC2 = "rc-c2"
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
	b, err := m.CreateBucket(ctx, "s3-rc", "owner", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}

	// planted records one object's plan + race intent so the assertions
	// can branch on who is expected to win the CAS.
	type planted struct {
		key       string // Strata object key
		srcKey    string // original c1 backend key == plan SrcRef.OID
		raced     bool   // client rewrote the manifest before the mover
		clientKey string // c1 backend key the client rewrote to (raced only)
	}

	const objects = 20 // even split: 10 raced, 10 clean
	ps := make([]planted, 0, objects)
	plan := make([]rebalance.Move, 0, objects)
	for i := 0; i < objects; i++ {
		body := make([]byte, 4096)
		if _, err := rand.Read(body); err != nil {
			t.Fatal(err)
		}
		key := fmt.Sprintf("k-%s", uuid.NewString())
		srcKey := uuid.NewString() + "/" + uuid.NewString()
		if err := putObject(ctx, c1Endpoint, username, password, bucketC1, srcKey, body); err != nil {
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
					Key:     srcKey,
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
			SrcRef:      data.ChunkRef{Cluster: "c1", OID: srcKey, Size: int64(len(body))},
			Class:       "STANDARD",
		})
		ps = append(ps, planted{key: key, srcKey: srcKey, raced: i%2 == 0})
	}

	// The racing client writes that already landed: a fresh PUT rewrites
	// the manifest BackendRef.Key on the same class + cluster c1. The
	// mover's GetObject now reads a locator that no longer matches the
	// plan's SrcRef.OID → buildUpdatedBackendManifest returns ok=false →
	// CAS conflict, the mover's c2 copy is discarded to GC, the client's
	// manifest survives untouched.
	for i := range ps {
		if !ps[i].raced {
			continue
		}
		clientKey := uuid.NewString() + "/" + uuid.NewString()
		clientBody := make([]byte, 4096)
		if _, err := rand.Read(clientBody); err != nil {
			t.Fatal(err)
		}
		if err := putObject(ctx, c1Endpoint, username, password, bucketC1, clientKey, clientBody); err != nil {
			t.Fatalf("client overwrite object %d: %v", i, err)
		}
		if err := m.PutObject(ctx, &meta.Object{
			BucketID:     b.ID,
			Key:          ps[i].key,
			Size:         int64(len(clientBody)),
			ETag:         "feedface",
			StorageClass: "STANDARD",
			Mtime:        time.Now().UTC(),
			IsLatest:     true,
			Manifest: &data.Manifest{
				Class: "STANDARD",
				Size:  int64(len(clientBody)),
				BackendRef: &data.BackendRef{
					Backend: "s3",
					Key:     clientKey,
					Size:    int64(len(clientBody)),
					Cluster: "c1",
				},
			},
		}, false); err != nil {
			t.Fatalf("client manifest overwrite %d: %v", i, err)
		}
		ps[i].clientKey = clientKey
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

	// Per-object: the winner's locator is read back, and the body is
	// readable on whichever cluster won.
	for _, p := range ps {
		obj, err := m.GetObject(ctx, b.ID, p.key, "")
		if err != nil {
			t.Fatalf("GetObject %s: %v", p.key, err)
		}
		if obj == nil || obj.Manifest == nil || obj.Manifest.BackendRef == nil {
			t.Fatalf("object %s: nil manifest/backendref", p.key)
		}
		br := obj.Manifest.BackendRef
		if p.raced {
			// Client won the CAS: manifest still points at c1 + clientKey.
			if br.Cluster != "c1" {
				t.Errorf("raced object %s: cluster got %q want c1 (client write must survive)", p.key, br.Cluster)
			}
			if br.Key != p.clientKey {
				t.Errorf("raced object %s: key got %q want client key %q", p.key, br.Key, p.clientKey)
			}
			body, err := getObject(ctx, c1Endpoint, username, password, bucketC1, br.Key)
			if err != nil {
				t.Errorf("raced object %s: read client body on c1: %v", p.key, err)
			} else if len(body) == 0 {
				t.Errorf("raced object %s: empty client body on c1", p.key)
			}
			continue
		}
		// Mover won the CAS: manifest points at c2, body readable on c2.
		if br.Cluster != "c2" {
			t.Errorf("clean object %s: cluster got %q want c2", p.key, br.Cluster)
		}
		body, err := getObject(ctx, c2Endpoint, username, password, bucketC2, br.Key)
		if err != nil {
			t.Errorf("clean object %s: read moved body on c2: %v", p.key, err)
		} else if len(body) == 0 {
			t.Errorf("clean object %s: empty moved body on c2", p.key)
		}
	}

	// GC reclaim accounting: exactly one entry per object — clean winners
	// queue the old c1 source key, raced losers queue the mover's discarded
	// c2 copy. No leak (every loser reclaimed), no double (one per object).
	entries, err := m.ListGCEntries(ctx, "default", time.Now().Add(time.Hour), objects*2)
	if err != nil {
		t.Fatalf("ListGCEntries: %v", err)
	}
	if len(entries) != objects {
		t.Errorf("gc entries: got %d want %d", len(entries), objects)
	}
	var c1Cleanup, c2Loser int
	for _, e := range entries {
		switch e.Chunk.Cluster {
		case "c1":
			c1Cleanup++
			if e.Chunk.Pool != bucketC1 {
				t.Errorf("c1 gc entry pool: got %q want %q", e.Chunk.Pool, bucketC1)
			}
		case "c2":
			c2Loser++
			if e.Chunk.Pool != bucketC2 {
				t.Errorf("c2 gc entry pool: got %q want %q", e.Chunk.Pool, bucketC2)
			}
		default:
			t.Errorf("unexpected gc entry cluster %q", e.Chunk.Cluster)
		}
	}
	if c1Cleanup != objects/2 {
		t.Errorf("clean-winner source GC entries: got %d want %d", c1Cleanup, objects/2)
	}
	if c2Loser != objects/2 {
		t.Errorf("raced-loser discarded GC entries: got %d want %d", c2Loser, objects/2)
	}
}

func startMinio(t *testing.T, ctx context.Context, ak, sk string) (*tcminio.MinioContainer, string) {
	t.Helper()
	container, err := tcminio.Run(ctx, "minio/minio:latest",
		tcminio.WithUsername(ak),
		tcminio.WithPassword(sk),
		// SSE-S3 (AES256) auto-encryption needs a KMS secret key on recent
		// minio images, else uploads 501 "KMS is not configured".
		testcontainers.WithEnv(map[string]string{"MINIO_KMS_SECRET_KEY": "strata-test-key:OSMM+vkKUTCvQs9YL/CVMIMt43HFhkUpqJxTmGl6rYw="}),
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
