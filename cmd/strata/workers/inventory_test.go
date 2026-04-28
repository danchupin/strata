package workers

import (
	"testing"
	"time"

	datamem "github.com/danchupin/strata/internal/data/memory"
	"github.com/danchupin/strata/internal/inventory"
	metamem "github.com/danchupin/strata/internal/meta/memory"
)

func TestInventoryWorkerRegistered(t *testing.T) {
	w, ok := Lookup("inventory")
	if !ok {
		t.Fatal("inventory worker not registered (init() did not fire)")
	}
	if w.Name != "inventory" {
		t.Fatalf("name=%q want inventory", w.Name)
	}
}

func TestBuildInventoryReadsEnv(t *testing.T) {
	t.Setenv("STRATA_INVENTORY_INTERVAL", "9s")
	t.Setenv("STRATA_INVENTORY_REGION", "eu-west-1")

	r, err := buildInventory(Dependencies{Meta: metamem.New(), Data: datamem.New()})
	if err != nil {
		t.Fatalf("buildInventory: %v", err)
	}
	if _, ok := r.(*inventory.Worker); !ok {
		t.Fatalf("buildInventory returned %T, want *inventory.Worker", r)
	}
}

func TestBuildInventoryDefaultsWhenEnvUnset(t *testing.T) {
	t.Setenv("STRATA_INVENTORY_INTERVAL", "")
	t.Setenv("STRATA_INVENTORY_REGION", "")

	r, err := buildInventory(Dependencies{Meta: metamem.New(), Data: datamem.New()})
	if err != nil {
		t.Fatalf("buildInventory: %v", err)
	}
	if _, ok := r.(*inventory.Worker); !ok {
		t.Fatalf("buildInventory returned %T, want *inventory.Worker", r)
	}
}

func TestInventoryDefaultIntervalMatchesLegacy(t *testing.T) {
	want := 5 * time.Minute
	if got := durationFromEnv("STRATA_INVENTORY_INTERVAL_UNSET", want); got != want {
		t.Errorf("durationFromEnv default = %v, want %v", got, want)
	}
}

func TestInventoryRegionPrecedence(t *testing.T) {
	t.Setenv("STRATA_INVENTORY_REGION", "")
	if got := inventoryRegion(""); got != "default" {
		t.Errorf("inventoryRegion empty = %q, want default", got)
	}
	if got := inventoryRegion("us-east-2"); got != "us-east-2" {
		t.Errorf("inventoryRegion deps = %q, want us-east-2", got)
	}
	t.Setenv("STRATA_INVENTORY_REGION", "eu-west-1")
	if got := inventoryRegion("us-east-2"); got != "eu-west-1" {
		t.Errorf("inventoryRegion env wins = %q, want eu-west-1", got)
	}
}
