// strata-gateway is the legacy gateway binary; the body lives in
// internal/serverapp.Run, shared with the unified `strata server`
// subcommand. US-014 deletes this directory once worker migrations land.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/danchupin/strata/internal/config"
	"github.com/danchupin/strata/internal/logging"
	"github.com/danchupin/strata/internal/serverapp"
)

func main() {
	logger := logging.Setup()

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config", "error", err.Error())
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serverapp.Run(ctx, cfg, logger, nil); err != nil {
		logger.Error("strata-gateway", "error", err.Error())
		os.Exit(1)
	}
}
