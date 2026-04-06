package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/venatiodecorus/funbot/internal/config"
	"github.com/venatiodecorus/funbot/internal/irc"
	"github.com/venatiodecorus/funbot/internal/nick"
	"github.com/venatiodecorus/funbot/internal/proxy"
)

// ClientState holds the state of a single IRC client for status reporting.
type ClientState struct {
	ID        string
	Nick      string
	Connected bool
	Channels  []string
	Proxy     string
	KeepNick  string
}

// NetworkState holds the state of all clients on a network.
type NetworkState struct {
	Network string
	Clients []ClientState
}

// NetworkManager manages multiple IRC client connections to a single network.
type NetworkManager struct {
	network    string
	netCfg     config.Network
	proxyPool  *proxy.Pool
	floodGuard *irc.GlobalFloodGuard
	nickGen    nick.Generator
	clients    []*irc.Client
	keepNicks  map[string]*irc.KeepNick // client ID -> keepnick manager
	ctx        context.Context
	mu         sync.RWMutex
	log        *slog.Logger
}

// NewNetworkManager creates a new network manager for the given network.
func NewNetworkManager(network string, netCfg config.Network, proxyPool *proxy.Pool, log *slog.Logger) (*NetworkManager, error) {
	nmLog := log.With("component", "network", "network", network)

	// Create a global flood guard for this network.
	floodDelay := netCfg.FloodDelay()
	if floodDelay <= 0 {
		floodDelay = 500 * time.Millisecond
	}
	guardRefill := floodDelay / 10
	if guardRefill < 50*time.Millisecond {
		guardRefill = 50 * time.Millisecond
	}
	floodGuard := irc.NewGlobalFloodGuard(10, guardRefill, nmLog)

	// Create nick generator from network config.
	nickCfg := netCfg.EffectiveNickConfig()
	nickGen, err := nick.NewGenerator(nick.Config{
		Strategy:     nick.Strategy(nickCfg.Strategy),
		Prefix:       nickCfg.Prefix,
		Length:       nickCfg.Length,
		WordlistPath: nickCfg.WordlistPath,
	})
	if err != nil {
		return nil, fmt.Errorf("creating nick generator for network %s: %w", network, err)
	}

	return &NetworkManager{
		network:    network,
		netCfg:     netCfg,
		proxyPool:  proxyPool,
		floodGuard: floodGuard,
		nickGen:    nickGen,
		keepNicks:  make(map[string]*irc.KeepNick),
		log:        nmLog,
	}, nil
}

// Start initializes the network manager and connects the default number of clients.
func (nm *NetworkManager) Start(ctx context.Context) error {
	nm.ctx = ctx

	count := nm.netCfg.DefaultClients
	if count <= 0 {
		count = 1
	}

	nm.log.Info("starting network manager", "default_clients", count)
	_, err := nm.AddClients(count)
	return err
}

// AddClients creates and connects additional IRC clients using proxies.
// Returns the number of clients actually added. When the proxy pool runs
// low, it attempts to fetch more from the API on demand before giving up.
func (nm *NetworkManager) AddClients(count int) (int, error) {
	if count <= 0 {
		return 0, fmt.Errorf("count must be > 0")
	}

	if nm.proxyPool == nil {
		return 0, fmt.Errorf("no proxy pool configured")
	}

	server, port, err := parseServerAddr(nm.netCfg.Servers[0])
	if err != nil {
		return 0, fmt.Errorf("parsing server address: %w", err)
	}

	// Try to ensure enough proxies are available before starting
	ctx := nm.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if avail, err := nm.proxyPool.EnsureAvailable(ctx, nm.network, count); err != nil {
		nm.log.Warn("failed to fetch additional proxies on demand",
			"error", err, "available", avail, "requested", count)
	}

	added := 0
	for i := 0; i < count; i++ {
		px := nm.proxyPool.AcquireForNetwork(nm.network)
		if px == nil {
			if added == 0 {
				return 0, fmt.Errorf("no proxies available for network %s", nm.network)
			}
			nm.log.Warn("ran out of proxies, added fewer clients than requested",
				"requested", count, "added", added)
			break
		}

		nm.mu.Lock()
		clientIndex := len(nm.clients)
		nm.mu.Unlock()

		clientID := fmt.Sprintf("%s-%d", nm.network, clientIndex)
		nick := nm.nickGen.Generate(clientIndex)

		cfg := irc.ClientConfig{
			ID:         clientID,
			Network:    nm.network,
			Server:     server,
			Port:       port,
			SSL:        nm.netCfg.SSL,
			Nick:       nick,
			User:       "funbot",
			Realname:   "Funbot",
			Logger:     nm.log,
			FloodDelay: nm.netCfg.FloodDelay(),
			ProxyAddr:  px.ProxyAddress(),
			ProxyUser:  px.User,
			ProxyPass:  px.Pass,
		}

		client := irc.New(cfg)

		// Auto-join configured channels on connect
		channels := nm.netCfg.Channels
		client.OnConnect(func() {
			for _, ch := range channels {
				client.Join(ch)
			}
		})

		nm.mu.Lock()
		nm.clients = append(nm.clients, client)
		nm.mu.Unlock()

		go nm.runClientWithProxyRotation(client, clientID, px.ProxyAddress())

		added++
	}

	return added, nil
}

// runClientWithProxyRotation manages a client's connection lifecycle with
// proxy rotation. It connects via ConnectWithRetry using the pool's
// maxRetries as the per-proxy attempt limit. When retries are exhausted
// the current proxy is released as failed and a new one is acquired. This
// continues until the context is cancelled or no proxies are available.
func (nm *NetworkManager) runClientWithProxyRotation(client *irc.Client, clientID, proxyAddr string) {
	maxRetries := nm.proxyPool.MaxRetries()
	if maxRetries <= 0 {
		maxRetries = 1
	}

	currentProxy := proxyAddr

	for {
		if nm.ctx.Err() != nil {
			return
		}

		nm.log.Info("connecting client via proxy",
			"client_id", clientID, "proxy", currentProxy)

		err := client.ConnectWithRetry(nm.ctx, maxRetries)
		if nm.ctx.Err() != nil {
			return
		}

		// Retries exhausted for this proxy — release it as failed
		nm.log.Warn("proxy retries exhausted, rotating proxy",
			"client_id", clientID,
			"proxy", currentProxy,
			"max_retries", maxRetries,
			"error", err,
		)
		nm.proxyPool.ReleaseByAddressFromNetwork(currentProxy, nm.network, true)

		// Try to get a new proxy (with on-demand fetch)
		ctx := nm.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		if _, err := nm.proxyPool.EnsureAvailable(ctx, nm.network, 1); err != nil {
			nm.log.Warn("failed to fetch proxies for rotation",
				"client_id", clientID, "error", err)
		}

		newPx := nm.proxyPool.AcquireForNetwork(nm.network)
		if newPx == nil {
			nm.log.Error("no proxies available for rotation, client giving up",
				"client_id", clientID, "network", nm.network)
			return
		}

		currentProxy = newPx.ProxyAddress()
		client.SetProxy(currentProxy, newPx.User, newPx.Pass)
		nm.log.Info("rotated to new proxy",
			"client_id", clientID, "proxy", currentProxy)
	}
}

// RemoveClients disconnects and removes clients from this network.
// Removes from the end of the client list (most recently added first).
// Returns the number of clients actually removed.
func (nm *NetworkManager) RemoveClients(count int) int {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	if count <= 0 || count > len(nm.clients) {
		count = len(nm.clients)
	}

	removed := 0
	for i := 0; i < count; i++ {
		idx := len(nm.clients) - 1
		client := nm.clients[idx]
		nm.clients = nm.clients[:idx]

		// Stop keepnick if active
		if kn, ok := nm.keepNicks[client.ID()]; ok {
			kn.Stop()
			delete(nm.keepNicks, client.ID())
		}

		// Release proxy
		if client.ProxyAddr() != "" && nm.proxyPool != nil {
			nm.proxyPool.ReleaseByAddressFromNetwork(client.ProxyAddr(), nm.network, false)
		}

		client.Quit("removed")
		removed++
	}

	return removed
}

// Stop gracefully disconnects all clients and releases all proxies.
func (nm *NetworkManager) Stop() {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	// Stop all keepnick goroutines first
	for id, kn := range nm.keepNicks {
		kn.Stop()
		nm.log.Debug("stopped keepnick", "client_id", id)
	}
	nm.keepNicks = make(map[string]*irc.KeepNick)

	for _, client := range nm.clients {
		// Release proxy
		if client.ProxyAddr() != "" && nm.proxyPool != nil {
			nm.proxyPool.ReleaseByAddressFromNetwork(client.ProxyAddr(), nm.network, false)
		}
		client.Quit("shutting down")
	}
	nm.clients = nil
	nm.log.Info("all clients stopped")
}

// Clients returns a snapshot of all managed clients.
func (nm *NetworkManager) Clients() []*irc.Client {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	result := make([]*irc.Client, len(nm.clients))
	copy(result, nm.clients)
	return result
}

// ConnectedClients returns only clients that are currently connected.
func (nm *NetworkManager) ConnectedClients() []*irc.Client {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	var result []*irc.Client
	for _, c := range nm.clients {
		if c.IsConnected() {
			result = append(result, c)
		}
	}
	return result
}

// ClientByID returns a specific client by its ID.
func (nm *NetworkManager) ClientByID(id string) *irc.Client {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	for _, c := range nm.clients {
		if c.ID() == id {
			return c
		}
	}
	return nil
}

// SelectClients returns up to `count` connected clients.
// If count <= 0 or count > available, returns all connected clients.
func (nm *NetworkManager) SelectClients(count int) []*irc.Client {
	connected := nm.ConnectedClients()
	if count <= 0 || count > len(connected) {
		return connected
	}
	return connected[:count]
}

// StartKeepNick starts a keepnick process for a specific client.
func (nm *NetworkManager) StartKeepNick(clientID, desiredNick string) string {
	client := nm.ClientByID(clientID)
	if client == nil {
		return fmt.Sprintf("client %s not found", clientID)
	}

	nm.mu.Lock()
	// Stop existing keepnick for this client if any
	if existing, ok := nm.keepNicks[clientID]; ok {
		existing.Stop()
	}

	kn := irc.NewKeepNick(client, desiredNick, nm.log)
	nm.keepNicks[clientID] = kn
	nm.mu.Unlock()

	kn.Start(nm.ctx)
	return fmt.Sprintf("keepnick started for %s -> %s", clientID, desiredNick)
}

// StopKeepNick stops the keepnick process for a client.
func (nm *NetworkManager) StopKeepNick(clientID string) string {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	kn, ok := nm.keepNicks[clientID]
	if !ok {
		return fmt.Sprintf("no keepnick active for %s", clientID)
	}

	kn.Stop()
	delete(nm.keepNicks, clientID)
	return fmt.Sprintf("keepnick stopped for %s", clientID)
}

// GetKeepNick returns the desired nick for a client's keepnick, or empty.
func (nm *NetworkManager) GetKeepNick(clientID string) string {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	kn, ok := nm.keepNicks[clientID]
	if !ok {
		return ""
	}
	return kn.DesiredNick()
}

// GetState returns the current state of all clients for status reporting.
func (nm *NetworkManager) GetState() NetworkState {
	nm.mu.RLock()
	defer nm.mu.RUnlock()

	var clients []ClientState
	for _, c := range nm.clients {
		cs := ClientState{
			ID:        c.ID(),
			Nick:      c.Nick(),
			Connected: c.IsConnected(),
			Channels:  c.Channels(),
			Proxy:     c.ProxyAddr(),
		}
		if kn, ok := nm.keepNicks[c.ID()]; ok && kn.IsActive() {
			cs.KeepNick = kn.DesiredNick()
		}
		clients = append(clients, cs)
	}

	return NetworkState{
		Network: nm.network,
		Clients: clients,
	}
}

// FloodGuard returns the global flood guard for this network.
func (nm *NetworkManager) FloodGuard() *irc.GlobalFloodGuard {
	return nm.floodGuard
}

// Network returns the network name.
func (nm *NetworkManager) Network() string {
	return nm.network
}

// Config returns the network configuration.
func (nm *NetworkManager) Config() config.Network {
	return nm.netCfg
}

// parseServerAddr splits "host:port" into components.
func parseServerAddr(addr string) (string, int, error) {
	parts := strings.SplitN(addr, ":", 2)
	if len(parts) != 2 {
		return addr, 6667, nil // default IRC port
	}
	port, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, fmt.Errorf("invalid port in %q: %w", addr, err)
	}
	return parts[0], port, nil
}
