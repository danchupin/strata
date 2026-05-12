package rebalance

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/danchupin/strata/internal/meta"
)

// fakeMover captures the moves dispatched to it so the chain test can
// assert routing.
type fakeMover struct {
	owns   map[string]bool
	got    []Move
	failOn map[string]bool
}

func (f *fakeMover) Owns(target string) bool { return f.owns[target] }

func (f *fakeMover) Move(_ context.Context, plan []Move) error {
	f.got = append(f.got, plan...)
	for _, mv := range plan {
		if f.failOn[mv.ToCluster] {
			return errors.New("forced failure")
		}
	}
	return nil
}

func TestMoverChainPartitionsByOwner(t *testing.T) {
	a := &fakeMover{owns: map[string]bool{"alpha": true}}
	b := &fakeMover{owns: map[string]bool{"beta": true}}
	chain := &MoverChain{Movers: []Mover{a, b}, Logger: slog.Default()}
	plan := []Move{
		{Bucket: "bkt", ToCluster: "alpha", ChunkIdx: 0, BucketID: uuid.New()},
		{Bucket: "bkt", ToCluster: "beta", ChunkIdx: 1, BucketID: uuid.New()},
		{Bucket: "bkt", ToCluster: "alpha", ChunkIdx: 2, BucketID: uuid.New()},
	}
	if err := chain.EmitPlan(context.Background(), &meta.Bucket{Name: "bkt", ID: uuid.New()}, nil, nil, plan); err != nil {
		t.Fatalf("EmitPlan: %v", err)
	}
	if len(a.got) != 2 || len(b.got) != 1 {
		t.Fatalf("partition: alpha=%d beta=%d want 2/1", len(a.got), len(b.got))
	}
	for _, mv := range a.got {
		if mv.ToCluster != "alpha" {
			t.Errorf("alpha got %q", mv.ToCluster)
		}
	}
	if b.got[0].ToCluster != "beta" {
		t.Errorf("beta got %q", b.got[0].ToCluster)
	}
}

func TestMoverChainDropsOrphans(t *testing.T) {
	m := &fakeMover{owns: map[string]bool{"known": true}}
	chain := &MoverChain{Movers: []Mover{m}, Logger: slog.Default()}
	plan := []Move{
		{Bucket: "bkt", ToCluster: "known", BucketID: uuid.New()},
		{Bucket: "bkt", ToCluster: "orphan", BucketID: uuid.New()},
	}
	if err := chain.EmitPlan(context.Background(), &meta.Bucket{Name: "bkt", ID: uuid.New()}, nil, nil, plan); err != nil {
		t.Fatalf("EmitPlan: %v", err)
	}
	if len(m.got) != 1 || m.got[0].ToCluster != "known" {
		t.Fatalf("orphan should be dropped; mover got %#v", m.got)
	}
}

func TestMoverChainEmptyMoversLogsOnly(t *testing.T) {
	chain := &MoverChain{Logger: slog.Default()}
	// Should not panic / not error with zero movers.
	if err := chain.EmitPlan(context.Background(), &meta.Bucket{Name: "b", ID: uuid.New()}, nil, nil, []Move{
		{Bucket: "b", ToCluster: "x", BucketID: uuid.New()},
	}); err != nil {
		t.Fatalf("EmitPlan: %v", err)
	}
}

func TestMoverChainMoverFailureDoesNotAbortOthers(t *testing.T) {
	a := &fakeMover{owns: map[string]bool{"alpha": true}, failOn: map[string]bool{"alpha": true}}
	b := &fakeMover{owns: map[string]bool{"beta": true}}
	chain := &MoverChain{Movers: []Mover{a, b}, Logger: slog.Default()}
	plan := []Move{
		{Bucket: "bkt", ToCluster: "alpha", BucketID: uuid.New()},
		{Bucket: "bkt", ToCluster: "beta", BucketID: uuid.New()},
	}
	if err := chain.EmitPlan(context.Background(), &meta.Bucket{Name: "bkt", ID: uuid.New()}, nil, nil, plan); err != nil {
		t.Fatalf("EmitPlan should not surface mover failure; got %v", err)
	}
	if len(b.got) != 1 {
		t.Fatalf("beta should still have run; got %#v", b.got)
	}
}
