// Command rosetta follows L2 execution events through the RPC backend.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Rorical/Rosetta/internal/bot"
	"github.com/Rorical/Rosetta/internal/remote"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

func main() {
	// ROSETTA_LOG_LEVEL (debug, info, warn, error) drops records below it.
	// At Robinhood's 0.1 s blocks the per-search info records were most of
	// the journal; warn keeps warnings and errors. Unset keeps info.
	var level slog.Level
	if text := os.Getenv("ROSETTA_LOG_LEVEL"); text != "" {
		if err := level.UnmarshalText([]byte(text)); err != nil {
			slog.Error("invalid ROSETTA_LOG_LEVEL", "value", text)
			os.Exit(2)
		}
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	limit, err := watchdogLimit()
	if err != nil {
		logger.Error("invalid watchdog setting", "error", err)
		os.Exit(2)
	}
	if limit > 0 {
		beat := telemetry.NewHeartbeat()
		ctx = telemetry.WithHeartbeat(ctx, beat)
		go watch(ctx, beat, limit, logger)
	}
	err = bot.Run(ctx, os.Args[1:], os.Stderr, remote.Factory{})
	stop()
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		logger.Error("rosetta stopped", "error", err)
		os.Exit(1)
	}
}
