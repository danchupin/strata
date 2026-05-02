package workers

import (
	"time"

	"github.com/danchupin/strata/internal/inventory"
)

func init() {
	Register(Worker{
		Name:  "inventory",
		Build: buildInventory,
	})
}

func buildInventory(deps Dependencies) (Runner, error) {
	return inventory.New(inventory.Config{
		Meta:     deps.Meta,
		Data:     deps.Data,
		Logger:   deps.Logger,
		Interval: durationFromEnv("STRATA_INVENTORY_INTERVAL", 5*time.Minute),
		Region:   inventoryRegion(deps.Region),
	})
}

func inventoryRegion(depsRegion string) string {
	if v := stringFromEnv("STRATA_INVENTORY_REGION", ""); v != "" {
		return v
	}
	if depsRegion != "" {
		return depsRegion
	}
	return "default"
}
