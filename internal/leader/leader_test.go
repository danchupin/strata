package leader_test

import (
	"context"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/leader"
	"github.com/danchupin/strata/internal/meta/memory"
)

func TestLeaderAcquireAndTakeover(t *testing.T) {
	locker := memory.NewLocker()
	a := &leader.Session{Locker: locker, Name: "test", Holder: "a", TTL: 50 * time.Millisecond, RenewPeriod: 20 * time.Millisecond, AcquireRetry: 5 * time.Millisecond}
	b := &leader.Session{Locker: locker, Name: "test", Holder: "b", TTL: 50 * time.Millisecond, RenewPeriod: 20 * time.Millisecond, AcquireRetry: 5 * time.Millisecond}

	ctxA := context.Background()
	if err := a.AwaitAcquire(ctxA); err != nil {
		t.Fatal(err)
	}

	bCtx, bCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer bCancel()
	if err := b.AwaitAcquire(bCtx); err == nil {
		t.Fatal("b should have timed out while a holds the lock")
	}

	a.Release(context.Background())

	ctxB := context.Background()
	if err := b.AwaitAcquire(ctxB); err != nil {
		t.Fatalf("b acquire after a released: %v", err)
	}
	b.Release(context.Background())
}

func TestLeaderSuperviseCancelsOnLoss(t *testing.T) {
	locker := memory.NewLocker()
	s := &leader.Session{
		Locker:       locker,
		Name:         "test",
		Holder:       "me",
		TTL:          40 * time.Millisecond,
		RenewPeriod:  10 * time.Millisecond,
		AcquireRetry: 5 * time.Millisecond,
	}
	if err := s.AwaitAcquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := s.Supervise(context.Background())

	// Forcibly expire our lock by releasing it under the hood.
	_ = locker.Release(context.Background(), "test", "me")

	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("supervise context did not cancel after lock was forcefully released")
	}
}
