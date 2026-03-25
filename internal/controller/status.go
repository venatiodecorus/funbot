package controller

import (
	"context"
	"fmt"
	"strings"

	fnredis "github.com/venatiodecorus/funbot/internal/redis"
)

// formatFullStatus generates a full status report across all networks.
func formatFullStatus(ctx context.Context, redisClient *fnredis.Client, homeNetwork string) string {
	summaries, err := redisClient.GetAllSummaries(ctx)
	if err != nil {
		return fmt.Sprintf("Error fetching status: %v", err)
	}

	if len(summaries) == 0 {
		return "--- Funbot Status ---\nNo workers reporting"
	}

	var lines []string
	lines = append(lines, "--- Funbot Status ---")

	for _, s := range summaries {
		marker := " "
		if s.Network == homeNetwork {
			marker = "*"
		}
		channels := "none"
		if len(s.Channels) > 0 {
			channels = strings.Join(s.Channels, ", ")
		}
		lines = append(lines, fmt.Sprintf("[%s%s] %d pod(s), %d/%d clients connected, channels: %s",
			marker, s.Network, s.TotalPods, s.Connected, s.TotalClients, channels))
	}

	return strings.Join(lines, "\n")
}

// formatNetworkStatus generates a detailed status for a specific network.
func formatNetworkStatus(ctx context.Context, redisClient *fnredis.Client, network string) string {
	states, err := redisClient.GetNetworkStates(ctx, network)
	if err != nil {
		return fmt.Sprintf("Error fetching status for %s: %v", network, err)
	}

	if len(states) == 0 {
		return fmt.Sprintf("[%s] No workers reporting", network)
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("--- %s Status ---", network))

	for _, state := range states {
		lines = append(lines, fmt.Sprintf("  Pod: %s", state.Pod))
		for _, client := range state.Clients {
			status := "disconnected"
			if client.Connected {
				status = "connected"
			}
			channels := "none"
			if len(client.Channels) > 0 {
				channels = strings.Join(client.Channels, ", ")
			}
			extra := ""
			if client.Proxy != "" {
				extra += fmt.Sprintf(" proxy=%s", client.Proxy)
			}
			if client.KeepNick != "" {
				extra += fmt.Sprintf(" keepnick=%s", client.KeepNick)
			}
			lines = append(lines, fmt.Sprintf("    [%s] nick=%s %s channels=[%s]%s",
				client.ID, client.Nick, status, channels, extra))
		}
	}

	return strings.Join(lines, "\n")
}
