package s3

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/data"
)

// TestPutChunksPlacementRoutesPerPolicy pins the US-002 placement-rebalance
// contract: when ctx carries a placement policy, PutChunks routes the
// upload to the cluster picked by placement.PickCluster (stable hash-mod
// over (bucketID, key, chunkIdx=0)). 1000 keys with {a:1, b:1} split
// ~50/50; per-key result is deterministic across retries.
func TestPutChunksPlacementRoutesPerPolicy(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "ak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sk")

	a := newCapturingS3Server(t)
	b := newCapturingS3Server(t)
	t.Cleanup(a.Close)
	t.Cleanup(b.Close)

	cfg := Config{
		Clusters: map[string]S3ClusterSpec{
			"a": {Endpoint: a.URL(), Region: "us-east-1", ForcePathStyle: true, Credentials: CredentialsRef{Type: CredentialsChain}},
			"b": {Endpoint: b.URL(), Region: "us-west-1", ForcePathStyle: true, Credentials: CredentialsRef{Type: CredentialsChain}},
		},
		Classes: map[string]ClassSpec{
			"STANDARD":   {Cluster: "a", Bucket: "bucket-a"},
			"STANDARD-B": {Cluster: "b", Bucket: "bucket-b"},
		},
		SkipCredsCheck: true,
	}
	be, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	bucketID := uuid.New()
	policy := map[string]int{"a": 1, "b": 1}

	a.reset()
	b.reset()
	for i := range 1000 {
		key := keyN(i)
		ctx := data.WithPlacement(data.WithObjectKey(data.WithBucketID(context.Background(), bucketID), key), policy)
		if _, err := be.PutChunks(ctx, strings.NewReader("payload"), "STANDARD"); err != nil {
			t.Fatalf("PutChunks iter %d: %v", i, err)
		}
	}

	// Distribution: 50/50 within 10% (sample noise around 1000).
	total := a.requestCount() + b.requestCount()
	if total != 1000 {
		t.Fatalf("total requests: want 1000, got %d (a=%d, b=%d)", total, a.requestCount(), b.requestCount())
	}
	for cluster, n := range map[string]int{"a": a.requestCount(), "b": b.requestCount()} {
		if n < 400 || n > 600 {
			t.Fatalf("cluster %s placement skew: %d/1000 not in [400,600]", cluster, n)
		}
	}
}

// TestPutChunksPlacementDeterministic pins the per-key stability rail:
// the same (bucketID, key) hashes to the same cluster across retries.
func TestPutChunksPlacementDeterministic(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "ak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sk")

	a := newCapturingS3Server(t)
	bsrv := newCapturingS3Server(t)
	t.Cleanup(a.Close)
	t.Cleanup(bsrv.Close)

	cfg := Config{
		Clusters: map[string]S3ClusterSpec{
			"a": {Endpoint: a.URL(), Region: "us-east-1", ForcePathStyle: true, Credentials: CredentialsRef{Type: CredentialsChain}},
			"b": {Endpoint: bsrv.URL(), Region: "us-west-1", ForcePathStyle: true, Credentials: CredentialsRef{Type: CredentialsChain}},
		},
		Classes: map[string]ClassSpec{
			"STANDARD":   {Cluster: "a", Bucket: "bucket-a"},
			"STANDARD-B": {Cluster: "b", Bucket: "bucket-b"},
		},
		SkipCredsCheck: true,
	}
	be, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	bucketID := uuid.New()
	policy := map[string]int{"a": 1, "b": 1}
	for i := range 50 {
		key := keyN(i)
		ctx := data.WithPlacement(data.WithObjectKey(data.WithBucketID(context.Background(), bucketID), key), policy)
		a.reset()
		bsrv.reset()
		_, err := be.PutChunks(ctx, strings.NewReader("p"), "STANDARD")
		if err != nil {
			t.Fatalf("PutChunks iter %d: %v", i, err)
		}
		firstHitA := a.requestCount() == 1
		// Replay the same call twice — must hit the same server.
		for r := range 3 {
			a.reset()
			bsrv.reset()
			if _, err := be.PutChunks(ctx, strings.NewReader("p"), "STANDARD"); err != nil {
				t.Fatalf("replay %d/%d: %v", i, r, err)
			}
			if (a.requestCount() == 1) != firstHitA {
				t.Fatalf("iter %d replay %d: non-deterministic placement", i, r)
			}
		}
	}
}

// TestPutChunksNilPlacementRoutesPerClass is the backwards-compat rail:
// buckets with no Placement on ctx route exactly via the class config —
// regression against the existing single-cluster fixture path.
func TestPutChunksNilPlacementRoutesPerClass(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "ak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sk")

	a := newCapturingS3Server(t)
	bsrv := newCapturingS3Server(t)
	t.Cleanup(a.Close)
	t.Cleanup(bsrv.Close)

	cfg := Config{
		Clusters: map[string]S3ClusterSpec{
			"a": {Endpoint: a.URL(), Region: "us-east-1", ForcePathStyle: true, Credentials: CredentialsRef{Type: CredentialsChain}},
			"b": {Endpoint: bsrv.URL(), Region: "us-west-1", ForcePathStyle: true, Credentials: CredentialsRef{Type: CredentialsChain}},
		},
		Classes: map[string]ClassSpec{
			"STANDARD": {Cluster: "a", Bucket: "bucket-a"},
			"COLD":     {Cluster: "b", Bucket: "bucket-b"},
		},
		SkipCredsCheck: true,
	}
	be, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// No placement on ctx — STANDARD must hit `a`, COLD must hit `b`.
	a.reset()
	bsrv.reset()
	if _, err := be.PutChunks(context.Background(), strings.NewReader("x"), "STANDARD"); err != nil {
		t.Fatalf("PutChunks STANDARD: %v", err)
	}
	if a.requestCount() != 1 || bsrv.requestCount() != 0 {
		t.Fatalf("STANDARD without placement: want (a=1, b=0), got (a=%d, b=%d)", a.requestCount(), bsrv.requestCount())
	}
	a.reset()
	bsrv.reset()
	if _, err := be.PutChunks(context.Background(), strings.NewReader("y"), "COLD"); err != nil {
		t.Fatalf("PutChunks COLD: %v", err)
	}
	if a.requestCount() != 0 || bsrv.requestCount() != 1 {
		t.Fatalf("COLD without placement: want (a=0, b=1), got (a=%d, b=%d)", a.requestCount(), bsrv.requestCount())
	}
}

// TestPutChunksPlacementAllZeroFallsBack pins the AC: a policy with all
// weights zero returns "" from the picker, and the caller falls back
// to the class-default routing.
func TestPutChunksPlacementAllZeroFallsBack(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "ak")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "sk")

	a := newCapturingS3Server(t)
	bsrv := newCapturingS3Server(t)
	t.Cleanup(a.Close)
	t.Cleanup(bsrv.Close)

	cfg := Config{
		Clusters: map[string]S3ClusterSpec{
			"a": {Endpoint: a.URL(), Region: "us-east-1", ForcePathStyle: true, Credentials: CredentialsRef{Type: CredentialsChain}},
			"b": {Endpoint: bsrv.URL(), Region: "us-west-1", ForcePathStyle: true, Credentials: CredentialsRef{Type: CredentialsChain}},
		},
		Classes: map[string]ClassSpec{
			"STANDARD":   {Cluster: "a", Bucket: "bucket-a"},
			"STANDARD-B": {Cluster: "b", Bucket: "bucket-b"},
		},
		SkipCredsCheck: true,
	}
	be, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	policy := map[string]int{"a": 0, "b": 0}
	bucketID := uuid.New()
	ctx := data.WithPlacement(data.WithObjectKey(data.WithBucketID(context.Background(), bucketID), "key"), policy)
	a.reset()
	bsrv.reset()
	if _, err := be.PutChunks(ctx, strings.NewReader("x"), "STANDARD"); err != nil {
		t.Fatalf("PutChunks: %v", err)
	}
	if a.requestCount() != 1 || bsrv.requestCount() != 0 {
		t.Fatalf("all-zero policy: must fall back to STANDARD/a, got (a=%d, b=%d)", a.requestCount(), bsrv.requestCount())
	}
}

func keyN(i int) string {
	return "object-key-" + uuid.NewString() + "-" + itoa(i)
}

func itoa(i int) string {
	// Hot-path-free local int->string; importing strconv would also work
	// but this keeps the imports list shorter.
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [16]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
