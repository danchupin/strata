package serverapp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/crypto/kms"
)

// cfgWithKeyMaxAge returns a *config.Config carrying the supplied
// KeyMaxAge value verbatim — bypassing config.Load so the helper's own
// clamp logic is exercised in isolation from the config-side clamp.
func cfgWithKeyMaxAge(d time.Duration) *config.Config {
	return &config.Config{Auth: config.AuthConfig{KeyMaxAge: d}}
}

// cfgWithDEKCacheTTL mirrors cfgWithKeyMaxAge for the KMS side.
func cfgWithDEKCacheTTL(d time.Duration) *config.Config {
	return &config.Config{KMS: config.KMSConfig{DEKCacheTTL: d}}
}

func TestKeyMaxAge_Default(t *testing.T) {
	if got := keyMaxAge(nil, discardLogger()); got != defaultKeyMaxAge {
		t.Fatalf("default: got %s want %s", got, defaultKeyMaxAge)
	}
}

func TestKeyMaxAge_Parsed(t *testing.T) {
	if got := keyMaxAge(cfgWithKeyMaxAge(120*time.Hour), discardLogger()); got != 120*time.Hour {
		t.Fatalf("parsed: got %s want 120h", got)
	}
}

func TestKeyMaxAge_ClampBelowMin(t *testing.T) {
	if got := keyMaxAge(cfgWithKeyMaxAge(10*time.Minute), discardLogger()); got != minKeyMaxAge {
		t.Fatalf("below-min: got %s want %s", got, minKeyMaxAge)
	}
}

func TestKeyMaxAge_ClampAboveMax(t *testing.T) {
	if got := keyMaxAge(cfgWithKeyMaxAge(9000*time.Hour), discardLogger()); got != maxKeyMaxAge {
		t.Fatalf("above-max: got %s want %s", got, maxKeyMaxAge)
	}
}

func TestClassifyKMSUnwrapErr_Unavailable(t *testing.T) {
	wrapped := fmt.Errorf("%w: dial tcp: i/o timeout", kms.ErrKMSUnavailable)
	got := classifyKMSUnwrapErr(wrapped)
	if !errors.Is(got, auth.ErrKMSUnavailable) {
		t.Fatalf("kms.ErrKMSUnavailable should translate to auth.ErrKMSUnavailable, got %v", got)
	}
}

func TestClassifyKMSUnwrapErr_KeyIDMismatch(t *testing.T) {
	got := classifyKMSUnwrapErr(kms.ErrKeyIDMismatch)
	if !errors.Is(got, auth.ErrKMSDenied) {
		t.Fatalf("kms.ErrKeyIDMismatch should translate to auth.ErrKMSDenied, got %v", got)
	}
}

func TestClassifyKMSUnwrapErr_UnknownFallsThroughToDenied(t *testing.T) {
	got := classifyKMSUnwrapErr(errors.New("opaque kms hiccup"))
	if !errors.Is(got, auth.ErrKMSDenied) {
		t.Fatalf("unknown kms err should be fail-closed to auth.ErrKMSDenied, got %v", got)
	}
}

func TestClassifyKMSUnwrapErr_PreservesAuthSentinels(t *testing.T) {
	cases := []error{auth.ErrKMSUnavailable, auth.ErrKMSDenied, auth.ErrKMSTampered}
	for _, sentinel := range cases {
		got := classifyKMSUnwrapErr(sentinel)
		if got != sentinel {
			t.Fatalf("auth-side sentinel should pass through unchanged: in=%v out=%v", sentinel, got)
		}
	}
}

// netError satisfies net.Error with Timeout()=true so we can prove the
// WrapTransient classifier captures dial/timeout shapes — these are the
// most common transient KMS failure mode in practice.
type fakeTimeoutErr struct{}

func (fakeTimeoutErr) Error() string   { return "i/o timeout" }
func (fakeTimeoutErr) Timeout() bool   { return true }
func (fakeTimeoutErr) Temporary() bool { return true }

var _ net.Error = (*fakeTimeoutErr)(nil)

func TestKMSWrapTransient_DetectsTimeout(t *testing.T) {
	got := kms.WrapTransient(fakeTimeoutErr{})
	if !errors.Is(got, kms.ErrKMSUnavailable) {
		t.Fatalf("net.Error timeout should wrap as kms.ErrKMSUnavailable, got %v", got)
	}
}

func TestKMSWrapTransient_PreservesKeyIDMismatch(t *testing.T) {
	got := kms.WrapTransient(kms.ErrKeyIDMismatch)
	if errors.Is(got, kms.ErrKMSUnavailable) {
		t.Fatalf("ErrKeyIDMismatch must not be wrapped as transient, got %v", got)
	}
	if !errors.Is(got, kms.ErrKeyIDMismatch) {
		t.Fatalf("unexpected wrap: %v", got)
	}
}

func TestDekCacheTTL_FromConfig(t *testing.T) {
	if got := dekCacheTTL(cfgWithDEKCacheTTL(45*time.Second), discardLogger()); got != 45*time.Second {
		t.Fatalf("got %s want 45s", got)
	}
}

func TestBuildSigningKeyAdminConfig_NilProvider(t *testing.T) {
	cfg := buildSigningKeyAdminConfig(nil, nil, nil, discardLogger())
	if cfg.Provider != nil {
		t.Fatalf("nil provider should yield empty config; got %+v", cfg)
	}
}

func TestBuildSigningKeyAdminConfig_PopulatesFromResolver(t *testing.T) {
	prov := &fakeKMSProvider{} // local stub.
	resolver := &auth.BucketSigningResolver{
		Cache: auth.NewDEKCache(time.Minute),
	}
	appCfg := &config.Config{KMS: config.KMSConfig{DefaultKeyID: "alias/strata-test"}}
	cfg := buildSigningKeyAdminConfig(appCfg, prov, resolver, discardLogger())
	if cfg.Provider != prov {
		t.Fatalf("provider plumbing broken")
	}
	if cfg.Cache != resolver.Cache {
		t.Fatalf("cache plumbing broken")
	}
	if cfg.DefaultKeyID != "alias/strata-test" {
		t.Fatalf("default key id: got %q", cfg.DefaultKeyID)
	}
	if cfg.MaxAge != defaultKeyMaxAge {
		t.Fatalf("max age: got %s want %s", cfg.MaxAge, defaultKeyMaxAge)
	}
}

// fakeKMSProvider satisfies kms.Provider with zero behaviour — only the
// pointer identity matters in the wiring tests.
type fakeKMSProvider struct{}

func (fakeKMSProvider) GenerateDataKey(ctx context.Context, keyID string) ([]byte, []byte, error) {
	return nil, nil, errors.New("not used")
}
func (fakeKMSProvider) UnwrapDEK(ctx context.Context, keyID string, wrapped []byte) ([]byte, error) {
	return nil, errors.New("not used")
}
