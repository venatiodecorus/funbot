package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/venatiodecorus/funbot/internal/config"
	fnredis "github.com/venatiodecorus/funbot/internal/redis"
)

// StatusInterval is how often the worker reports its state to Redis.
const StatusInterval = 10 * time.Second

// Worker manages IRC clients for a single network and communicates
// with the controller via Redis.
type Worker struct {
	cfg      *config.Config
	network  string
	redis    *fnredis.Client
	cm       *ClientManager
	executor *Executor
	log      *slog.Logger
}

// New creates a new Worker for the given network.
func New(cfg *config.Config, network string, redisClient *fnredis.Client, log *slog.Logger) (*Worker, error) {
	netCfg, ok := cfg.Networks[network]
	if !ok {
		return nil, fmt.Errorf("network %q not found in config", network)
	}

	podName := getPodName()

	cm := NewClientManager(network, netCfg, podName, log)
	executor := NewExecutor(cm, log)

	return &Worker{
		cfg:      cfg,
		network:  network,
		redis:    redisClient,
		cm:       cm,
		executor: executor,
		log:      log.With("component", "worker", "network", network, "pod", podName),
	}, nil
}

// Run starts the worker: connects clients, subscribes to commands,
// and begins status reporting. Blocks until context is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	w.log.Info("starting worker")

	// Start IRC clients
	if err := w.cm.Start(ctx); err != nil {
		return fmt.Errorf("starting client manager: %w", err)
	}
	defer w.cm.Stop()

	// Subscribe to commands for this network
	cmdCh, err := w.redis.SubscribeCommands(ctx, w.network)
	if err != nil {
		return fmt.Errorf("subscribing to commands: %w", err)
	}

	// Start status reporting
	go w.reportStatus(ctx)

	// Main loop: process commands
	w.log.Info("worker ready, listening for commands")
	for {
		select {
		case <-ctx.Done():
			w.log.Info("worker shutting down")
			// Clean up state from Redis
			if err := w.redis.DeleteState(context.Background(), w.network, w.cm.PodName()); err != nil {
				w.log.Error("failed to delete state on shutdown", "error", err)
			}
			return nil

		case cmd, ok := <-cmdCh:
			if !ok {
				w.log.Warn("command channel closed")
				return nil
			}

			ack := w.executor.Execute(cmd)
			if err := w.redis.PublishAck(ctx, ack); err != nil {
				w.log.Error("failed to publish ack", "error", err, "command_id", cmd.ID)
			}
		}
	}
}

// reportStatus periodically publishes this worker's state to Redis.
func (w *Worker) reportStatus(ctx context.Context) {
	ticker := time.NewTicker(StatusInterval)
	defer ticker.Stop()

	// Report immediately on start
	w.publishState(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.publishState(ctx)
		}
	}
}

// publishState writes the current state to Redis.
func (w *Worker) publishState(ctx context.Context) {
	state := w.cm.GetState()
	if err := w.redis.SetState(ctx, state); err != nil {
		w.log.Error("failed to publish state", "error", err)
	}
}

// getPodName returns the pod name from the environment or a default.
func getPodName() string {
	// In Kubernetes, the pod name is typically in HOSTNAME
	if name := os.Getenv("HOSTNAME"); name != "" {
		return name
	}
	if name := os.Getenv("POD_NAME"); name != "" {
		return name
	}
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}
