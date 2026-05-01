package auth

import (
	"bytes"
	"crypto/hmac"
	"testing"
)

// TestChunkSignerAWSVectors verifies the chained per-chunk signatures
// against the AWS-published "Example Calculations" for streaming SigV4.
//
// Source: https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-streaming.html
// "Example Calculations" — PUT /examplebucket/chunkObject.txt with a
// 66560-byte body of 'a' bytes split into one 65536-byte chunk, one
// 1024-byte chunk, and a final empty chunk. Vectors transcribed inline
// so the test is offline-runnable across AWS doc URL rotations.
func TestChunkSignerAWSVectors(t *testing.T) {
	const (
		secret  = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
		date    = "20130524"
		region  = "us-east-1"
		service = "s3"
		isoDate = "20130524T000000Z"
		// Outer SigV4 signature from the request's Authorization header.
		seedSig = "4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9"
	)

	scope := credentialScope(date, region, service)
	signingKey := deriveSigningKey(secret, date, region, service)

	cases := []struct {
		name        string
		payload     []byte
		expectedSig string
	}{
		{
			name:        "chunk-1 (65536 bytes of 'a')",
			payload:     bytes.Repeat([]byte{'a'}, 65536),
			expectedSig: "ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648",
		},
		{
			name:        "chunk-2 (1024 bytes of 'a')",
			payload:     bytes.Repeat([]byte{'a'}, 1024),
			expectedSig: "0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497",
		},
		{
			name:        "final-empty (0 bytes still participates in the chain)",
			payload:     []byte{},
			expectedSig: "b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9",
		},
	}

	cs := newChunkSigner(seedSig, signingKey, isoDate, scope)
	for _, tc := range cases {
		got := cs.Next(tc.payload)
		// hmac.Equal: constant-time compare, Go stdlib idiom for auth tags.
		if !hmac.Equal([]byte(got), []byte(tc.expectedSig)) {
			t.Errorf("%s: signature mismatch\n  got  %s\n  want %s", tc.name, got, tc.expectedSig)
		}
	}
}

// TestChunkSignerAdvancesChain verifies that Next mutates prevSig so that
// repeated calls with the same payload produce different signatures (the
// chain advances).
func TestChunkSignerAdvancesChain(t *testing.T) {
	signingKey := deriveSigningKey("secret", "20260101", "us-east-1", "s3")
	scope := credentialScope("20260101", "us-east-1", "s3")
	cs := newChunkSigner("seed", signingKey, "20260101T000000Z", scope)

	payload := []byte("hello")
	first := cs.Next(payload)
	second := cs.Next(payload)
	if first == second {
		t.Fatalf("chain did not advance: both calls returned %s", first)
	}
}
