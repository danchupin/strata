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
// caller. Used by the middleware when STRATA_AUTH_MODE=optional|disabled and a
// request carries no Authorization header or presigned query.
func AnonymousIdentity() *AuthInfo {
	return &AuthInfo{IsAnonymous: true, Owner: "anonymous"}
}

type Mode int

const (
	// ModeOff (alias ModeDisabled) bypasses signature validation entirely and
	// treats every request as anonymous. Dev-only.
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
