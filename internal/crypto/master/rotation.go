package master

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// EnvMasterKeys configures a rotation list:
//
//	STRATA_SSE_MASTER_KEYS=<keyID>:<hex64>[,<keyID>:<hex64>...]
//
// The first entry is the active wrap key; subsequent entries are unwrap-only.
// Used by the gateway during rotation windows and by cmd/strata-rewrap.
const EnvMasterKeys = "STRATA_SSE_MASTER_KEYS"

// ErrUnknownKeyID is returned by Resolver.ResolveByID when the requested keyID
// is not in the rotation set. Callers (GET, multipart unwrap) treat this as
// "object was wrapped under a key the operator forgot to keep" and surface 500.
var ErrUnknownKeyID = errors.New("strata: unknown master key id")

// ErrDuplicateKeyID is returned when STRATA_SSE_MASTER_KEYS contains two
// entries with the same keyID — the rotation list must have unique ids.
var ErrDuplicateKeyID = errors.New("strata: duplicate master key id in rotation list")

// Resolver extends Provider with keyID-addressed lookup. RotationProvider
// implements this; single-key providers (env/file/vault) do not — callers must
// type-assert and fall back to Resolve when the assertion fails.
type Resolver interface {
	Provider
	ResolveByID(ctx context.Context, keyID string) (key []byte, err error)
}

// KeyEntry is a single (id, raw key) pair for RotationProvider.
type KeyEntry struct {
	ID  string
	Key []byte
}

// RotationProvider serves an ordered set of master keys. Entries[0] is the
// active wrap key; the rest cover historical wrap keys so already-encrypted
// objects can still be unwrapped after rotation.
type RotationProvider struct {
	entries []KeyEntry
	byID    map[string][]byte
}

// NewRotationProvider validates and stores entries. Requires at least one
// entry, each key exactly KeySize bytes, and all ids unique.
func NewRotationProvider(entries []KeyEntry) (*RotationProvider, error) {
	if len(entries) == 0 {
		return nil, ErrNoConfig
	}
	byID := make(map[string][]byte, len(entries))
	cp := make([]KeyEntry, 0, len(entries))
	for i, e := range entries {
		if e.ID == "" {
			return nil, fmt.Errorf("rotation entry %d: empty key id", i)
		}
		if len(e.Key) != KeySize {
			return nil, fmt.Errorf("%w: id %q got %d bytes", ErrInvalidKeyLength, e.ID, len(e.Key))
		}
		if _, dup := byID[e.ID]; dup {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateKeyID, e.ID)
		}
		k := append([]byte(nil), e.Key...)
		byID[e.ID] = k
		cp = append(cp, KeyEntry{ID: e.ID, Key: k})
	}
	return &RotationProvider{entries: cp, byID: byID}, nil
}

// NewRotationProviderFromEnv parses STRATA_SSE_MASTER_KEYS. Returns ErrNoConfig
// when unset.
func NewRotationProviderFromEnv() (*RotationProvider, error) {
	raw := strings.TrimSpace(os.Getenv(EnvMasterKeys))
	if raw == "" {
		return nil, ErrNoConfig
	}
	entries, err := parseRotationList(raw)
	if err != nil {
		return nil, err
	}
	return NewRotationProvider(entries)
}

func parseRotationList(raw string) ([]KeyEntry, error) {
	parts := strings.Split(raw, ",")
	out := make([]KeyEntry, 0, len(parts))
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		id, hexKey, ok := strings.Cut(p, ":")
		id = strings.TrimSpace(id)
		hexKey = strings.TrimSpace(hexKey)
		if !ok || id == "" || hexKey == "" {
			return nil, fmt.Errorf("rotation entry %d: expected <keyID>:<hex64>, got %q", i, p)
		}
		key, err := decodeHexKey(hexKey)
		if err != nil {
			return nil, fmt.Errorf("rotation entry %q: %w", id, err)
		}
		out = append(out, KeyEntry{ID: id, Key: key})
	}
	if len(out) == 0 {
		return nil, ErrNoConfig
	}
	return out, nil
}

// Resolve returns the active wrap key (entries[0]).
func (r *RotationProvider) Resolve(_ context.Context) ([]byte, string, error) {
	e := r.entries[0]
	return e.Key, e.ID, nil
}

// ResolveByID returns the key matching the requested id, or ErrUnknownKeyID.
func (r *RotationProvider) ResolveByID(_ context.Context, keyID string) ([]byte, error) {
	k, ok := r.byID[keyID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKeyID, keyID)
	}
	return k, nil
}

// ActiveID returns the wrap key id (entries[0].ID) without performing crypto.
// Useful for the rewrap CLI's "is this object already current?" check.
func (r *RotationProvider) ActiveID() string {
	return r.entries[0].ID
}

// IDs returns all known key ids in declared order. Useful for logging and
// admin-CLI output. The returned slice is a copy.
func (r *RotationProvider) IDs() []string {
	out := make([]string, len(r.entries))
	for i, e := range r.entries {
		out[i] = e.ID
	}
	return out
}

// ResolveByID is a convenience that bridges Provider → Resolver: when p
// implements Resolver, it is used; otherwise Resolve is used and the result
// is returned only when its key id matches keyID. This lets server code
// stay uniform across rotation- and single-key configs.
func ResolveByID(ctx context.Context, p Provider, keyID string) ([]byte, error) {
	if r, ok := p.(Resolver); ok {
		return r.ResolveByID(ctx, keyID)
	}
	key, id, err := p.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	if keyID != "" && keyID != id {
		return nil, fmt.Errorf("%w: %q (provider holds %q)", ErrUnknownKeyID, keyID, id)
	}
	return key, nil
}
