package irc

import (
	"sync"
	"time"
)

// FloodControl manages rate limiting for IRC message sending
// to prevent being disconnected for excess flood.
type FloodControl struct {
	delay    time.Duration
	mu       sync.Mutex
	lastSend time.Time
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
}

// Delay returns the configured delay between messages.
func (fc *FloodControl) Delay() time.Duration {
	return fc.delay
}
