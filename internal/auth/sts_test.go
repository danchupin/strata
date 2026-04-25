package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/auth"
)

func TestSTSStore_IssueAndLookup(t *testing.T) {
	s := auth.NewSTSStore()
	sess, err := s.Issue("alice", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if !strings.HasPrefix(sess.AccessKey, "ASIA") {
		t.Errorf("expected ASIA-prefixed access key, got %q", sess.AccessKey)
	}
	if sess.SecretKey == "" || sess.SessionToken == "" {
		t.Errorf("missing secret or token: %+v", sess)
	}
	if sess.Owner != "alice" {
		t.Errorf("owner=%q want alice", sess.Owner)
	}

	cred, err := s.Lookup(context.Background(), sess.AccessKey)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if cred.Secret != sess.SecretKey || cred.SessionToken != sess.SessionToken || cred.Owner != "alice" {
		t.Errorf("cred mismatch: %+v vs sess %+v", cred, sess)
	}
}

func TestSTSStore_LookupUnknownReturnsNoSuchCredential(t *testing.T) {
	s := auth.NewSTSStore()
	_, err := s.Lookup(context.Background(), "ASIAUNKNOWN")
	if !errors.Is(err, auth.ErrNoSuchCredential) {
		t.Fatalf("expected ErrNoSuchCredential, got %v", err)
	}
}

func TestSTSStore_ExpiredLookupReturnsExpiredToken(t *testing.T) {
	s := auth.NewSTSStore()
	now := time.Now()
	s.SetClock(func() time.Time { return now })
	sess, err := s.Issue("alice", 5*time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// Move clock past expiry.
	s.SetClock(func() time.Time { return now.Add(10 * time.Minute) })

	if _, err := s.Lookup(context.Background(), sess.AccessKey); !errors.Is(err, auth.ErrExpiredToken) {
		t.Fatalf("expected ErrExpiredToken, got %v", err)
	}
	// After eviction the key looks unknown.
	if _, err := s.Lookup(context.Background(), sess.AccessKey); !errors.Is(err, auth.ErrNoSuchCredential) {
		t.Fatalf("expected ErrNoSuchCredential after eviction, got %v", err)
	}
}

func TestSTSStore_DefaultDuration(t *testing.T) {
	s := auth.NewSTSStore()
	now := time.Now()
	s.SetClock(func() time.Time { return now })
	sess, err := s.Issue("alice", 0)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if delta := sess.Expiration.Sub(now); delta < 59*time.Minute || delta > 61*time.Minute {
		t.Errorf("expected ~1h ttl, got %v", delta)
	}
}

func TestSTSStore_RevokeDropsSession(t *testing.T) {
	s := auth.NewSTSStore()
	sess, err := s.Issue("bob", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	s.Revoke(sess.AccessKey)
	if _, err := s.Lookup(context.Background(), sess.AccessKey); !errors.Is(err, auth.ErrNoSuchCredential) {
		t.Fatalf("expected ErrNoSuchCredential after revoke, got %v", err)
	}
}

func TestSTSStore_UniqueAccessKeys(t *testing.T) {
	s := auth.NewSTSStore()
	a, _ := s.Issue("a", time.Hour)
	b, _ := s.Issue("b", time.Hour)
	if a.AccessKey == b.AccessKey || a.SecretKey == b.SecretKey || a.SessionToken == b.SessionToken {
		t.Errorf("expected unique creds, got %+v vs %+v", a, b)
	}
}

func TestMultiStore_DoesNotCacheTempCreds(t *testing.T) {
	sts := auth.NewSTSStore()
	sess, err := sts.Issue("alice", time.Hour)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	m := auth.NewMultiStore(time.Minute, sts)

	// First lookup hits store.
	if _, err := m.Lookup(context.Background(), sess.AccessKey); err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	// Revoke directly on the store. If MultiStore had cached the cred we'd
	// still see it; with no caching for SessionToken-bearing creds the next
	// lookup must fall through to ErrNoSuchCredential.
	sts.Revoke(sess.AccessKey)
	if _, err := m.Lookup(context.Background(), sess.AccessKey); !errors.Is(err, auth.ErrNoSuchCredential) {
		t.Fatalf("expected ErrNoSuchCredential after revoke, got %v", err)
	}
}
