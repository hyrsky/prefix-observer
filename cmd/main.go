package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/hyrsky/prefix-observer/internal/dhcpv6"
	"github.com/hyrsky/prefix-observer/internal/k8s"
	"github.com/hyrsky/prefix-observer/internal/observer"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg, err := observer.ConfigFromEnv()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	slog.Info("starting prefix-observer", "config", cfg)

	patcher, err := k8s.NewPatcher(cfg.DryRun)
	if err != nil {
		slog.Error("failed to create kubernetes patcher", "error", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	client := dhcpv6.NewClient(cfg.DHCPv6Interface)
	obs := observer.New(cfg, patcher, client, dhcpv6.WaitForInterface)
	if err := obs.Run(ctx); err != nil {
		slog.Error("observer exited with error", "error", err)
		os.Exit(1)
	}

	slog.Info("prefix-observer shut down gracefully")
}
