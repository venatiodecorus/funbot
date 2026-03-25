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
	InUse    bool // Whether this proxy is assigned to an active client
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

// Acquire returns the next available healthy proxy and marks it as in use.
// Returns nil if no healthy proxies are available. Prefers proxies that
// are not currently in use; falls back to least-recently-used healthy proxy.
func (p *Pool) Acquire() *Proxy {
	p.mu.Lock()
	defer p.mu.Unlock()

	// First pass: find the least-recently-used healthy proxy that is NOT in use
	var best *Proxy
	for _, px := range p.proxies {
		if !px.Healthy || px.InUse {
			continue
		}
		if best == nil || px.LastUsed.Before(best.LastUsed) {
			best = px
		}
	}

	// Second pass: if all healthy proxies are in use, allow reuse
	// (multiple clients may share a proxy if pool is small)
	if best == nil {
		for _, px := range p.proxies {
			if !px.Healthy {
				continue
			}
			if best == nil || px.UseCount < best.UseCount {
				best = px
			}
		}
	}

	if best != nil {
		best.InUse = true
		best.LastUsed = time.Now()
		best.UseCount++
		p.log.Debug("proxy acquired",
			"proxy", best.ProxyAddress(),
			"use_count", best.UseCount,
			"in_use", p.inUseCountLocked(),
			"available", p.availableCountLocked(),
		)
	} else {
		p.log.Warn("no healthy proxies available", "total", len(p.proxies))
	}

	return best
}

// Release marks a proxy as no longer in active use.
// If failed is true, the proxy is also marked unhealthy.
func (p *Pool) Release(px *Proxy, failed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	px.InUse = false

	if failed {
		px.Healthy = false
		px.LastFail = time.Now()
		p.log.Warn("proxy released and marked unhealthy",
			"proxy", px.ProxyAddress(),
			"in_use", p.inUseCountLocked(),
			"available", p.availableCountLocked(),
		)
	} else {
		p.log.Debug("proxy released",
			"proxy", px.ProxyAddress(),
			"in_use", p.inUseCountLocked(),
			"available", p.availableCountLocked(),
		)
	}
}

// ReleaseByAddress releases a proxy matching the given address.
// This is useful when the caller only has the address string (e.g., from irc.Client).
func (p *Pool) ReleaseByAddress(addr string, failed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, px := range p.proxies {
		if px.ProxyAddress() == addr {
			px.InUse = false
			if failed {
				px.Healthy = false
				px.LastFail = time.Now()
				p.log.Warn("proxy released by address and marked unhealthy", "proxy", addr)
			} else {
				p.log.Debug("proxy released by address", "proxy", addr)
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

// InUseCount returns the number of proxies currently assigned to clients.
func (p *Pool) InUseCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.inUseCountLocked()
}

// AvailableCount returns the number of healthy proxies not currently in use.
func (p *Pool) AvailableCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.availableCountLocked()
}

// inUseCountLocked returns in-use count (caller must hold lock).
func (p *Pool) inUseCountLocked() int {
	count := 0
	for _, px := range p.proxies {
		if px.InUse {
			count++
		}
	}
	return count
}

// availableCountLocked returns available count (caller must hold lock).
func (p *Pool) availableCountLocked() int {
	count := 0
	for _, px := range p.proxies {
		if px.Healthy && !px.InUse {
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

// String returns a human-readable representation (without credentials).
func (px *Proxy) String() string {
	status := "healthy"
	if !px.Healthy {
		status = "unhealthy"
	}
	return fmt.Sprintf("socks5://%s:%s (%s, used %d times)", px.Host, px.Port, status, px.UseCount)
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
		URL:     raw,
		Host:    host,
		Port:    port,
		Healthy: true,
	}

	if u.User != nil {
		proxy.Username = u.User.Username()
		proxy.Password, _ = u.User.Password()
	}

	return proxy, nil
}
