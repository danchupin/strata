package tikv

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/danchupin/strata/internal/data"
	"github.com/danchupin/strata/internal/meta"
)

// TestGCDualWriteConcurrentLockstep is the US-012 anchor for the GC dual-write
// invariant under concurrency at the TiKV wire layer: with dual-write ON,
// every EnqueueChunkDeletion writes BOTH the v2 sharded row and the legacy
// region row, and every AckGCEntry deletes BOTH — even when many producers and
// ackers race. The check is at the physical prefix level: after a concurrent
// enqueue both prefixes hold exactly `total` rows; after a concurrent full
// drain both hold zero. A lockstep break (an ack that dropped one side but not
// the other, or an enqueue that landed only one side) surfaces as a row-count
// mismatch between the two prefixes or a non-empty post-drain prefix.
//
// Distinct from TestGCQueueDualWriteToggle (single-threaded toggle proof) —
// this drives the same surface under contention so a missing rollback / lost
// commit shows up as a leaked or orphaned row on exactly one side.
func TestGCDualWriteConcurrentLockstep(t *testing.T) {
	ctx := context.Background()
	s := openWithBackend(newMemBackend())
	s.gcDualWrite = true
	t.Cleanup(func() { _ = s.Close() })

	const (
		region = "r"
		total  = 120
	)

	// Concurrent enqueue: `total` producers, one unique-OID chunk each.
	var wg sync.WaitGroup
	for i := range total {
		wg.Add(1)
		go func() {
			defer wg.Done()
			oid := fmt.Sprintf("obj-%05d", i)
			err := s.EnqueueChunkDeletion(ctx, region, []data.ChunkRef{
				{Cluster: "c", Pool: "p", OID: oid, Size: int64(i + 1)},
			})
			if err != nil {
				t.Errorf("enqueue %s: %v", oid, err)
			}
		}()
	}
	wg.Wait()

	// Lockstep after concurrent enqueue: both physical prefixes hold `total`.
	if k := scanPrefix(t, s, GCQueueRegionPrefixV2(region)); len(k) != total {
		t.Fatalf("v2 rows after enqueue=%d want %d", len(k), total)
	}
	if k := scanPrefix(t, s, GCQueuePrefix(region)); len(k) != total {
		t.Fatalf("legacy rows after enqueue=%d want %d (dual-write out of lockstep)", len(k), total)
	}

	// ListGCEntries dedups the two sides — must surface each OID exactly once.
	entries, err := s.ListGCEntries(ctx, region, time.Now().Add(time.Hour), total+100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != total {
		t.Fatalf("deduped entries=%d want %d", len(entries), total)
	}
	seen := make(map[string]struct{}, total)
	for _, e := range entries {
		if _, dup := seen[e.Chunk.OID]; dup {
			t.Fatalf("oid %q listed twice after dedup", e.Chunk.OID)
		}
		seen[e.Chunk.OID] = struct{}{}
	}

	// Concurrent ack: each entry acked by exactly one of `ackers` goroutines.
	const ackers = 16
	jobs := make(chan meta.GCEntry, len(entries))
	for _, e := range entries {
		jobs <- e
	}
	close(jobs)
	var ackWG sync.WaitGroup
	for range ackers {
		ackWG.Add(1)
		go func() {
			defer ackWG.Done()
			for e := range jobs {
				if err := s.AckGCEntry(ctx, region, e); err != nil {
					t.Errorf("ack %s: %v", e.Chunk.OID, err)
				}
			}
		}()
	}
	ackWG.Wait()

	// Lockstep after concurrent ack: both physical prefixes fully drained.
	if k := scanPrefix(t, s, GCQueueRegionPrefixV2(region)); len(k) != 0 {
		t.Fatalf("v2 rows after drain=%d want 0 (leak)", len(k))
	}
	if k := scanPrefix(t, s, GCQueuePrefix(region)); len(k) != 0 {
		t.Fatalf("legacy rows after drain=%d want 0 (dual-write ack out of lockstep)", len(k))
	}
}
