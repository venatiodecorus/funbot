package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Key patterns for state storage.
const (
	// StateKeyPrefix is the prefix for worker state keys.
	// Full key: funbot:state:<network>:<pod>
	StateKeyPrefix = "funbot:state:"

	// StateTTL is how long a state key lives before expiring.
	// Workers should refresh before this expires.
	StateTTL = 30 * time.Second
)

// StateKey returns the Redis key for a worker's state.
func StateKey(network, pod string) string {
	return StateKeyPrefix + network + ":" + pod
}

// ClientState represents the state of a single IRC client connection.
type ClientState struct {
	ID        string   `json:"id"`
	Nick      string   `json:"nick"`
	Connected bool     `json:"connected"`
	Channels  []string `json:"channels"`
	Proxy     string   `json:"proxy,omitempty"`
	KeepNick  string   `json:"keepnick,omitempty"`
}

// PodState represents the full state of a worker pod.
type PodState struct {
	Pod       string        `json:"pod"`
	Network   string        `json:"network"`
	Clients   []ClientState `json:"clients"`
	Timestamp time.Time     `json:"timestamp"`
}

// NetworkSummary provides an aggregated view of a network's status.
type NetworkSummary struct {
	Network      string
	TotalPods    int
	TotalClients int
	Connected    int
	Channels     []string
}

// SetState writes a worker pod's state to Redis with a TTL.
func (c *Client) SetState(ctx context.Context, state PodState) error {
	state.Timestamp = time.Now()

	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	key := StateKey(state.Network, state.Pod)
	if err := c.rdb.Set(ctx, key, data, StateTTL).Err(); err != nil {
		return fmt.Errorf("setting state key %s: %w", key, err)
	}

	return nil
}

// GetState retrieves a specific pod's state from Redis.
func (c *Client) GetState(ctx context.Context, network, pod string) (*PodState, error) {
	key := StateKey(network, pod)
	data, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("getting state key %s: %w", key, err)
	}

	var state PodState
	if err := json.Unmarshal([]byte(data), &state); err != nil {
		return nil, fmt.Errorf("unmarshaling state: %w", err)
	}

	return &state, nil
}

// GetNetworkStates retrieves all pod states for a given network.
func (c *Client) GetNetworkStates(ctx context.Context, network string) ([]PodState, error) {
	pattern := StateKeyPrefix + network + ":*"
	keys, err := c.rdb.Keys(ctx, pattern).Result()
	if err != nil {
		return nil, fmt.Errorf("scanning state keys for %s: %w", network, err)
	}

	states := make([]PodState, 0, len(keys))
	for _, key := range keys {
		data, err := c.rdb.Get(ctx, key).Result()
		if err != nil {
			c.log.Warn("failed to get state key", "key", key, "error", err)
			continue
		}

		var state PodState
		if err := json.Unmarshal([]byte(data), &state); err != nil {
			c.log.Warn("failed to unmarshal state", "key", key, "error", err)
			continue
		}
		states = append(states, state)
	}

	return states, nil
}

// GetAllNetworkStates retrieves states for all networks.
func (c *Client) GetAllNetworkStates(ctx context.Context) (map[string][]PodState, error) {
	pattern := StateKeyPrefix + "*"
	keys, err := c.rdb.Keys(ctx, pattern).Result()
	if err != nil {
		return nil, fmt.Errorf("scanning all state keys: %w", err)
	}

	result := make(map[string][]PodState)
	for _, key := range keys {
		data, err := c.rdb.Get(ctx, key).Result()
		if err != nil {
			c.log.Warn("failed to get state key", "key", key, "error", err)
			continue
		}

		var state PodState
		if err := json.Unmarshal([]byte(data), &state); err != nil {
			c.log.Warn("failed to unmarshal state", "key", key, "error", err)
			continue
		}
		result[state.Network] = append(result[state.Network], state)
	}

	return result, nil
}

// GetNetworkSummary returns an aggregated summary for a network.
func (c *Client) GetNetworkSummary(ctx context.Context, network string) (*NetworkSummary, error) {
	states, err := c.GetNetworkStates(ctx, network)
	if err != nil {
		return nil, err
	}

	summary := &NetworkSummary{
		Network:   network,
		TotalPods: len(states),
	}

	channelSet := make(map[string]struct{})
	for _, state := range states {
		summary.TotalClients += len(state.Clients)
		for _, client := range state.Clients {
			if client.Connected {
				summary.Connected++
			}
			for _, ch := range client.Channels {
				channelSet[ch] = struct{}{}
			}
		}
	}

	for ch := range channelSet {
		summary.Channels = append(summary.Channels, ch)
	}

	return summary, nil
}

// GetAllSummaries returns summaries for all known networks.
func (c *Client) GetAllSummaries(ctx context.Context) ([]NetworkSummary, error) {
	allStates, err := c.GetAllNetworkStates(ctx)
	if err != nil {
		return nil, err
	}

	var summaries []NetworkSummary
	for network, states := range allStates {
		summary := NetworkSummary{
			Network:   network,
			TotalPods: len(states),
		}

		channelSet := make(map[string]struct{})
		for _, state := range states {
			summary.TotalClients += len(state.Clients)
			for _, client := range state.Clients {
				if client.Connected {
					summary.Connected++
				}
				for _, ch := range client.Channels {
					channelSet[ch] = struct{}{}
				}
			}
		}
		for ch := range channelSet {
			summary.Channels = append(summary.Channels, ch)
		}

		summaries = append(summaries, summary)
	}

	return summaries, nil
}

// CountAvailableClients returns how many connected clients exist on a network.
func (c *Client) CountAvailableClients(ctx context.Context, network string) (int, error) {
	states, err := c.GetNetworkStates(ctx, network)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, state := range states {
		for _, client := range state.Clients {
			if client.Connected {
				count++
			}
		}
	}

	return count, nil
}

// DeleteState removes a pod's state from Redis.
func (c *Client) DeleteState(ctx context.Context, network, pod string) error {
	key := StateKey(network, pod)
	if err := c.rdb.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("deleting state key %s: %w", key, err)
	}
	return nil
}

// parseNetworkFromKey extracts the network name from a state key.
func parseNetworkFromKey(key string) string {
	// key format: funbot:state:<network>:<pod>
	trimmed := strings.TrimPrefix(key, StateKeyPrefix)
	parts := strings.SplitN(trimmed, ":", 2)
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}
