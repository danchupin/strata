package tikv

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/rand"
	"sort"
	"testing"

	"github.com/gocql/gocql"
	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// TestEscapedRoundtrip walks the byte-stuffing layer over 1k random
// inputs (some containing 0x00) and asserts encode→decode is identity.
// Required by US-002 acceptance ("property-test 1k random inputs").
func TestEscapedRoundtrip(t *testing.T) {
	r := rand.New(rand.NewSource(0xC0FFEE))
	for range 1000 {
		want := randomBytes(r, r.Intn(64))
		enc := appendEscaped(nil, string(want))
		got, rest, err := readEscaped(enc)
		if err != nil {
			t.Fatalf("readEscaped(%q): %v", want, err)
		}
		if len(rest) != 0 {
			t.Fatalf("readEscaped: %d trailing bytes", len(rest))
		}
		if got != string(want) {
			t.Fatalf("roundtrip mismatch: got %q want %q", got, want)
		}
	}
}

// TestEscapedOrdering checks that lex order is preserved across
// heterogeneous-length escaped segments — the property that justifies
// FoundationDB-style stuffing over plain length-prefixing.
func TestEscapedOrdering(t *testing.T) {
	in := []string{
		"",
		"a",
		"a\x00",
		"a\x00b",
		"ab",
		"ab\x00",
		"abc",
		"b",
	}
	encoded := make([][]byte, len(in))
	for i, s := range in {
		encoded[i] = appendEscaped(nil, s)
	}
	for i := 1; i < len(encoded); i++ {
		if bytes.Compare(encoded[i-1], encoded[i]) >= 0 {
			t.Fatalf("escaped(%q)=%x must lex-precede escaped(%q)=%x",
				in[i-1], encoded[i-1], in[i], encoded[i])
		}
	}
}

// TestObjectKeyRoundtrip — 1k random (bucket, key, version) triples
// round-trip through ObjectKey/DecodeObjectKey.
func TestObjectKeyRoundtrip(t *testing.T) {
	r := rand.New(rand.NewSource(0xABCD))
	for range 1000 {
		bucketID := uuid.New()
		key := string(randomBytes(r, 1+r.Intn(48)))
		ver := gocql.TimeUUID().String()

		k, err := ObjectKey(bucketID, key, ver)
		if err != nil {
			t.Fatalf("ObjectKey: %v", err)
		}
		gotBucket, gotKey, gotVer, err := DecodeObjectKey(k)
		if err != nil {
			t.Fatalf("DecodeObjectKey: %v (key=%x)", err, k)
		}
		if gotBucket != bucketID {
			t.Fatalf("bucket mismatch: %s want %s", gotBucket, bucketID)
		}
		if gotKey != key {
			t.Fatalf("key mismatch: %q want %q", gotKey, key)
		}
		if gotVer != ver {
			t.Fatalf("version mismatch: %s want %s", gotVer, ver)
		}
	}
}

// TestObjectKeyOrdering asserts ascending TiKV scans return
// (key ASC, version DESC) — newer version first within the same key.
func TestObjectKeyOrdering(t *testing.T) {
	bucketID := uuid.New()
	type row struct {
		key, ver string
	}
	// Build a few keys, each with several versions emitted oldest→newest.
	rows := []row{}
	keys := []string{"a", "ab", "abc", "b"}
	for _, k := range keys {
		for range 4 {
			rows = append(rows, row{k, gocql.TimeUUID().String()})
		}
	}
	encoded := make([][]byte, len(rows))
	for i, r := range rows {
		k, err := ObjectKey(bucketID, r.key, r.ver)
		if err != nil {
			t.Fatalf("ObjectKey: %v", err)
		}
		encoded[i] = k
	}
	// Sort encoded keys ascending — should equal:
	// keys ascending; within a key, version-id timestamps DESC.
	type indexed struct {
		key []byte
		idx int
	}
	idx := make([]indexed, len(encoded))
	for i, e := range encoded {
		idx[i] = indexed{e, i}
	}
	sort.Slice(idx, func(i, j int) bool { return bytes.Compare(idx[i].key, idx[j].key) < 0 })

	// Walk groups of 4 (one per user key) and verify version_id DESC.
	for g := range len(keys) {
		group := idx[g*4 : g*4+4]
		// All four must share the same user-key.
		_, kStr, _, err := DecodeObjectKey(group[0].key)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if kStr != keys[g] {
			t.Fatalf("group %d user-key=%q want %q", g, kStr, keys[g])
		}
		// Versions descending by timeuuid timestamp; ties allowed (rare
		// — same 100-ns tick — and broken by raw UUID bytes via the
		// version-DESC suffix, which is what we ultimately verify).
		var prevTS uint64 = math.MaxUint64
		var prevKey []byte
		for _, e := range group {
			_, _, ver, err := DecodeObjectKey(e.key)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			id := uuid.MustParse(ver)
			ts := uint64(id.Time())
			if ts > prevTS {
				t.Fatalf("version order: ts=%d not <= prev=%d", ts, prevTS)
			}
			if prevKey != nil && bytes.Compare(prevKey, e.key) >= 0 {
				t.Fatalf("encoded order: prev key %x not strictly less than %x", prevKey, e.key)
			}
			prevTS = ts
			prevKey = e.key
		}
	}
}

// TestVersionDescNullSentinel — the "null" version (NullVersionID)
// encodes to MaxUint64 in the inverted-ts half (timestamp 0 → inverted
// MaxUint64) so it sorts last among the versions of a given key.
func TestVersionDescNullSentinel(t *testing.T) {
	suf, err := EncodeVersionDesc(meta.NullVersionID)
	if err != nil {
		t.Fatalf("EncodeVersionDesc(null): %v", err)
	}
	if got := binary.BigEndian.Uint64(suf[:8]); got != math.MaxUint64 {
		t.Fatalf("null inverted-ts = %#016x; want %#016x", got, uint64(math.MaxUint64))
	}
	// And the round-trip recovers the exact NullVersionID literal.
	back, err := DecodeVersionDesc(suf)
	if err != nil {
		t.Fatalf("DecodeVersionDesc: %v", err)
	}
	if back != meta.NullVersionID {
		t.Fatalf("null roundtrip = %q; want %q", back, meta.NullVersionID)
	}
}

// TestObjectPrefixWithKey — the prefix must lex-precede every
// ObjectKey whose user-space key starts with the prefix, and lex-be-
// >= every ObjectKey whose user-space key sorts strictly before it.
func TestObjectPrefixWithKey(t *testing.T) {
	bucketID := uuid.New()
	prefix := ObjectPrefixWithKey(bucketID, "ab")

	cases := []struct {
		key      string
		matches  bool
	}{
		{"a", false},
		{"ab", true},
		{"abc", true},
		{"abcd", true},
		{"ac", false},
		{"b", false},
	}
	for _, c := range cases {
		k, err := ObjectKey(bucketID, c.key, gocql.TimeUUID().String())
		if err != nil {
			t.Fatalf("ObjectKey(%q): %v", c.key, err)
		}
		got := bytes.HasPrefix(k, prefix)
		if got != c.matches {
			t.Fatalf("key=%q HasPrefix=%v want %v", c.key, got, c.matches)
		}
	}
}

// TestQueueKeyOrdering — within a bucket, queueKey orders by ts then
// eventID. Forward range scans claim oldest first.
func TestQueueKeyOrdering(t *testing.T) {
	bucketID := uuid.New()
	a := NotifyQueueKey(bucketID, 100, "a")
	b := NotifyQueueKey(bucketID, 200, "z") // newer ts wins
	c := NotifyQueueKey(bucketID, 200, "a") // same ts, smaller eventID wins
	d := NotifyQueueKey(bucketID, 200, "z")
	if !(bytes.Compare(a, c) < 0 && bytes.Compare(c, b) < 0 && bytes.Equal(b, d)) {
		t.Fatalf("queue key ordering broken")
	}
}

// TestUniquePrefixesNoOverlap — every top-level entity prefix must be
// distinct from every other (no prefix-of relationship). A scan over
// one entity's namespace must never accidentally bleed into another.
func TestUniquePrefixesNoOverlap(t *testing.T) {
	prefixes := []string{
		prefixBucketByName, prefixBucketScoped,
		prefixIAMUser, prefixIAMAccessKey, prefixIAMUserKeyIndex,
		prefixAccessPoint, prefixAccessPointAlias,
		prefixNotifyQueue, prefixNotifyDLQ,
		prefixReplicationQueue, prefixAccessLogQueue, prefixGCQueue, prefixGCQueueV2,
		prefixAuditLog, prefixLeaderLock, prefixReshardJob,
	}
	for i, p := range prefixes {
		for j, q := range prefixes {
			if i == j {
				continue
			}
			if bytes.HasPrefix([]byte(p), []byte(q)) {
				t.Fatalf("prefix %q is a prefix of %q", q, p)
			}
		}
	}
}

func randomBytes(r *rand.Rand, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		// Mix in occasional 0x00 to exercise the escape path.
		if r.Intn(8) == 0 {
			out[i] = 0x00
		} else {
			out[i] = byte(r.Intn(256))
		}
	}
	return out
}
