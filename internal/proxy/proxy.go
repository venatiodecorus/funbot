// Package proxy manages a pool of SOCKS5 proxies for IRC client connections.
package proxy

import (
	"bufio"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Proxy represents a single SOCKS5 proxy.
type Proxy struct {
	URL      string
	Host     string
	Port     string
	Username string
	Password string
	Healthy  bool
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

	return p.LoadFromFile(file)
}

// Acquire returns the next available healthy proxy and marks it as in use.
// Returns nil if no healthy proxies are available.
func (p *Pool) Acquire() *Proxy {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Find the least-recently-used healthy proxy
	var best *Proxy
	for _, proxy := range p.proxies {
		if !proxy.Healthy {
			continue
		}
		if best == nil || proxy.LastUsed.Before(best.LastUsed) {
			best = proxy
		}
	}

	if best != nil {
		best.LastUsed = time.Now()
		best.UseCount++
	}

	return best
}

// Release marks a proxy as no longer in active use.
// If failed is true, the proxy is marked unhealthy.
func (p *Pool) Release(proxy *Proxy, failed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if failed {
		proxy.Healthy = false
		proxy.LastFail = time.Now()
		p.log.Warn("proxy marked unhealthy", "proxy", proxy.Host+":"+proxy.Port)
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
	for _, proxy := range p.proxies {
		if proxy.Healthy {
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
