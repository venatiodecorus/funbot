package worker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/venatiodecorus/funbot/internal/art"
	"github.com/venatiodecorus/funbot/internal/config"
	"github.com/venatiodecorus/funbot/internal/health"
	"github.com/venatiodecorus/funbot/internal/proxy"
	fnredis "github.com/venatiodecorus/funbot/internal/redis"
)

// StatusInterval is how often the worker reports its state to Redis.
const StatusInterval = 10 * time.Second

// Worker manages IRC clients for a single network and communicates
// with the controller via Redis.
type Worker struct {
	cfg        *config.Config
	network    string
	redis      *fnredis.Client
	cm         *ClientManager
	executor   *Executor
	proxyPool  *proxy.Pool
	artRepo    *art.Repo
	artCatalog *art.Catalog
	log        *slog.Logger
}

// New creates a new Worker for the given network.
func New(cfg *config.Config, network string, redisClient *fnredis.Client, log *slog.Logger) (*Worker, error) {
	netCfg, ok := cfg.Networks[network]
	if !ok {
		return nil, fmt.Errorf("network %q not found in config", network)
	}

	podName := getPodName()

	// Set up proxy pool
	proxyPool := proxy.NewPool(log)
	if cfg.Proxies.File != "" {
		if err := proxyPool.LoadFromFile(cfg.Proxies.File); err != nil {
			log.Warn("failed to load proxy file", "error", err)
		}
	}
	if len(cfg.Proxies.List) > 0 {
		if err := proxyPool.LoadFromList(cfg.Proxies.List); err != nil {
			log.Warn("failed to load proxy list", "error", err)
		}
	}

	cm := NewClientManager(network, netCfg, podName, proxyPool, log)

	// Set up art repo and catalog
	var artRepo *art.Repo
	var artCatalog *art.Catalog
	if cfg.Art.RepoURL != "" && cfg.Art.LocalPath != "" {
		interval, err := time.ParseDuration(cfg.Art.UpdateInterval)
		if err != nil {
			interval = 1 * time.Hour
		}
		artRepo = art.NewRepo(cfg.Art.RepoURL, cfg.Art.LocalPath, interval, log)
		artCatalog = art.NewCatalog(cfg.Art.LocalPath, log)
	}

	return &Worker{
		cfg:        cfg,
		network:    network,
		redis:      redisClient,
		cm:         cm,
		proxyPool:  proxyPool,
		artRepo:    artRepo,
		artCatalog: artCatalog,
		log:        log.With("component", "worker", "network", network, "pod", podName),
	}, nil
}

// Run starts the worker: connects clients, subscribes to commands,
// and begins status reporting. Blocks until context is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	w.log.Info("starting worker")

	// Create a derived context so disconnect commands can trigger shutdown
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Initialize art repo if configured
	if w.artRepo != nil {
		if err := w.artRepo.Init(ctx); err != nil {
			w.log.Warn("failed to initialize art repo", "error", err)
		} else {
			// Index the art files
			if err := w.artCatalog.Refresh(); err != nil {
				w.log.Warn("failed to refresh art catalog", "error", err)
			}
			// Start background updater
			go func() {
				w.artRepo.StartUpdater(ctx)
			}()
		}
	}

	// Start proxy health checker if we have proxies
	if w.proxyPool != nil && w.proxyPool.Count() > 0 {
		go w.proxyPool.StartHealthChecker(ctx)
	}

	// Create executor with the context and cancel function (needed for keepnick and disconnect)
	w.executor = NewExecutor(ctx, cancel, w.cm, w.artCatalog, w.cfg, w.log)

	// Start health check server
	healthSrv := health.New("", w.isReady, w.log)
	healthSrv.Start()
	defer healthSrv.Shutdown()

	// Start IRC clients
	if err := w.cm.Start(ctx); err != nil {
		return fmt.Errorf("starting client manager: %w", err)
	}
	defer w.cm.Stop()

	// Start status reporting
	go w.reportStatus(ctx)

	// Main loop: subscribe to commands with reconnection
	w.log.Info("worker ready, listening for commands")
	return w.commandLoop(ctx)
}

// commandLoop subscribes to Redis commands and processes them.
// It automatically resubscribes if the connection is lost.
func (w *Worker) commandLoop(ctx context.Context) error {
	const resubscribeDelay = 3 * time.Second

	for {
		if ctx.Err() != nil {
			w.log.Info("worker shutting down")
			w.cleanupState()
			return nil
		}

		cmdCh, err := w.redis.SubscribeCommands(ctx, w.network)
		if err != nil {
			if ctx.Err() != nil {
				w.cleanupState()
				return nil
			}
			w.log.Error("failed to subscribe to commands, retrying", "error", err, "delay", resubscribeDelay)
			select {
			case <-ctx.Done():
				w.cleanupState()
				return nil
			case <-time.After(resubscribeDelay):
				continue
			}
		}

		w.log.Info("subscribed to command channel", "network", w.network)

		// Process commands until channel closes
		for {
			select {
			case <-ctx.Done():
				w.log.Info("worker shutting down")
				w.cleanupState()
				return nil
			case cmd, ok := <-cmdCh:
				if !ok {
					w.log.Warn("command channel closed, resubscribing", "delay", resubscribeDelay)
					select {
					case <-ctx.Done():
						w.cleanupState()
						return nil
					case <-time.After(resubscribeDelay):
					}
					break // break inner for, resubscribe
				}

				ack := w.executor.Execute(cmd)
				if err := w.redis.PublishAck(ctx, ack); err != nil {
					w.log.Error("failed to publish ack", "error", err, "command_id", cmd.ID)
				}
				continue // stay in inner loop
			}
			break // resubscribe
		}
	}
}

// cleanupState removes this worker's state from Redis on shutdown.
func (w *Worker) cleanupState() {
	if err := w.redis.DeleteState(context.Background(), w.network, w.cm.PodName()); err != nil {
		w.log.Error("failed to delete state on shutdown", "error", err)
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

// isReady returns true when at least one IRC client is connected.
func (w *Worker) isReady() bool {
	return len(w.cm.ConnectedClients()) > 0
}

// getPodName returns the pod name from the environment or a default.
func getPodName() string {
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
