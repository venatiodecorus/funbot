// Funbot is a distributed IRC bot system with a controller/worker architecture.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/venatiodecorus/funbot/internal/config"
	"github.com/venatiodecorus/funbot/internal/controller"
	fnredis "github.com/venatiodecorus/funbot/internal/redis"
	"github.com/venatiodecorus/funbot/internal/worker"
)

func main() {
	// Parse flags
	role := flag.String("role", "", "Role to run: controller or worker")
	network := flag.String("network", "", "Network to connect to (worker role)")
	configPath := flag.String("config", "", "Path to config file")
	flag.Parse()

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	// Flag/env overrides
	if *role != "" {
		cfg.Role = *role
	}
	if envNetwork := os.Getenv("FUNBOT_NETWORK"); envNetwork != "" && *network == "" {
		*network = envNetwork
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

	// Connect to Redis
	redisClient, err := fnredis.New(cfg.Redis, log)
	if err != nil {
		log.Error("failed to create redis client", "error", err)
		os.Exit(1)
	}
	defer redisClient.Close()

	if err := redisClient.Ping(ctx); err != nil {
		log.Warn("redis not reachable, continuing without redis", "error", err)
		// For controller, we can still run in standalone mode.
		// For worker, Redis is required.
		if cfg.Role == "worker" {
			log.Error("worker requires redis connection")
			os.Exit(1)
		}
	}

	// Run the appropriate role
	switch cfg.Role {
	case "controller":
		if err := runController(ctx, cfg, redisClient, log); err != nil {
			log.Error("controller exited with error", "error", err)
			os.Exit(1)
		}
	case "worker":
		if *network == "" {
			log.Error("worker requires --network flag or FUNBOT_NETWORK env var")
			os.Exit(1)
		}
		if err := runWorker(ctx, cfg, *network, redisClient, log); err != nil {
			log.Error("worker exited with error", "error", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown role: %s\n", cfg.Role)
		os.Exit(1)
	}
}

func runController(ctx context.Context, cfg *config.Config, redis *fnredis.Client, log *slog.Logger) error {
	ctrl, err := controller.New(cfg, redis, log)
	if err != nil {
		return err
	}
	return ctrl.Run(ctx)
}

func runWorker(ctx context.Context, cfg *config.Config, network string, redis *fnredis.Client, log *slog.Logger) error {
	w, err := worker.New(cfg, network, redis, log)
	if err != nil {
		return err
	}
	return w.Run(ctx)
}
