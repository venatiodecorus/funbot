// Package proxy manages a pool of SOCKS5 proxies for IRC client connections.
package proxy

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

const (
	// DefaultHealthCheckInterval is how often to check unhealthy proxies.
	DefaultHealthCheckInterval = 2 * time.Minute

	// DefaultHealthCheckTimeout is the timeout for proxy health checks.
	DefaultHealthCheckTimeout = 10 * time.Second

	// DefaultRecoveryDelay is the minimum time after failure before retrying.
	DefaultRecoveryDelay = 1 * time.Minute
)

// Proxy represents a single SOCKS5 proxy.
type Proxy struct {
	URL      string
	Host     string
	Port     string
	Username string
	Password string
	Healthy  bool
	Networks map[string]bool // networks this proxy is currently connected to
	LastUsed time.Time
	LastFail time.Time
	UseCount int
}

// Pool manages a collection of proxies and handles assignment to clients.
type Pool struct {
	proxies    []*Proxy
	mu         sync.RWMutex
	log        *slog.Logger
	sourceFile string
}

// NewPool creates a new proxy pool.
func NewPool(log *slog.Logger) *Pool {
	return &Pool{
		log: log.With("component", "proxy"),
	}
}

// LoadFromFile reads proxies from a file, one per line.
// Format: socks5://host:port or socks5://user:pass@host:port
// Lines starting with # are comments. Empty lines are skipped.
func (p *Pool) LoadFromFile(path string) error {
	if path == "" {
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening proxy file %s: %w", path, err)
	}
	defer f.Close()

	p.mu.Lock()
	defer p.mu.Unlock()

	p.sourceFile = path
	p.proxies = nil

	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		proxy, err := parseProxy(line)
		if err != nil {
			p.log.Warn("skipping invalid proxy", "line", lineNum, "error", err)
			continue
		}

		p.proxies = append(p.proxies, proxy)
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading proxy file: %w", err)
	}

	p.log.Info("loaded proxies", "count", len(p.proxies), "file", path)
	return nil
}

// LoadFromList loads proxies from a string slice.
func (p *Pool) LoadFromList(list []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.proxies = nil

	for i, line := range list {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		proxy, err := parseProxy(line)
		if err != nil {
			p.log.Warn("skipping invalid proxy", "index", i, "error", err)
			continue
		}

		p.proxies = append(p.proxies, proxy)
	}

	p.log.Info("loaded proxies from list", "count", len(p.proxies))
	return nil
}

// Reload re-reads proxies from the original source file.
func (p *Pool) Reload() error {
	p.mu.RLock()
	file := p.sourceFile
	p.mu.RUnlock()

	if file == "" {
		return fmt.Errorf("no source file to reload from")
	}

	p.log.Info("reloading proxies", "file", file)
	return p.LoadFromFile(file)
}

// AcquireForNetwork returns the next available healthy proxy that is not
// already connected to the given network. A single proxy can serve
// connections to multiple different networks simultaneously.
// Returns nil if no suitable proxy is available.
func (p *Pool) AcquireForNetwork(network string) *Proxy {
	p.mu.Lock()
	defer p.mu.Unlock()

	// First pass: find least-recently-used healthy proxy NOT on this network
	var best *Proxy
	for _, px := range p.proxies {
		if !px.Healthy || px.Networks[network] {
			continue
		}
		if best == nil || px.LastUsed.Before(best.LastUsed) {
			best = px
		}
	}

	if best != nil {
		best.Networks[network] = true
		best.LastUsed = time.Now()
		best.UseCount++
		p.log.Debug("proxy acquired for network",
			"proxy", best.ProxyAddress(),
			"network", network,
			"use_count", best.UseCount,
			"available_for_network", p.availableForNetworkLocked(network),
		)
	} else {
		p.log.Warn("no healthy proxies available for network",
			"network", network,
			"total", len(p.proxies),
		)
	}

	return best
}

// ReleaseFromNetwork releases a proxy from a specific network.
// If failed is true, the proxy is also marked unhealthy.
func (p *Pool) ReleaseFromNetwork(px *Proxy, network string, failed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(px.Networks, network)

	if failed {
		px.Healthy = false
		px.LastFail = time.Now()
		p.log.Warn("proxy released from network and marked unhealthy",
			"proxy", px.ProxyAddress(),
			"network", network,
		)
	} else {
		p.log.Debug("proxy released from network",
			"proxy", px.ProxyAddress(),
			"network", network,
		)
	}
}

// ReleaseByAddressFromNetwork releases a proxy matching the given address
// from a specific network. Useful when the caller only has the address string.
func (p *Pool) ReleaseByAddressFromNetwork(addr, network string, failed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, px := range p.proxies {
		if px.ProxyAddress() == addr {
			delete(px.Networks, network)
			if failed {
				px.Healthy = false
				px.LastFail = time.Now()
				p.log.Warn("proxy released by address from network and marked unhealthy",
					"proxy", addr, "network", network)
			} else {
				p.log.Debug("proxy released by address from network",
					"proxy", addr, "network", network)
			}
			return
		}
	}
}

// MarkHealthy marks a specific proxy as healthy again.
func (p *Pool) MarkHealthy(proxy *Proxy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	proxy.Healthy = true
}

// Count returns the total number of loaded proxies.
func (p *Pool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.proxies)
}

// HealthyCount returns the number of currently healthy proxies.
func (p *Pool) HealthyCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	count := 0
	for _, px := range p.proxies {
		if px.Healthy {
			count++
		}
	}
	return count
}

// AvailableForNetwork returns the number of healthy proxies not currently
// connected to the given network.
func (p *Pool) AvailableForNetwork(network string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.availableForNetworkLocked(network)
}

// availableForNetworkLocked returns available count for a network (caller must hold lock).
func (p *Pool) availableForNetworkLocked(network string) int {
	count := 0
	for _, px := range p.proxies {
		if px.Healthy && !px.Networks[network] {
			count++
		}
	}
	return count
}

// All returns a snapshot of all proxies (for status reporting).
func (p *Pool) All() []*Proxy {
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make([]*Proxy, len(p.proxies))
	copy(result, p.proxies)
	return result
}

// ProxyAddress returns the SOCKS5 address string for dialing.
// Format: "host:port"
func (px *Proxy) ProxyAddress() string {
	return px.Host + ":" + px.Port
}

// HasAuth returns whether this proxy requires authentication.
func (px *Proxy) HasAuth() bool {
	return px.Username != ""
}

// NetworkCount returns the number of networks this proxy is currently connected to.
func (px *Proxy) NetworkCount() int {
	return len(px.Networks)
}

// String returns a human-readable representation (without credentials).
func (px *Proxy) String() string {
	status := "healthy"
	if !px.Healthy {
		status = "unhealthy"
	}
	return fmt.Sprintf("socks5://%s:%s (%s, used %d times, %d networks)",
		px.Host, px.Port, status, px.UseCount, len(px.Networks))
}

// StartHealthChecker runs a background goroutine that periodically tests
// unhealthy proxies and marks them healthy again if they recover.
func (p *Pool) StartHealthChecker(ctx context.Context) {
	ticker := time.NewTicker(DefaultHealthCheckInterval)
	defer ticker.Stop()

	p.log.Info("proxy health checker started", "interval", DefaultHealthCheckInterval)

	for {
		select {
		case <-ctx.Done():
			p.log.Info("proxy health checker stopped")
			return
		case <-ticker.C:
			p.checkUnhealthyProxies(ctx)
		}
	}
}

// checkUnhealthyProxies tests all unhealthy proxies and marks recovered ones healthy.
func (p *Pool) checkUnhealthyProxies(ctx context.Context) {
	p.mu.RLock()
	var unhealthy []*Proxy
	for _, px := range p.proxies {
		if !px.Healthy && time.Since(px.LastFail) >= DefaultRecoveryDelay {
			unhealthy = append(unhealthy, px)
		}
	}
	p.mu.RUnlock()

	if len(unhealthy) == 0 {
		return
	}

	p.log.Debug("checking unhealthy proxies", "count", len(unhealthy))

	for _, px := range unhealthy {
		if ctx.Err() != nil {
			return
		}
		if p.testProxy(px) {
			p.MarkHealthy(px)
			p.log.Info("proxy recovered", "proxy", px.Host+":"+px.Port)
		}
	}
}

// testProxy checks if a proxy is reachable by establishing a SOCKS5 connection.
func (p *Pool) testProxy(px *Proxy) bool {
	var auth *proxy.Auth
	if px.Username != "" {
		auth = &proxy.Auth{
			User:     px.Username,
			Password: px.Password,
		}
	}

	dialer, err := proxy.SOCKS5("tcp", px.ProxyAddress(), auth, &net.Dialer{
		Timeout: DefaultHealthCheckTimeout,
	})
	if err != nil {
		p.log.Debug("proxy health check failed (dialer)", "proxy", px.ProxyAddress(), "error", err)
		return false
	}

	// Try to establish a connection through the proxy to a well-known address
	conn, err := dialer.Dial("tcp", "irc.efnet.org:6667")
	if err != nil {
		p.log.Debug("proxy health check failed (dial)", "proxy", px.ProxyAddress(), "error", err)
		return false
	}
	conn.Close()
	return true
}

// parseProxy parses a proxy URL string into a Proxy struct.
func parseProxy(raw string) (*Proxy, error) {
	// Ensure it has a scheme
	if !strings.Contains(raw, "://") {
		raw = "socks5://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing proxy URL %q: %w", raw, err)
	}

	if u.Scheme != "socks5" && u.Scheme != "socks" {
		return nil, fmt.Errorf("unsupported proxy scheme %q (only socks5 supported)", u.Scheme)
	}

	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "1080" // default SOCKS5 port
	}

	proxy := &Proxy{
		URL:      raw,
		Host:     host,
		Port:     port,
		Healthy:  true,
		Networks: make(map[string]bool),
	}

	if u.User != nil {
		proxy.Username = u.User.Username()
		proxy.Password, _ = u.User.Password()
	}

	return proxy, nil
}
