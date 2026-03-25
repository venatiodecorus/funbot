// Package worker implements the Funbot worker role, which manages
// multiple IRC client connections to a single network.
package worker

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/venatiodecorus/funbot/internal/config"
	"github.com/venatiodecorus/funbot/internal/irc"
	fnredis "github.com/venatiodecorus/funbot/internal/redis"
)

// ClientManager manages multiple IRC client connections to a single network.
type ClientManager struct {
	network string
	netCfg  config.Network
	podName string
	clients []*irc.Client
	mu      sync.RWMutex
	log     *slog.Logger
}

// NewClientManager creates a new client manager for the given network.
func NewClientManager(network string, netCfg config.Network, podName string, log *slog.Logger) *ClientManager {
	return &ClientManager{
		network: network,
		netCfg:  netCfg,
		podName: podName,
		log:     log.With("component", "clientmgr", "network", network),
	}
}

// Start creates and connects the configured number of IRC clients.
// Each client runs in its own goroutine. The method blocks until all
// clients have attempted their initial connection.
func (cm *ClientManager) Start(ctx context.Context) error {
	maxClients := cm.netCfg.MaxClientsPerIP
	if maxClients <= 0 {
		maxClients = 1
	}

	cm.log.Info("starting clients", "count", maxClients)

	var wg sync.WaitGroup
	errCh := make(chan error, maxClients)

	for i := 0; i < maxClients; i++ {
		clientID := fmt.Sprintf("%s-%d", cm.network, i)
		nick := fmt.Sprintf("%s%d", cm.netCfg.NickPrefix, i)

		server, port, err := parseServerAddr(cm.netCfg.Servers[0])
		if err != nil {
			return fmt.Errorf("parsing server address: %w", err)
		}

		cfg := irc.ClientConfig{
			ID:       clientID,
			Network:  cm.network,
			Server:   server,
			Port:     port,
			SSL:      cm.netCfg.SSL,
			Nick:     nick,
			User:     "funbot",
			Realname: "Funbot Worker",
			Logger:   cm.log,
		}

		client := irc.New(cfg)

		// Auto-join configured channels on connect
		channels := cm.netCfg.Channels
		client.OnConnect(func() {
			for _, ch := range channels {
				client.Join(ch)
			}
		})

		cm.mu.Lock()
		cm.clients = append(cm.clients, client)
		cm.mu.Unlock()

		wg.Add(1)
		go func(c *irc.Client, id string) {
			defer wg.Done()
			cm.log.Info("connecting client", "client_id", id)
			if err := c.Connect(ctx); err != nil {
				if ctx.Err() != nil {
					// Context cancelled, expected during shutdown
					return
				}
				cm.log.Error("client connection failed", "client_id", id, "error", err)
				errCh <- fmt.Errorf("client %s: %w", id, err)
			}
		}(client, clientID)
	}

	// Don't block waiting for all connections — they run in background.
	// Return immediately so the worker can start listening for commands.
	go func() {
		wg.Wait()
		close(errCh)
	}()

	return nil
}

// Stop disconnects all clients.
func (cm *ClientManager) Stop() {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	for _, client := range cm.clients {
		client.Close()
	}
	cm.log.Info("all clients stopped")
}

// Clients returns a snapshot of all managed clients.
func (cm *ClientManager) Clients() []*irc.Client {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	result := make([]*irc.Client, len(cm.clients))
	copy(result, cm.clients)
	return result
}

// ConnectedClients returns only clients that are currently connected.
func (cm *ClientManager) ConnectedClients() []*irc.Client {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	var result []*irc.Client
	for _, c := range cm.clients {
		if c.IsConnected() {
			result = append(result, c)
		}
	}
	return result
}

// ClientByID returns a specific client by its ID.
func (cm *ClientManager) ClientByID(id string) *irc.Client {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	for _, c := range cm.clients {
		if c.ID() == id {
			return c
		}
	}
	return nil
}

// SelectClients returns up to `count` connected clients.
// If count <= 0 or count > available, returns all connected clients.
func (cm *ClientManager) SelectClients(count int) []*irc.Client {
	connected := cm.ConnectedClients()
	if count <= 0 || count > len(connected) {
		return connected
	}
	return connected[:count]
}

// GetState returns the current state of all clients for status reporting.
func (cm *ClientManager) GetState() fnredis.PodState {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	var clients []fnredis.ClientState
	for _, c := range cm.clients {
		cs := fnredis.ClientState{
			ID:        c.ID(),
			Nick:      c.Nick(),
			Connected: c.IsConnected(),
			Channels:  c.Channels(),
		}
		clients = append(clients, cs)
	}

	return fnredis.PodState{
		Pod:     cm.podName,
		Network: cm.network,
		Clients: clients,
	}
}

// Network returns the network name.
func (cm *ClientManager) Network() string {
	return cm.network
}

// PodName returns the pod name.
func (cm *ClientManager) PodName() string {
	return cm.podName
}

// parseServerAddr splits "host:port" into components.
func parseServerAddr(addr string) (string, int, error) {
	parts := strings.SplitN(addr, ":", 2)
	if len(parts) != 2 {
		return addr, 6667, nil
	}
	port, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, fmt.Errorf("invalid port in %q: %w", addr, err)
	}
	return parts[0], port, nil
}
