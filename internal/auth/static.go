package auth

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

type StaticStore struct {
	mu    sync.RWMutex
	creds map[string]*Credential
}

func NewStaticStore(creds map[string]*Credential) *StaticStore {
	cp := make(map[string]*Credential, len(creds))
	for k, v := range creds {
		cp[k] = v
	}
	return &StaticStore{creds: cp}
}

func (s *StaticStore) Lookup(_ context.Context, accessKey string) (*Credential, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.creds[accessKey]
	if !ok {
		return nil, ErrNoSuchCredential
	}
	return c, nil
}

func ParseStaticCredentials(s string) (map[string]*Credential, error) {
	out := make(map[string]*Credential)
	if s == "" {
		return out, nil
	}
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ":", 3)
		if len(parts) < 2 {
			return nil, fmt.Errorf("bad credential entry %q (want key:secret[:owner])", entry)
		}
		owner := parts[0]
		if len(parts) == 3 && parts[2] != "" {
			owner = parts[2]
		}
		out[parts[0]] = &Credential{
			AccessKey: parts[0],
			Secret:    parts[1],
			Owner:     owner,
		}
	}
	return out, nil
}
