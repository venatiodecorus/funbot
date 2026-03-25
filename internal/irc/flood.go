package irc

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// FloodControl manages rate limiting for IRC message sending
// to prevent being disconnected for excess flood.
//
// It uses a simple delay-based approach where each message must wait
// at least `delay` since the previous message. This ensures we never
// exceed the rate limit for a single client.
type FloodControl struct {
	delay    time.Duration
	mu       sync.Mutex
	lastSend time.Time
	msgCount atomic.Int64
}

// NewFloodControl creates a new flood controller with the specified
// minimum delay between messages.
func NewFloodControl(delay time.Duration) *FloodControl {
	return &FloodControl{
		delay: delay,
	}
}

// Wait blocks until it is safe to send the next message without
// triggering flood protection.
func (fc *FloodControl) Wait() {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(fc.lastSend)
	if elapsed < fc.delay {
		time.Sleep(fc.delay - elapsed)
	}
	fc.lastSend = time.Now()
	fc.msgCount.Add(1)
}

// Delay returns the configured delay between messages.
func (fc *FloodControl) Delay() time.Duration {
	return fc.delay
}

// MessageCount returns the total number of messages rate-limited
// through this controller.
func (fc *FloodControl) MessageCount() int64 {
	return fc.msgCount.Load()
}

// GlobalFloodGuard provides a network-wide rate limit safety net that
// prevents the total message rate across all clients from exceeding
// a hard limit. This is the last line of defense against flooding.
//
// It uses a token bucket algorithm: tokens are added at a fixed rate,
// and each message consumes one token. If no tokens are available,
// the caller blocks until one becomes available.
type GlobalFloodGuard struct {
	tokens     int
	maxTokens  int
	refillRate time.Duration // how often a token is added
	mu         sync.Mutex
	lastRefill time.Time
	log        *slog.Logger
	blocked    atomic.Int64
}

// NewGlobalFloodGuard creates a new global flood guard.
//
//   - maxTokens: maximum burst size (messages that can be sent without delay)
//   - refillRate: how often one token is added (e.g., 200ms = 5 msg/sec max)
func NewGlobalFloodGuard(maxTokens int, refillRate time.Duration, log *slog.Logger) *GlobalFloodGuard {
	return &GlobalFloodGuard{
		tokens:     maxTokens,
		maxTokens:  maxTokens,
		refillRate: refillRate,
		lastRefill: time.Now(),
		log:        log.With("component", "flood-guard"),
	}
}

// Acquire blocks until a send token is available. This should be called
// before sending any message to ensure the global rate limit is respected.
func (g *GlobalFloodGuard) Acquire() {
	for {
		g.mu.Lock()
		g.refillTokens()

		if g.tokens > 0 {
			g.tokens--
			g.mu.Unlock()
			return
		}

		// No tokens available, need to wait
		g.blocked.Add(1)
		g.mu.Unlock()

		// Wait for one refill period
		time.Sleep(g.refillRate)
	}
}

// refillTokens adds tokens based on elapsed time (caller must hold lock).
func (g *GlobalFloodGuard) refillTokens() {
	now := time.Now()
	elapsed := now.Sub(g.lastRefill)
	newTokens := int(elapsed / g.refillRate)
	if newTokens > 0 {
		g.tokens += newTokens
		if g.tokens > g.maxTokens {
			g.tokens = g.maxTokens
		}
		g.lastRefill = now
	}
}

// BlockedCount returns how many times callers were blocked waiting for tokens.
// This is useful for monitoring whether the rate limit is being hit.
func (g *GlobalFloodGuard) BlockedCount() int64 {
	return g.blocked.Load()
}

// TokensAvailable returns the current number of available tokens.
func (g *GlobalFloodGuard) TokensAvailable() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refillTokens()
	return g.tokens
}
