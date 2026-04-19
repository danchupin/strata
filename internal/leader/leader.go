package leader

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
)

var ErrNotLeader = errors.New("not leader")

type Locker interface {
	Acquire(ctx context.Context, name, holder string, ttl time.Duration) (bool, error)
	Renew(ctx context.Context, name, holder string, ttl time.Duration) (bool, error)
	Release(ctx context.Context, name, holder string) error
}

type Session struct {
	Locker       Locker
	Name         string
	Holder       string
	TTL          time.Duration
	RenewPeriod  time.Duration
	AcquireRetry time.Duration
	Logger       *log.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
}

func DefaultHolder() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s/%d/%s", host, os.Getpid(), uuid.NewString())
}

func (s *Session) applyDefaults() {
	if s.TTL == 0 {
		s.TTL = 30 * time.Second
	}
	if s.RenewPeriod == 0 {
		s.RenewPeriod = s.TTL / 3
	}
	if s.AcquireRetry == 0 {
		s.AcquireRetry = 5 * time.Second
	}
	if s.Logger == nil {
		s.Logger = log.Default()
	}
	if s.Holder == "" {
		s.Holder = DefaultHolder()
	}
}

// AwaitAcquire blocks until the lock is acquired, the parent context is cancelled,
// or an unrecoverable error occurs.
func (s *Session) AwaitAcquire(ctx context.Context) error {
	s.applyDefaults()
	for {
		ok, err := s.Locker.Acquire(ctx, s.Name, s.Holder, s.TTL)
		if err != nil {
			s.Logger.Printf("leader %s: acquire error: %v", s.Name, err)
		} else if ok {
			s.Logger.Printf("leader %s: acquired by %s", s.Name, s.Holder)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.AcquireRetry):
		}
	}
}

// Supervise returns a context that is cancelled if the lease is lost, and starts
// a background goroutine that renews the lease on RenewPeriod.
func (s *Session) Supervise(parent context.Context) context.Context {
	child, cancel := context.WithCancel(parent)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()

	go func() {
		ticker := time.NewTicker(s.RenewPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-child.Done():
				return
			case <-ticker.C:
				ok, err := s.Locker.Renew(parent, s.Name, s.Holder, s.TTL)
				if err != nil {
					s.Logger.Printf("leader %s: renew error: %v", s.Name, err)
				}
				if !ok {
					s.Logger.Printf("leader %s: lease lost", s.Name)
					cancel()
					return
				}
			}
		}
	}()
	return child
}

func (s *Session) Release(ctx context.Context) {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.mu.Unlock()
	if err := s.Locker.Release(ctx, s.Name, s.Holder); err != nil {
		s.Logger.Printf("leader %s: release: %v", s.Name, err)
	}
}
