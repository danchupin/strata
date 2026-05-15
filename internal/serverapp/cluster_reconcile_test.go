package serverapp

import (
	"context"
	"testing"

	"github.com/danchupin/strata/internal/meta"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

func TestReconcileClusters_NewPending(t *testing.T) {
	m := metamem.New()
	defer m.Close()
	in := ReconcileInput{
		EnvClusters:   []string{"alpha", "beta"},
		ClassDefaults: map[string]bool{},
		HasData:       false,
	}
	pending, live, err := ReconcileClusters(context.Background(), m, in, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if pending != 2 || live != 0 {
		t.Fatalf("counts: pending=%d live=%d want 2/0", pending, live)
	}
	row, ok, _ := m.GetClusterState(context.Background(), "alpha")
	if !ok || row.State != meta.ClusterStatePending || row.Weight != 0 {
		t.Errorf("alpha: %+v", row)
	}
}

func TestReconcileClusters_ExistingLiveViaClassDefault(t *testing.T) {
	m := metamem.New()
	defer m.Close()
	// Plant a bucket so HasData is true via the helper, then reconcile.
	_, err := m.CreateBucket(context.Background(), "bkt", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	// Bump bucket_stats so reconcileHasData returns true.
	b, _ := m.GetBucket(context.Background(), "bkt")
	if _, err := m.BumpBucketStats(context.Background(), b.ID, 1024, 1); err != nil {
		t.Fatalf("bump: %v", err)
	}
	in := ReconcileInput{
		EnvClusters:   []string{"primary"},
		ClassDefaults: map[string]bool{"primary": true},
		HasData:       reconcileHasData(context.Background(), m),
	}
	pending, live, err := ReconcileClusters(context.Background(), m, in, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if pending != 0 || live != 1 {
		t.Fatalf("counts: pending=%d live=%d want 0/1", pending, live)
	}
	row, _, _ := m.GetClusterState(context.Background(), "primary")
	if row.State != meta.ClusterStateLive || row.Weight != 100 {
		t.Errorf("primary row: %+v want=(live,100)", row)
	}
}

func TestReconcileClusters_Idempotent(t *testing.T) {
	m := metamem.New()
	defer m.Close()
	in := ReconcileInput{EnvClusters: []string{"alpha"}}
	if _, _, err := ReconcileClusters(context.Background(), m, in, nil); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Second run sees existing row → no-op (counts both 0).
	pending, live, err := ReconcileClusters(context.Background(), m, in, nil)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if pending != 0 || live != 0 {
		t.Errorf("idempotent re-run: pending=%d live=%d want 0/0", pending, live)
	}
}

func TestReconcileClusters_HasDataWithoutRefStaysPending(t *testing.T) {
	m := metamem.New()
	defer m.Close()
	_, err := m.CreateBucket(context.Background(), "bkt", "alice", "STANDARD")
	if err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	b, _ := m.GetBucket(context.Background(), "bkt")
	_, _ = m.BumpBucketStats(context.Background(), b.ID, 1, 1)
	in := ReconcileInput{
		EnvClusters:   []string{"newcomer"},
		ClassDefaults: map[string]bool{"primary": true}, // newcomer not in class env
		HasData:       reconcileHasData(context.Background(), m),
	}
	pending, live, err := ReconcileClusters(context.Background(), m, in, nil)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if pending != 1 || live != 0 {
		t.Errorf("counts: pending=%d live=%d want 1/0", pending, live)
	}
}
