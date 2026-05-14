package s3

import (
	"context"
	"errors"
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

// TestPutChunksRefusesDrainFallback pins the US-007 always-strict
// contract: when the placement picker falls back to a draining cluster
// (empty policy or all-excluded policy), PutChunks returns
// data.ErrDrainRefused with the resolved cluster id; nothing is written
// to either backend. No env / Config gate — drain is unconditionally
// strict.
func TestPutChunksRefusesDrainFallback(t *testing.T) {
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

	// No placement on ctx — fallback to STANDARD/a. Cluster "a" drained.
	bucketID := uuid.New()
	ctx := data.WithDrainingClusters(
		data.WithObjectKey(data.WithBucketID(context.Background(), bucketID), "key"),
		map[string]bool{"a": true},
	)
	a.reset()
	bsrv.reset()
	_, err = be.PutChunks(ctx, strings.NewReader("payload"), "STANDARD")
	if err == nil {
		t.Fatal("PutChunks: want ErrDrainRefused, got nil")
	}
	if !errors.Is(err, data.ErrDrainRefused) {
		t.Fatalf("PutChunks err: want ErrDrainRefused, got %v", err)
	}
	var dre *data.DrainRefusedError
	if !errors.As(err, &dre) {
		t.Fatalf("errors.As DrainRefusedError: false; err=%v", err)
	}
	if dre.Cluster != "a" {
		t.Fatalf("DrainRefusedError.Cluster: want %q, got %q", "a", dre.Cluster)
	}
	if a.requestCount() != 0 || bsrv.requestCount() != 0 {
		t.Fatalf("strict refusal must not write: got (a=%d, b=%d)", a.requestCount(), bsrv.requestCount())
	}

	// All-excluded policy {a:1} + a drained — also refuses.
	ctx2 := data.WithPlacement(ctx, map[string]int{"a": 1})
	a.reset()
	bsrv.reset()
	_, err = be.PutChunks(ctx2, strings.NewReader("payload"), "STANDARD")
	if !errors.Is(err, data.ErrDrainRefused) {
		t.Fatalf("all-excluded policy: want ErrDrainRefused, got %v", err)
	}
	if a.requestCount() != 0 || bsrv.requestCount() != 0 {
		t.Fatalf("strict refusal (all-excluded) must not write: got (a=%d, b=%d)", a.requestCount(), bsrv.requestCount())
	}
}

// TestPutChunksDoesNotRefuseAlternateCluster pins the AC that the
// always-strict refusal fires ONLY when fallback hits a draining
// cluster — if the policy picks a non-draining peer the PUT succeeds.
func TestPutChunksDoesNotRefuseAlternateCluster(t *testing.T) {
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

	// Policy {a:1, b:1} with a drained — picker excludes a and routes to b.
	bucketID := uuid.New()
	ctx := data.WithPlacement(
		data.WithDrainingClusters(
			data.WithObjectKey(data.WithBucketID(context.Background(), bucketID), "key"),
			map[string]bool{"a": true},
		),
		map[string]int{"a": 1, "b": 1},
	)
	a.reset()
	bsrv.reset()
	if _, err := be.PutChunks(ctx, strings.NewReader("payload"), "STANDARD"); err != nil {
		t.Fatalf("PutChunks: want nil, got %v", err)
	}
	if a.requestCount() != 0 || bsrv.requestCount() != 1 {
		t.Fatalf("policy with drain exclusion: want write to b, got (a=%d, b=%d)", a.requestCount(), bsrv.requestCount())
	}
}

// TestPutChunksDefaultPolicyRoutesWhenBucketPolicyNil pins the US-002
// cluster-weights contract: when ctx carries data.WithDefaultPlacement
// (synthesised from cluster.weight) AND bucket Placement is nil, the
// picker routes via the default policy. Distribution ~50/50 over 1000
// keys on {a:1, b:1}.
func TestPutChunksDefaultPolicyRoutesWhenBucketPolicyNil(t *testing.T) {
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
	defaultPolicy := map[string]int{"a": 1, "b": 1}
	for i := range 1000 {
		key := keyN(i)
		ctx := data.WithDefaultPlacement(
			data.WithObjectKey(data.WithBucketID(context.Background(), bucketID), key),
			defaultPolicy,
		)
		if _, err := be.PutChunks(ctx, strings.NewReader("payload"), "STANDARD"); err != nil {
			t.Fatalf("PutChunks iter %d: %v", i, err)
		}
	}
	for cluster, n := range map[string]int{"a": a.requestCount(), "b": bsrv.requestCount()} {
		if n < 400 || n > 600 {
			t.Fatalf("default policy skew: cluster %s got %d/1000 not in [400,600]", cluster, n)
		}
	}
}

// TestPutChunksBucketPolicyWinsOverDefault pins the third AC: when both
// data.WithPlacement (bucket Placement) AND data.WithDefaultPlacement
// (cluster.weight synthesis) are on ctx, the bucket policy wins —
// cluster.weight is IGNORED for that bucket.
//
// bucket={a:1, b:1} (50/50) + default={a:100, b:0} (would be all on a)
// → 1000 keys split ~500/500, NOT 1000/0.
func TestPutChunksBucketPolicyWinsOverDefault(t *testing.T) {
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
	bucketPolicy := map[string]int{"a": 1, "b": 1}
	defaultPolicy := map[string]int{"a": 100, "b": 0}
	for i := range 1000 {
		key := keyN(i)
		ctx := data.WithDefaultPlacement(
			data.WithPlacement(
				data.WithObjectKey(data.WithBucketID(context.Background(), bucketID), key),
				bucketPolicy,
			),
			defaultPolicy,
		)
		if _, err := be.PutChunks(ctx, strings.NewReader("payload"), "STANDARD"); err != nil {
			t.Fatalf("PutChunks iter %d: %v", i, err)
		}
	}
	for cluster, n := range map[string]int{"a": a.requestCount(), "b": bsrv.requestCount()} {
		if n < 400 || n > 600 {
			t.Fatalf("bucket policy must win over default: cluster %s got %d/1000 not in [400,600]", cluster, n)
		}
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
