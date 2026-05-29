package auth

import (
	"context"
	"errors"
)

var (
	ErrNoSuchCredential = errors.New("credential not found")
	ErrMissingSignature = errors.New("missing Authorization header")
	ErrMalformedAuth    = errors.New("malformed Authorization header")
	ErrSignatureInvalid = errors.New("signature does not match")
	ErrClockSkew        = errors.New("request time outside permitted window")
	ErrUnsupportedBody  = errors.New("unsupported x-amz-content-sha256 mode")
	ErrExpiredToken     = errors.New("expired token")
	ErrInvalidToken     = errors.New("invalid security token")
	// ErrUnsupportedChecksumAlgorithm — STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER
	// requests carry X-Amz-Trailer: <algo>. The supported set is sha256
	// (US-009 of ralph/storage-correctness) + sha1 + crc32 + crc32c (US-004
	// of ralph/auth-dx-trailer-lima). Names outside that set (md5, sha512,
	// ...) reject with this sentinel — mapped to HTTP 400 InvalidRequest.
	ErrUnsupportedChecksumAlgorithm = errors.New("unsupported checksum algorithm")
)

type Credential struct {
	AccessKey    string
	Secret       string
	Owner        string
	SessionToken string
}

type CredentialsStore interface {
	Lookup(ctx context.Context, accessKey string) (*Credential, error)
}

type AuthInfo struct {
	AccessKey   string
	Owner       string
	IsAnonymous bool
	// FullAccess marks an identity that bypasses the data-plane
	// authorization gates (bucket policy + ACL) entirely. Set ONLY by the
	// middleware under ModeOff — a dev-only "no auth at all" posture. It does
	// NOT grant IAM-admin (those endpoints stay gated on the root principal).
	FullAccess bool
}

type ctxKey struct{}

func WithAuth(ctx context.Context, info *AuthInfo) context.Context {
	return context.WithValue(ctx, ctxKey{}, info)
}

func FromContext(ctx context.Context) *AuthInfo {
	if v, ok := ctx.Value(ctxKey{}).(*AuthInfo); ok {
		return v
	}
	return AnonymousIdentity()
}

// AnonymousIdentity returns a fresh AuthInfo describing an unauthenticated
// caller. Used by the middleware when STRATA_AUTH_MODE=optional and a request
// carries no Authorization header or presigned query.
func AnonymousIdentity() *AuthInfo {
	return &AuthInfo{IsAnonymous: true, Owner: "anonymous"}
}

// FullAccessIdentity returns the identity injected by the middleware under
// ModeOff: a dev-only caller that bypasses the policy/ACL data-plane gates.
// Owner is a stable, non-anonymous label so audit rows and bucket ownership
// stay coherent.
func FullAccessIdentity() *AuthInfo {
	return &AuthInfo{Owner: "dev-full-access", FullAccess: true}
}

type Mode int

const (
	// ModeOff (alias ModeDisabled) is a dev-only "no auth at all" posture: it
	// bypasses signature validation AND the data-plane authorization gates
	// (bucket policy + ACL), so every request behaves as a full-access caller.
	// NEVER use in production — it leaves the gateway wide open. The default
	// auth mode is ModeRequired precisely so an unconfigured deployment is
	// secure; off must be opted into explicitly.
	ModeOff Mode = iota
	// ModeRequired demands a valid SigV4 signature on every request.
	ModeRequired
	// ModeOptional accepts unsigned requests as anonymous but still validates
	// any request that carries an Authorization header or presigned query.
	// Anonymous requests are then gated by the bucket policy/ACL stack.
	ModeOptional
)

// ModeDisabled is an alias for ModeOff exposed under the name used by the
// STRATA_AUTH_MODE env var ("disabled").
const ModeDisabled = ModeOff

func ParseMode(s string) (Mode, error) {
	switch s {
	case "", "off", "disabled":
		return ModeOff, nil
	case "required":
		return ModeRequired, nil
	case "optional":
		return ModeOptional, nil
	default:
		return ModeOff, errors.New("unknown auth mode: " + s)
	}
}
