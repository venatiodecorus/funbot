// Funbot is a distributed IRC bot system with a controller/worker architecture.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/venatiodecorus/funbot/internal/config"
	"github.com/venatiodecorus/funbot/internal/controller"
)

func main() {
	// Parse flags
	role := flag.String("role", "", "Role to run: controller or worker")
	configPath := flag.String("config", "", "Path to config file")
	flag.Parse()

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Role from flag overrides config
	if *role != "" {
		cfg.Role = *role
	}

	// Set up structured logging
	logLevel := slog.LevelInfo
	switch cfg.Logging.Level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	}

	var handler slog.Handler
	if cfg.Logging.Format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	} else {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel})
	}
	log := slog.New(handler)
	slog.SetDefault(log)

	log.Info("starting funbot", "role", cfg.Role)

	// Set up context with signal handling for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	// Run the appropriate role
	switch cfg.Role {
	case "controller":
		if err := runController(ctx, cfg, log); err != nil {
			log.Error("controller exited with error", "error", err)
			os.Exit(1)
		}
	case "worker":
		log.Info("worker role not yet implemented (Phase 2)")
		// Block until signal
		<-ctx.Done()
	default:
		log.Error("unknown role", "role", cfg.Role)
		os.Exit(1)
	}
}

func runController(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	ctrl, err := controller.New(cfg, log)
	if err != nil {
		return err
	}
	return ctrl.Run(ctx)
}
