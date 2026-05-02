package memory_test

import (
	"testing"

	"github.com/danchupin/strata/internal/meta"
	"github.com/danchupin/strata/internal/meta/memory"
	"github.com/danchupin/strata/internal/meta/storetest"
)

func TestMemoryStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) meta.Store { return memory.New() })
}

// TestMemoryStoreImplementsRangeScanStore confirms the memory backend
// advertises the optional meta.RangeScanStore capability surface (US-012).
// The compile-time assertion in store.go enforces the same; this is a
// runtime smoke test so a future refactor that breaks the interface
// contract surfaces here too.
func TestMemoryStoreImplementsRangeScanStore(t *testing.T) {
	var s meta.Store = memory.New()
	rs, ok := s.(meta.RangeScanStore)
	if !ok {
		t.Fatal("memory.Store must implement meta.RangeScanStore — see US-012")
	}
	if rs == nil {
		t.Fatal("type assertion returned nil RangeScanStore")
	}
}
