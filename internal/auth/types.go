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
)

type Credential struct {
	AccessKey string
	Secret    string
	Owner     string
}

type CredentialsStore interface {
	Lookup(ctx context.Context, accessKey string) (*Credential, error)
}

type AuthInfo struct {
	AccessKey string
	Owner     string
	Anonymous bool
}

type ctxKey struct{}

func WithAuth(ctx context.Context, info *AuthInfo) context.Context {
	return context.WithValue(ctx, ctxKey{}, info)
}

func FromContext(ctx context.Context) *AuthInfo {
	if v, ok := ctx.Value(ctxKey{}).(*AuthInfo); ok {
		return v
	}
	return &AuthInfo{Anonymous: true, Owner: "anonymous"}
}

type Mode int

const (
	ModeOff Mode = iota
	ModeRequired
)

func ParseMode(s string) (Mode, error) {
	switch s {
	case "", "off":
		return ModeOff, nil
	case "required":
		return ModeRequired, nil
	default:
		return ModeOff, errors.New("unknown auth mode: " + s)
	}
}
