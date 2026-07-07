// Command didbill runs the anniversary-billing pass once and exits.
// Scheduled by deploy/central/systemd/didbill.timer (hourly).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"didstorage/internal/billing"
	"didstorage/internal/config"
	"didstorage/internal/db"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	pg, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "db: %v\n", err)
		os.Exit(2)
	}
	defer pg.Close()

	res, err := billing.Run(ctx, pg.Pool, logger.With("component", "billing"), time.Now().UTC())
	if err != nil {
		logger.Error("billing run failed", "err", err)
		os.Exit(1)
	}
	logger.Info("billing run complete",
		"processed", res.Processed,
		"charged", res.Charged,
		"downgraded", res.Downgrade,
		"suspended", res.Suspended,
		"errors", res.Errors,
	)
}
