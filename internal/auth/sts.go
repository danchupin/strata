package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// DefaultSTSDuration is the default validity window for AssumeRole-issued
// temporary credentials when the caller does not specify DurationSeconds.
const DefaultSTSDuration = time.Hour

// STSSession is the credential triple returned to the caller of AssumeRole
// plus enough metadata to drive expiry checks.
type STSSession struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
	Owner        string
	Expiration   time.Time
}

// STSStore holds in-memory temporary credentials. Sessions are evicted lazily
// on Lookup once they expire; expired lookups surface ErrExpiredToken so
// callers can return 403 ExpiredToken.
type STSStore struct {
	mu   sync.Mutex
	sess map[string]STSSession
	now  func() time.Time
}

func NewSTSStore() *STSStore {
	return &STSStore{
		sess: make(map[string]STSSession),
		now:  time.Now,
	}
}

// SetClock swaps the time source. Tests use it to advance past a session's
// expiry without sleeping.
func (s *STSStore) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// Issue mints a new (AccessKey, SecretKey, SessionToken) tuple and stores it
// for ttl. ttl <= 0 falls back to DefaultSTSDuration.
func (s *STSStore) Issue(owner string, ttl time.Duration) (STSSession, error) {
	if ttl <= 0 {
		ttl = DefaultSTSDuration
	}
	var akBuf [10]byte
	if _, err := rand.Read(akBuf[:]); err != nil {
		return STSSession{}, err
	}
	var skBuf [30]byte
	if _, err := rand.Read(skBuf[:]); err != nil {
		return STSSession{}, err
	}
	var stBuf [48]byte
	if _, err := rand.Read(stBuf[:]); err != nil {
		return STSSession{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := STSSession{
		AccessKey:    "ASIA" + strings.ToUpper(hex.EncodeToString(akBuf[:])),
		SecretKey:    base64.StdEncoding.EncodeToString(skBuf[:]),
		SessionToken: base64.StdEncoding.EncodeToString(stBuf[:]),
		Owner:        owner,
		Expiration:   s.now().Add(ttl).UTC(),
	}
	s.sess[sess.AccessKey] = sess
	return sess, nil
}

// Lookup implements CredentialsStore. Returns ErrNoSuchCredential when the
// access key is not from a known session, ErrExpiredToken when the session is
// expired (and evicts it).
func (s *STSStore) Lookup(_ context.Context, accessKey string) (*Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sess[accessKey]
	if !ok {
		return nil, ErrNoSuchCredential
	}
	if !sess.Expiration.After(s.now()) {
		delete(s.sess, accessKey)
		return nil, ErrExpiredToken
	}
	return &Credential{
		AccessKey:    sess.AccessKey,
		Secret:       sess.SecretKey,
		Owner:        sess.Owner,
		SessionToken: sess.SessionToken,
	}, nil
}

// Revoke drops a session early so the next Lookup returns ErrNoSuchCredential.
func (s *STSStore) Revoke(accessKey string) {
	s.mu.Lock()
	delete(s.sess, accessKey)
	s.mu.Unlock()
}
