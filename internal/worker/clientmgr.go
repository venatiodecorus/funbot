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
	"github.com/venatiodecorus/funbot/internal/proxy"
	fnredis "github.com/venatiodecorus/funbot/internal/redis"
)

// ClientManager manages multiple IRC client connections to a single network.
type ClientManager struct {
	network   string
	netCfg    config.Network
	podName   string
	proxyPool *proxy.Pool
	clients   []*irc.Client
	keepNicks map[string]*irc.KeepNick // client ID -> keepnick manager
	mu        sync.RWMutex
	log       *slog.Logger
}

// NewClientManager creates a new client manager for the given network.
func NewClientManager(network string, netCfg config.Network, podName string, proxyPool *proxy.Pool, log *slog.Logger) *ClientManager {
	return &ClientManager{
		network:   network,
		netCfg:    netCfg,
		podName:   podName,
		proxyPool: proxyPool,
		keepNicks: make(map[string]*irc.KeepNick),
		log:       log.With("component", "clientmgr", "network", network),
	}
}

// Start creates and connects the configured number of IRC clients.
// Direct connections are created up to max_clients_per_ip. If proxies
// are available, additional clients are created using proxies.
func (cm *ClientManager) Start(ctx context.Context) error {
	maxDirect := cm.netCfg.MaxClientsPerIP
	if maxDirect <= 0 {
		maxDirect = 1
	}

	// Determine how many proxy clients we can create
	proxyCount := 0
	if cm.proxyPool != nil {
		proxyCount = cm.proxyPool.HealthyCount()
	}

	totalClients := maxDirect + proxyCount
	cm.log.Info("starting clients", "direct", maxDirect, "proxied", proxyCount, "total", totalClients)

	server, port, err := parseServerAddr(cm.netCfg.Servers[0])
	if err != nil {
		return fmt.Errorf("parsing server address: %w", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, totalClients)

	for i := 0; i < totalClients; i++ {
		clientID := fmt.Sprintf("%s-%d", cm.network, i)
		nick := fmt.Sprintf("%s%d", cm.netCfg.NickPrefix, i)

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

		// Clients beyond max_clients_per_ip use proxies
		if i >= maxDirect && cm.proxyPool != nil {
			px := cm.proxyPool.Acquire()
			if px != nil {
				cfg.ProxyAddr = px.ProxyAddress()
				cfg.ProxyUser = px.Username
				cfg.ProxyPass = px.Password
				cm.log.Info("assigning proxy to client", "client_id", clientID, "proxy", px.ProxyAddress())
			}
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
					return
				}
				cm.log.Error("client connection failed", "client_id", id, "error", err)
				// If this was a proxied connection, mark the proxy as failed
				if c.ProxyAddr() != "" && cm.proxyPool != nil {
					for _, px := range cm.proxyPool.All() {
						if px.ProxyAddress() == c.ProxyAddr() {
							cm.proxyPool.Release(px, true)
							break
						}
					}
				}
				errCh <- fmt.Errorf("client %s: %w", id, err)
			}
		}(client, clientID)
	}

	// Don't block waiting for all connections — they run in background.
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

// StartKeepNick starts a keepnick process for a specific client.
func (cm *ClientManager) StartKeepNick(ctx context.Context, clientID, desiredNick string) string {
	client := cm.ClientByID(clientID)
	if client == nil {
		return fmt.Sprintf("client %s not found", clientID)
	}

	cm.mu.Lock()
	// Stop existing keepnick for this client if any
	if existing, ok := cm.keepNicks[clientID]; ok {
		existing.Stop()
	}

	kn := irc.NewKeepNick(client, desiredNick, cm.log)
	cm.keepNicks[clientID] = kn
	cm.mu.Unlock()

	kn.Start(ctx)
	return fmt.Sprintf("keepnick started for %s -> %s", clientID, desiredNick)
}

// StopKeepNick stops the keepnick process for a client.
func (cm *ClientManager) StopKeepNick(clientID string) string {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	kn, ok := cm.keepNicks[clientID]
	if !ok {
		return fmt.Sprintf("no keepnick active for %s", clientID)
	}

	kn.Stop()
	delete(cm.keepNicks, clientID)
	return fmt.Sprintf("keepnick stopped for %s", clientID)
}

// GetKeepNick returns the desired nick for a client's keepnick, or empty.
func (cm *ClientManager) GetKeepNick(clientID string) string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	kn, ok := cm.keepNicks[clientID]
	if !ok {
		return ""
	}
	return kn.DesiredNick()
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
			Proxy:     c.ProxyAddr(),
		}
		// Include keepnick info if active
		if kn, ok := cm.keepNicks[c.ID()]; ok && kn.IsActive() {
			cs.KeepNick = kn.DesiredNick()
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
