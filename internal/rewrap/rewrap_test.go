package rewrap

import (
	"context"
	"testing"

	"github.com/danchupin/strata/internal/crypto/master"
	ssecrypto "github.com/danchupin/strata/internal/crypto/sse"
	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

func keyN(b byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = b
	}
	return k
}

func mustWrap(t *testing.T, mk, dek []byte) []byte {
	t.Helper()
	w, err := ssecrypto.WrapDEK(mk, dek)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	return w
}

// putEncryptedObject seeds a metamem with a single encrypted object so the
// rewrap library can act on it. We bypass the s3api PUT path and write the
// row directly.
func putEncryptedObject(t *testing.T, store *metamem.Store, bucketName, key string, mk []byte, mkID string) {
	t.Helper()
	ctx := context.Background()
	b, err := store.GetBucket(ctx, bucketName)
	if err != nil {
		b, err = store.CreateBucket(ctx, bucketName, "owner", "STANDARD")
		if err != nil {
			t.Fatalf("create bucket: %v", err)
		}
	}
	dek := keyN(0xAA)
	wrapped := mustWrap(t, mk, dek)
	o := &meta.Object{
		BucketID:     b.ID,
		Key:          key,
		Size:         5,
		ETag:         "deadbeef",
		StorageClass: "STANDARD",
		Manifest:     &data.Manifest{},
		SSE:          "AES256",
		SSEKey:       wrapped,
		SSEKeyID:     mkID,
	}
	if err := store.PutObject(ctx, o, false); err != nil {
		t.Fatalf("put object: %v", err)
	}
}

func TestRewrap_NewRequiresMetaAndProvider(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatalf("expected error when meta+provider missing")
	}
}

func TestRewrap_PutUnderA_RewrapToB(t *testing.T) {
	store := metamem.New()
	putEncryptedObject(t, store, "bkt", "k", keyN(0x11), "A")

	rotated, err := master.NewRotationProvider([]master.KeyEntry{
		{ID: "B", Key: keyN(0x22)},
		{ID: "A", Key: keyN(0x11)},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, err := New(Config{Meta: store, Provider: rotated})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := w.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.ObjectsRewrapped != 1 {
		t.Fatalf("ObjectsRewrapped = %d, want 1", stats.ObjectsRewrapped)
	}

	b, _ := store.GetBucket(context.Background(), "bkt")
	o, _ := store.GetObject(context.Background(), b.ID, "k", "")
	if o.SSEKeyID != "B" {
		t.Fatalf("SSEKeyID = %q, want B", o.SSEKeyID)
	}
	dek, err := ssecrypto.UnwrapDEK(keyN(0x22), o.SSEKey)
	if err != nil {
		t.Fatalf("unwrap with B: %v", err)
	}
	if string(dek) != string(keyN(0xAA)) {
		t.Fatalf("DEK changed under rewrap")
	}

	prog, err := store.GetRewrapProgress(context.Background(), b.ID)
	if err != nil || !prog.Complete || prog.TargetID != "B" {
		t.Fatalf("progress = %+v err=%v", prog, err)
	}
}

func TestRewrap_AlreadyCurrent_NoOp(t *testing.T) {
	store := metamem.New()
	putEncryptedObject(t, store, "bkt", "k", keyN(0x22), "B")

	rot, _ := master.NewRotationProvider([]master.KeyEntry{
		{ID: "B", Key: keyN(0x22)},
		{ID: "A", Key: keyN(0x11)},
	})
	w, _ := New(Config{Meta: store, Provider: rot})
	stats, err := w.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.ObjectsRewrapped != 0 {
		t.Fatalf("ObjectsRewrapped = %d, want 0 (already current)", stats.ObjectsRewrapped)
	}
	if stats.ObjectsScanned != 1 {
		t.Fatalf("ObjectsScanned = %d, want 1", stats.ObjectsScanned)
	}
}

func TestRewrap_UnknownOldKey_Fails(t *testing.T) {
	store := metamem.New()
	putEncryptedObject(t, store, "bkt", "k", keyN(0x11), "A")

	// Provider has only B — A is gone, so unwrap must fail.
	rot, _ := master.NewRotationProvider([]master.KeyEntry{
		{ID: "B", Key: keyN(0x22)},
	})
	w, _ := New(Config{Meta: store, Provider: rot})
	_, err := w.Run(context.Background())
	if err == nil {
		t.Fatalf("expected error when historical key id is missing")
	}
}

func TestRewrap_RewrapsInFlightMultipart(t *testing.T) {
	store := metamem.New()
	ctx := context.Background()
	b, err := store.CreateBucket(ctx, "bkt", "owner", "STANDARD")
	if err != nil {
		t.Fatal(err)
	}
	mu := &meta.MultipartUpload{
		BucketID:     b.ID,
		UploadID:     "00000000-0000-0000-0000-000000000001",
		Key:          "key",
		Status:       "uploading",
		StorageClass: "STANDARD",
		SSE:          "AES256",
		SSEKey:       mustWrap(t, keyN(0x11), keyN(0xAA)),
		SSEKeyID:     "A",
	}
	if err := store.CreateMultipartUpload(ctx, mu); err != nil {
		t.Fatalf("create mu: %v", err)
	}

	rot, _ := master.NewRotationProvider([]master.KeyEntry{
		{ID: "B", Key: keyN(0x22)},
		{ID: "A", Key: keyN(0x11)},
	})
	w, _ := New(Config{Meta: store, Provider: rot})
	stats, err := w.Run(ctx)
	if err != nil {
		t.Fatalf("rewrap: %v", err)
	}
	if stats.UploadsRewrapped != 1 {
		t.Fatalf("UploadsRewrapped = %d, want 1 (stats=%+v)", stats.UploadsRewrapped, stats)
	}
	got, err := store.GetMultipartUpload(ctx, b.ID, mu.UploadID)
	if err != nil {
		t.Fatalf("get mu: %v", err)
	}
	if got.SSEKeyID != "B" {
		t.Fatalf("upload SSEKeyID = %q, want B", got.SSEKeyID)
	}
	dek, err := ssecrypto.UnwrapDEK(keyN(0x22), got.SSEKey)
	if err != nil {
		t.Fatalf("unwrap with B: %v", err)
	}
	if string(dek) != string(keyN(0xAA)) {
		t.Fatalf("DEK changed under multipart rewrap")
	}
}

func TestRewrap_RecordsProgressForEmptyBucket(t *testing.T) {
	store := metamem.New()
	if _, err := store.CreateBucket(context.Background(), "empty", "owner", "STANDARD"); err != nil {
		t.Fatal(err)
	}
	rot, _ := master.NewRotationProvider([]master.KeyEntry{{ID: "A", Key: keyN(0x11)}})
	w, _ := New(Config{Meta: store, Provider: rot})
	stats, err := w.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.BucketsScanned != 1 || stats.ObjectsRewrapped != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	b, _ := store.GetBucket(context.Background(), "empty")
	prog, err := store.GetRewrapProgress(context.Background(), b.ID)
	if err != nil || !prog.Complete {
		t.Fatalf("progress = %+v err=%v", prog, err)
	}
}
