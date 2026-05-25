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
	cfg := workerCfg(deps)
	invCfg := cfg.Workers.Inventory
	return inventory.New(inventory.Config{
		Meta:     deps.Meta,
		Data:     deps.Data,
		Logger:   deps.Logger,
		Interval: orDuration(invCfg.Interval, 5*time.Minute),
		Region:   inventoryRegion(invCfg.Region, deps.Region),
		Tracer:   deps.Tracer.Tracer("strata.worker.inventory"),
	})
}

func inventoryRegion(cfgRegion, depsRegion string) string {
	if cfgRegion != "" {
		return cfgRegion
	}
	if depsRegion != "" {
		return depsRegion
	}
	return "default"
}
