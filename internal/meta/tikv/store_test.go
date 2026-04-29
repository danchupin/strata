package tikv

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/danchupin/strata/internal/meta"
)

// newTestStore returns a Store backed by an in-process memBackend so the
// surface tests do not need a TiKV testcontainer (US-013 lands the
// contract suite that exercises the real txnkv path).
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return openWithBackend(newMemBackend())
}

func TestProbeReady(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

func TestCreateBucket(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	b, err := s.CreateBucket(ctx, "bkt", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if b.Name != "bkt" || b.Owner != "alice" || b.DefaultClass != "STANDARD" {
		t.Fatalf("bucket fields: %+v", b)
	}
	if b.Versioning != meta.VersioningDisabled {
		t.Fatalf("versioning default: %q", b.Versioning)
	}
	if b.ShardCount != defaultShardCount {
		t.Fatalf("shard count default: %d", b.ShardCount)
	}
	if b.ID.String() == "" {
		t.Fatalf("bucket id empty")
	}
}

func TestCreateBucketDuplicate(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if _, err := s.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := s.CreateBucket(ctx, "bkt", "bob", "STANDARD")
	if !errors.Is(err, meta.ErrBucketAlreadyExists) {
		t.Fatalf("dup create: got %v, want ErrBucketAlreadyExists", err)
	}
}

func TestGetBucketRoundTrip(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	created, err := s.CreateBucket(ctx, "bkt", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.GetBucket(ctx, "bkt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("id mismatch: got %v want %v", got.ID, created.ID)
	}
	if got.Owner != "alice" || got.DefaultClass != "STANDARD" {
		t.Fatalf("fields: %+v", got)
	}
}

func TestGetBucketMissing(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	if _, err := s.GetBucket(context.Background(), "ghost"); !errors.Is(err, meta.ErrBucketNotFound) {
		t.Fatalf("got %v, want ErrBucketNotFound", err)
	}
}

func TestSetBucketVersioning(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if _, err := s.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetBucketVersioning(ctx, "bkt", meta.VersioningEnabled); err != nil {
		t.Fatalf("set versioning: %v", err)
	}
	got, err := s.GetBucket(ctx, "bkt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Versioning != meta.VersioningEnabled {
		t.Fatalf("versioning: got %q want Enabled", got.Versioning)
	}

	if err := s.SetBucketVersioning(ctx, "bkt", meta.VersioningSuspended); err != nil {
		t.Fatalf("set suspended: %v", err)
	}
	got, err = s.GetBucket(ctx, "bkt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Versioning != meta.VersioningSuspended {
		t.Fatalf("versioning: got %q want Suspended", got.Versioning)
	}
}

func TestSetBucketVersioningMissing(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	err := s.SetBucketVersioning(context.Background(), "ghost", meta.VersioningEnabled)
	if !errors.Is(err, meta.ErrBucketNotFound) {
		t.Fatalf("got %v, want ErrBucketNotFound", err)
	}
}

func TestSetBucketAttrsIndependent(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if _, err := s.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetBucketACL(ctx, "bkt", "private"); err != nil {
		t.Fatalf("set acl: %v", err)
	}
	if err := s.SetBucketRegion(ctx, "bkt", "us-east-1"); err != nil {
		t.Fatalf("set region: %v", err)
	}
	if err := s.SetBucketObjectLockEnabled(ctx, "bkt", true); err != nil {
		t.Fatalf("set object lock: %v", err)
	}
	if err := s.SetBucketMfaDelete(ctx, "bkt", meta.MfaDeleteEnabled); err != nil {
		t.Fatalf("set mfa: %v", err)
	}
	got, err := s.GetBucket(ctx, "bkt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ACL != "private" {
		t.Fatalf("acl: %q", got.ACL)
	}
	if got.Region != "us-east-1" {
		t.Fatalf("region: %q", got.Region)
	}
	if !got.ObjectLockEnabled {
		t.Fatalf("object lock not flipped")
	}
	if got.MfaDelete != meta.MfaDeleteEnabled {
		t.Fatalf("mfa delete: %q", got.MfaDelete)
	}
}

func TestListBuckets(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	for _, n := range []string{"alpha", "bravo", "charlie"} {
		owner := "alice"
		if n == "bravo" {
			owner = "bob"
		}
		if _, err := s.CreateBucket(ctx, n, owner, "STANDARD"); err != nil {
			t.Fatalf("create %q: %v", n, err)
		}
	}

	all, err := s.ListBuckets(ctx, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	names := bucketNames(all)
	sort.Strings(names)
	if got, want := names, []string{"alpha", "bravo", "charlie"}; !equalStrings(got, want) {
		t.Fatalf("all names: %v want %v", got, want)
	}

	mine, err := s.ListBuckets(ctx, "alice")
	if err != nil {
		t.Fatalf("list mine: %v", err)
	}
	names = bucketNames(mine)
	sort.Strings(names)
	if got, want := names, []string{"alpha", "charlie"}; !equalStrings(got, want) {
		t.Fatalf("mine names: %v want %v", got, want)
	}
}

func TestDeleteBucketEmpty(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if _, err := s.CreateBucket(ctx, "bkt", "alice", "STANDARD"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.DeleteBucket(ctx, "bkt"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetBucket(ctx, "bkt"); !errors.Is(err, meta.ErrBucketNotFound) {
		t.Fatalf("get after delete: %v want NotFound", err)
	}
}

func TestDeleteBucketMissing(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	if err := s.DeleteBucket(context.Background(), "ghost"); !errors.Is(err, meta.ErrBucketNotFound) {
		t.Fatalf("got %v, want ErrBucketNotFound", err)
	}
}

func TestDeleteBucketNotEmpty(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	b, err := s.CreateBucket(ctx, "bkt", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Inject a bucket-scoped row directly via the kv backend so the
	// emptiness probe trips. Story US-004 lands real PutObject; this
	// test just needs *something* under PrefixForBucket.
	mb := s.kv.(*memBackend)
	mb.data[string(append(PrefixForBucket(b.ID), "o/test\x00\x00..."...))] = []byte("placeholder")

	err = s.DeleteBucket(ctx, "bkt")
	if !errors.Is(err, meta.ErrBucketNotEmpty) {
		t.Fatalf("got %v, want ErrBucketNotEmpty", err)
	}
}

func bucketNames(bs []*meta.Bucket) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Name)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPrefixEnd guards the helper used by every range scan.
func TestPrefixEnd(t *testing.T) {
	cases := []struct {
		in, want []byte
	}{
		{[]byte("a"), []byte("b")},
		{[]byte("ab"), []byte("ac")},
		{[]byte{0x00}, []byte{0x01}},
		{[]byte{0xFF}, nil},
		{[]byte{0x01, 0xFF}, []byte{0x02}},
	}
	for _, c := range cases {
		got := prefixEnd(c.in)
		if string(got) != string(c.want) {
			t.Fatalf("prefixEnd(%v)=%v want %v", c.in, got, c.want)
		}
	}
}
