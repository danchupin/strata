package kms

import (
	"context"
	"crypto/rand"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awskms "github.com/aws/aws-sdk-go-v2/service/kms"
)

// Compile-time check that the real SDK client satisfies the narrow KMSAPI.
var _ KMSAPI = (*awskms.Client)(nil)

// fakeKMS is a deterministic in-memory KMS implementing the narrow interface.
// Each GenerateDataKey returns a fresh DEK; the CiphertextBlob carries the
// keyID + a per-call counter so Decrypt can verify the (keyID, blob) pair.
type fakeKMS struct {
	knownKeys map[string]bool

	mu     int32 // unused; atomic counter only
	nextID int32

	// store maps ciphertext-blob → (keyID, plaintext)
	store map[string]fakeKMSEntry

	genCalls     int32
	decryptCalls int32

	// optional forced errors
	genErr     error
	decryptErr error
}

type fakeKMSEntry struct {
	keyID string
	plain []byte
}

func newFakeKMS(knownKeyIDs ...string) *fakeKMS {
	fk := &fakeKMS{
		knownKeys: map[string]bool{},
		store:     map[string]fakeKMSEntry{},
	}
	for _, k := range knownKeyIDs {
		fk.knownKeys[k] = true
	}
	return fk
}

func (f *fakeKMS) GenerateDataKey(ctx context.Context, params *awskms.GenerateDataKeyInput, optFns ...func(*awskms.Options)) (*awskms.GenerateDataKeyOutput, error) {
	atomic.AddInt32(&f.genCalls, 1)
	if f.genErr != nil {
		return nil, f.genErr
	}
	keyID := aws.ToString(params.KeyId)
	if !f.knownKeys[keyID] {
		return nil, errors.New("fakeKMS: unknown key " + keyID)
	}
	plain := make([]byte, DEKSize)
	if _, err := rand.Read(plain); err != nil {
		return nil, err
	}
	id := atomic.AddInt32(&f.nextID, 1)
	blob := []byte{byte(id), byte(id >> 8), byte(id >> 16), byte(id >> 24)}
	// embed key id in the blob so a wrong-keyID Decrypt can be detected
	blob = append(blob, []byte(keyID)...)
	f.store[string(blob)] = fakeKMSEntry{keyID: keyID, plain: plain}
	arn := "arn:aws:kms:us-east-1:111122223333:key/" + keyID
	return &awskms.GenerateDataKeyOutput{
		KeyId:          aws.String(arn),
		Plaintext:      plain,
		CiphertextBlob: blob,
	}, nil
}

func (f *fakeKMS) Decrypt(ctx context.Context, params *awskms.DecryptInput, optFns ...func(*awskms.Options)) (*awskms.DecryptOutput, error) {
	atomic.AddInt32(&f.decryptCalls, 1)
	if f.decryptErr != nil {
		return nil, f.decryptErr
	}
	entry, ok := f.store[string(params.CiphertextBlob)]
	if !ok {
		return nil, errors.New("fakeKMS: ciphertext not found")
	}
	arn := "arn:aws:kms:us-east-1:111122223333:key/" + entry.keyID
	return &awskms.DecryptOutput{
		KeyId:     aws.String(arn),
		Plaintext: entry.plain,
	}, nil
}

func TestAWSKMSProviderRoundTrip(t *testing.T) {
	fk := newFakeKMS("key-1")
	p, err := NewAWSKMSProvider(fk, "us-east-1")
	if err != nil {
		t.Fatalf("NewAWSKMSProvider: %v", err)
	}
	plain, wrapped, err := p.GenerateDataKey(context.Background(), "key-1")
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	if len(plain) != DEKSize {
		t.Fatalf("plain size = %d, want %d", len(plain), DEKSize)
	}
	if len(wrapped) == 0 {
		t.Fatal("wrapped is empty")
	}
	got, err := p.UnwrapDEK(context.Background(), "key-1", wrapped)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if string(got) != string(plain) {
		t.Fatalf("UnwrapDEK plaintext mismatch")
	}
	if fk.genCalls != 1 || fk.decryptCalls != 1 {
		t.Fatalf("call counts: gen=%d decrypt=%d", fk.genCalls, fk.decryptCalls)
	}
}

func TestAWSKMSProviderMissingKeyID(t *testing.T) {
	fk := newFakeKMS("key-1")
	p, _ := NewAWSKMSProvider(fk, "us-east-1")
	if _, _, err := p.GenerateDataKey(context.Background(), ""); !errors.Is(err, ErrMissingKeyID) {
		t.Fatalf("GenerateDataKey empty keyID err=%v, want ErrMissingKeyID", err)
	}
	if _, err := p.UnwrapDEK(context.Background(), "", []byte{1, 2, 3}); !errors.Is(err, ErrMissingKeyID) {
		t.Fatalf("UnwrapDEK empty keyID err=%v, want ErrMissingKeyID", err)
	}
}

func TestAWSKMSProviderUnknownKeyID(t *testing.T) {
	fk := newFakeKMS("key-1")
	p, _ := NewAWSKMSProvider(fk, "us-east-1")
	_, _, err := p.GenerateDataKey(context.Background(), "key-missing")
	if err == nil {
		t.Fatal("expected error for unknown keyID")
	}
}

func TestAWSKMSProviderKeyIDMismatchOnUnwrap(t *testing.T) {
	fk := newFakeKMS("key-A", "key-B")
	p, _ := NewAWSKMSProvider(fk, "us-east-1")
	_, wrappedA, err := p.GenerateDataKey(context.Background(), "key-A")
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	// Decrypt with the wrong keyID — fake returns the actual A ARN, code
	// should detect mismatch.
	_, err = p.UnwrapDEK(context.Background(), "key-B", wrappedA)
	if err == nil || !errors.Is(err, ErrKeyIDMismatch) {
		t.Fatalf("UnwrapDEK mismatch err=%v, want ErrKeyIDMismatch", err)
	}
}

func TestAWSKMSProviderEmptyWrapped(t *testing.T) {
	fk := newFakeKMS("key-1")
	p, _ := NewAWSKMSProvider(fk, "us-east-1")
	_, err := p.UnwrapDEK(context.Background(), "key-1", nil)
	if err == nil {
		t.Fatal("expected error for empty wrapped DEK")
	}
}

func TestNewAWSKMSProviderNilClient(t *testing.T) {
	_, err := NewAWSKMSProvider(nil, "us-east-1")
	if err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestNewAWSKMSProviderFromEnv(t *testing.T) {
	t.Setenv(EnvAWSKMSRegion, "")
	if _, err := NewAWSKMSProviderFromEnv(nil); !errors.Is(err, ErrNoConfig) {
		t.Fatalf("empty env err=%v, want ErrNoConfig", err)
	}

	t.Setenv(EnvAWSKMSRegion, "us-east-1")
	if _, err := NewAWSKMSProviderFromEnv(nil); err == nil {
		t.Fatal("expected error when factory missing")
	}

	fk := newFakeKMS("key-1")
	factoryCalls := 0
	factory := func(region string) (KMSAPI, error) {
		factoryCalls++
		if region != "us-east-1" {
			t.Fatalf("factory region=%q", region)
		}
		return fk, nil
	}
	p, err := NewAWSKMSProviderFromEnv(factory)
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if p.Region != "us-east-1" {
		t.Fatalf("Region=%q", p.Region)
	}
	if factoryCalls != 1 {
		t.Fatalf("factory calls=%d", factoryCalls)
	}

	// factory error propagates
	_, err = NewAWSKMSProviderFromEnv(func(string) (KMSAPI, error) { return nil, errors.New("boom") })
	if err == nil {
		t.Fatal("expected factory error")
	}
}

func TestSameKeyARN(t *testing.T) {
	cases := []struct {
		reported, requested string
		want                bool
	}{
		{"key-1", "key-1", true},
		{"arn:aws:kms:us-east-1:111122223333:key/key-1", "key-1", true},
		{"key-1", "arn:aws:kms:us-east-1:111122223333:key/key-1", true},
		{"arn:aws:kms:us-east-1:111122223333:key/key-1", "arn:aws:kms:us-east-1:111122223333:key/key-1", true},
		{"arn:aws:kms:us-east-1:111122223333:key/key-1", "key-2", false},
		{"key-1", "key-2", false},
		{"", "", true},
	}
	for _, c := range cases {
		if got := sameKeyARN(c.reported, c.requested); got != c.want {
			t.Errorf("sameKeyARN(%q,%q)=%v, want %v", c.reported, c.requested, got, c.want)
		}
	}
}
