// Package proxy manages a pool of SOCKS5 proxies sourced from a
// proxy-scanner API for IRC client connections.
package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	netproxy "golang.org/x/net/proxy"
)

const (
	// DefaultRefreshInterval is how often to re-fetch proxies from the API.
	DefaultRefreshInterval = 5 * time.Minute

	// DefaultAPITimeout is the HTTP timeout for API requests.
	DefaultAPITimeout = 10 * time.Second

	// DefaultHealthCheckTimeout is the timeout for proxy connectivity checks.
	DefaultHealthCheckTimeout = 10 * time.Second

	// DefaultMaxRetries is the default number of consecutive connection failures
	// before a proxy is purged from the pool.
	DefaultMaxRetries = 3
)

// apiProxy is the JSON structure returned by the proxy-scanner API.
type apiProxy struct {
	ID        int     `json:"id"`
	IP        string  `json:"ip"`
	Port      int     `json:"port"`
	Protocol  string  `json:"protocol"`
	Anonymity string  `json:"anonymity"`
	Country   string  `json:"country"`
	LatencyMs float64 `json:"latency_ms"`
	Alive     bool    `json:"alive"`
}

// apiResponse is the JSON wrapper returned by the proxy-scanner /v1/proxies endpoint.
type apiResponse struct {
	Proxies []apiProxy `json:"proxies"`
	Total   int        `json:"total"`
}

// Proxy represents a single SOCKS5 proxy.
type Proxy struct {
	Host      string
	Port      string
	Healthy   bool
	Networks  map[string]bool // networks this proxy is currently connected to
	Country   string
	LastUsed  time.Time
	LastFail  time.Time
	UseCount  int
	FailCount int // consecutive connection failures
}

// Pool manages a collection of proxies fetched from the proxy-scanner API.
type Pool struct {
	proxies     []*Proxy
	mu          sync.RWMutex
	log         *slog.Logger
	apiURL      string
	protocol    string
	maxLat      int
	maxRetries  int // consecutive failures before purging a proxy
	minPoolSize int // minimum healthy proxies to maintain
	client      *http.Client
}

// NewPool creates a new proxy pool.
func NewPool(log *slog.Logger) *Pool {
	return &Pool{
		log:        log.With("component", "proxy"),
		maxRetries: DefaultMaxRetries,
		client: &http.Client{
			Timeout: DefaultAPITimeout,
		},
	}
}

// SetAPI configures the proxy-scanner API endpoint and filter parameters.
func (p *Pool) SetAPI(apiURL, protocol string, maxLatency int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.apiURL = apiURL
	p.protocol = protocol
	p.maxLat = maxLatency
}

// SetMaxRetries configures the number of consecutive failures before a proxy
// is purged from the pool. A value of 0 means purge on first failure.
func (p *Pool) SetMaxRetries(maxRetries int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.maxRetries = maxRetries
}

// SetMinPoolSize configures the minimum number of healthy proxies to maintain.
// The background refresher will auto-fetch from the API when the healthy count
// drops below this threshold. A value of 0 disables this behavior.
func (p *Pool) SetMinPoolSize(minPoolSize int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.minPoolSize = minPoolSize
}

// MaxRetries returns the configured maximum consecutive failures before a
// proxy is purged. This is useful for callers that need to limit per-proxy
// connection attempts to match the pool's purge threshold.
func (p *Pool) MaxRetries() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.maxRetries
}

// FetchFromAPI fetches proxies from the proxy-scanner API and replaces the
// pool contents. Proxies that are currently in use (assigned to a network)
// are preserved and merged with the fresh list.
func (p *Pool) FetchFromAPI(ctx context.Context) error {
	if p.apiURL == "" {
		return fmt.Errorf("proxy API URL not configured")
	}

	u, err := url.Parse(p.apiURL + "/v1/proxies")
	if err != nil {
		return fmt.Errorf("parsing API URL: %w", err)
	}

	q := u.Query()
	if p.protocol != "" {
		q.Set("protocol", p.protocol)
	}
	if p.maxLat > 0 {
		q.Set("max_latency", fmt.Sprintf("%d", p.maxLat))
	}
	q.Set("limit", "1000")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("creating API request: %w", err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetching proxies from API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("API returned status %d: %s", resp.StatusCode, string(body))
	}

	var apiResp apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return fmt.Errorf("decoding API response: %w", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Build a set of currently in-use proxies (assigned to networks) so we
	// can preserve their state across refreshes.
	inUse := make(map[string]*Proxy)
	for _, px := range p.proxies {
		if len(px.Networks) > 0 {
			inUse[px.ProxyAddress()] = px
		}
	}

	var newProxies []*Proxy
	for _, ap := range apiResp.Proxies {
		if !ap.Alive {
			continue
		}
		addr := fmt.Sprintf("%s:%d", ap.IP, ap.Port)
		if existing, ok := inUse[addr]; ok {
			// Preserve the in-use proxy with its network assignments
			existing.Healthy = true
			existing.Country = ap.Country
			newProxies = append(newProxies, existing)
			delete(inUse, addr)
		} else {
			newProxies = append(newProxies, &Proxy{
				Host:     ap.IP,
				Port:     fmt.Sprintf("%d", ap.Port),
				Healthy:  true,
				Networks: make(map[string]bool),
				Country:  ap.Country,
			})
		}
	}

	// Keep any in-use proxies that disappeared from the API (still connected).
	for _, px := range inUse {
		px.Healthy = false // mark unhealthy since API no longer reports it
		newProxies = append(newProxies, px)
	}

	p.proxies = newProxies
	p.log.Info("refreshed proxies from API",
		"fetched", len(apiResp.Proxies),
		"alive", len(newProxies),
		"preserved_in_use", len(inUse),
	)

	return nil
}

// StartRefresher runs a background goroutine that periodically re-fetches
// proxies from the API and refills the pool if healthy count is below the
// configured minimum.
func (p *Pool) StartRefresher(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultRefreshInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	p.log.Info("proxy refresher started", "interval", interval)

	for {
		select {
		case <-ctx.Done():
			p.log.Info("proxy refresher stopped")
			return
		case <-ticker.C:
			if err := p.FetchFromAPI(ctx); err != nil {
				p.log.Error("failed to refresh proxies from API", "error", err)
			}
			// After regular refresh, check if we still need more
			if err := p.RefillIfNeeded(ctx); err != nil {
				p.log.Error("failed to refill proxy pool", "error", err)
			}
		}
	}
}

// AcquireForNetwork returns the next available healthy proxy that is not
// already connected to the given network. A single proxy can serve
// connections to multiple different networks simultaneously.
// Returns nil if no suitable proxy is available.
func (p *Pool) AcquireForNetwork(network string) *Proxy {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Find least-recently-used healthy proxy NOT on this network
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
// If failed is true, the proxy's failure count is incremented. Once the
// failure count exceeds maxRetries the proxy is purged from the pool.
func (p *Pool) ReleaseFromNetwork(px *Proxy, network string, failed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(px.Networks, network)

	if failed {
		px.FailCount++
		px.LastFail = time.Now()

		if px.FailCount > p.maxRetries {
			// Purge from pool
			p.removeProxyLocked(px)
			p.log.Warn("proxy purged from pool after exceeding max retries",
				"proxy", px.ProxyAddress(),
				"network", network,
				"fail_count", px.FailCount,
				"max_retries", p.maxRetries,
			)
		} else {
			px.Healthy = false
			p.log.Warn("proxy released from network and marked unhealthy",
				"proxy", px.ProxyAddress(),
				"network", network,
				"fail_count", px.FailCount,
				"max_retries", p.maxRetries,
			)
		}
	} else {
		// Successful release resets the failure counter
		px.FailCount = 0
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
				px.FailCount++
				px.LastFail = time.Now()

				if px.FailCount > p.maxRetries {
					p.removeProxyLocked(px)
					p.log.Warn("proxy purged from pool after exceeding max retries",
						"proxy", addr, "network", network,
						"fail_count", px.FailCount, "max_retries", p.maxRetries)
				} else {
					px.Healthy = false
					p.log.Warn("proxy released by address from network and marked unhealthy",
						"proxy", addr, "network", network,
						"fail_count", px.FailCount, "max_retries", p.maxRetries)
				}
			} else {
				px.FailCount = 0
				p.log.Debug("proxy released by address from network",
					"proxy", addr, "network", network)
			}
			return
		}
	}
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

// removeProxyLocked removes a proxy from the pool. Caller must hold p.mu.
func (p *Pool) removeProxyLocked(px *Proxy) {
	for i, candidate := range p.proxies {
		if candidate == px {
			p.proxies = append(p.proxies[:i], p.proxies[i+1:]...)
			return
		}
	}
}

// EnsureAvailable checks whether at least `count` healthy proxies are
// available for the given network. If not, it fetches more from the API
// (up to maxAttempts rounds) until enough are available or the API has
// nothing new to offer. It returns the number currently available.
func (p *Pool) EnsureAvailable(ctx context.Context, network string, count int) (int, error) {
	const maxAttempts = 3

	available := p.AvailableForNetwork(network)
	if available >= count {
		return available, nil
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		p.log.Info("pool needs more proxies, fetching from API",
			"network", network,
			"need", count,
			"available", available,
			"attempt", attempt+1,
		)
		if err := p.FetchFromAPI(ctx); err != nil {
			return available, fmt.Errorf("fetching proxies on demand: %w", err)
		}

		newAvailable := p.AvailableForNetwork(network)
		if newAvailable >= count {
			return newAvailable, nil
		}
		// If the API didn't give us any new proxies, stop trying
		if newAvailable <= available {
			break
		}
		available = newAvailable
	}

	return p.AvailableForNetwork(network), nil
}

// RefillIfNeeded checks if the healthy proxy count has dropped below the
// configured minimum pool size and fetches more from the API if so.
func (p *Pool) RefillIfNeeded(ctx context.Context) error {
	p.mu.RLock()
	minSize := p.minPoolSize
	p.mu.RUnlock()

	if minSize <= 0 {
		return nil
	}

	healthy := p.HealthyCount()
	if healthy >= minSize {
		return nil
	}

	p.log.Info("healthy proxy count below minimum, refilling",
		"healthy", healthy,
		"min_pool_size", minSize,
	)
	return p.FetchFromAPI(ctx)
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
func (px *Proxy) ProxyAddress() string {
	return px.Host + ":" + px.Port
}

// NetworkCount returns the number of networks this proxy is currently connected to.
func (px *Proxy) NetworkCount() int {
	return len(px.Networks)
}

// String returns a human-readable representation.
func (px *Proxy) String() string {
	status := "healthy"
	if !px.Healthy {
		status = "unhealthy"
	}
	extra := ""
	if px.Country != "" {
		extra = " " + px.Country
	}
	return fmt.Sprintf("socks5://%s:%s (%s, used %d times, %d networks%s)",
		px.Host, px.Port, status, px.UseCount, len(px.Networks), extra)
}

// TestConnectivity checks if a proxy is reachable by establishing a SOCKS5 connection.
func (p *Pool) TestConnectivity(px *Proxy) bool {
	dialer, err := netproxy.SOCKS5("tcp", px.ProxyAddress(), nil, &net.Dialer{
		Timeout: DefaultHealthCheckTimeout,
	})
	if err != nil {
		p.log.Debug("proxy connectivity check failed (dialer)", "proxy", px.ProxyAddress(), "error", err)
		return false
	}

	conn, err := dialer.Dial("tcp", "irc.efnet.org:6667")
	if err != nil {
		p.log.Debug("proxy connectivity check failed (dial)", "proxy", px.ProxyAddress(), "error", err)
		return false
	}
	conn.Close()
	return true
}
