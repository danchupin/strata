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
