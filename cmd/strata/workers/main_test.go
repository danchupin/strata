package workers

import (
	"maps"
	"os"
	"testing"
)

// initialReg snapshots the production registry populated by per-worker init()
// so tests that call Reset can restore the package back to its boot state via
// restoreInitial. Without this, any test running alphabetically after a
// registry_test.go cleanup would observe an empty registry and fail Lookup.
var initialReg map[string]Worker

func TestMain(m *testing.M) {
	regMu.RLock()
	initialReg = make(map[string]Worker, len(reg))
	maps.Copy(initialReg, reg)
	regMu.RUnlock()
	os.Exit(m.Run())
}

func restoreInitial() {
	regMu.Lock()
	reg = make(map[string]Worker, len(initialReg))
	maps.Copy(reg, initialReg)
	regMu.Unlock()
}
