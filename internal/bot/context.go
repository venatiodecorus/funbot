package bot

import "sync"

// CommandContext holds the active network/channel context that allows
// the user to omit these arguments from commands.
type CommandContext struct {
	mu      sync.RWMutex
	network string
	channel string
}

// NewCommandContext creates a new empty command context.
func NewCommandContext() *CommandContext {
	return &CommandContext{}
}

// Set sets the active network and optionally a channel.
func (cc *CommandContext) Set(network, channel string) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.network = network
	cc.channel = channel
}

// Clear resets the context to empty.
func (cc *CommandContext) Clear() {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.network = ""
	cc.channel = ""
}

// Network returns the active network, or empty string if not set.
func (cc *CommandContext) Network() string {
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	return cc.network
}

// Channel returns the active channel, or empty string if not set.
func (cc *CommandContext) Channel() string {
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	return cc.channel
}

// ResolveNetwork returns the explicitly provided network if non-empty,
// otherwise falls back to the context network.
func (cc *CommandContext) ResolveNetwork(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return cc.Network()
}

// ResolveChannel returns the explicitly provided channel if non-empty,
// otherwise falls back to the context channel.
func (cc *CommandContext) ResolveChannel(explicit string) string {
	if explicit != "" {
		return explicit
	}
	return cc.Channel()
}

// String returns a human-readable representation of the current context.
func (cc *CommandContext) String() string {
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	if cc.network == "" {
		return "no context set"
	}
	if cc.channel == "" {
		return "network: " + cc.network
	}
	return "network: " + cc.network + ", channel: " + cc.channel
}
