package redis

import (
	"context"
	"encoding/json"
	"fmt"

	goredis "github.com/redis/go-redis/v9"
)

// Key patterns for Redis channels and keys.
const (
	// CmdChannelPrefix is the prefix for command channels per network.
	// Full channel: funbot:cmd:<network>
	CmdChannelPrefix = "funbot:cmd:"

	// StatusChannel is the channel workers publish status updates to.
	StatusChannel = "funbot:status"

	// AckChannel is the channel workers publish command acks/results to.
	AckChannel = "funbot:ack"
)

// CmdChannel returns the Redis pub/sub channel name for commands
// targeting a specific network.
func CmdChannel(network string) string {
	return CmdChannelPrefix + network
}

// Command represents a command message sent from controller to workers
// via Redis pub/sub.
type Command struct {
	ID      string   `json:"id"`      // Unique command ID for tracking
	Type    string   `json:"type"`    // Command type: "join", "part", "nick", "pm", "say", "raw", etc.
	Network string   `json:"network"` // Target network
	Args    []string `json:"args"`    // Command arguments
	Count   int      `json:"count"`   // Number of clients to use (0 = 1)
	Channel string   `json:"channel"` // Target channel (if applicable)
	Target  string   `json:"target"`  // Target user (if applicable)
	Message string   `json:"message"` // Message content (if applicable)
}

// CommandAck represents a response from a worker after executing a command.
type CommandAck struct {
	CommandID string `json:"command_id"` // References the original Command.ID
	Pod       string `json:"pod"`        // Pod that executed the command
	Network   string `json:"network"`    // Network the command was executed on
	Success   bool   `json:"success"`    // Whether the command succeeded
	Message   string `json:"message"`    // Human-readable result or error
}

// PublishCommand sends a command to all workers listening on the
// network's command channel.
func (c *Client) PublishCommand(ctx context.Context, cmd Command) error {
	data, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("marshaling command: %w", err)
	}

	channel := CmdChannel(cmd.Network)
	if err := c.rdb.Publish(ctx, channel, data).Err(); err != nil {
		return fmt.Errorf("publishing command to %s: %w", channel, err)
	}

	c.log.Debug("published command", "channel", channel, "type", cmd.Type, "id", cmd.ID)
	return nil
}

// PublishAck sends a command acknowledgment back to the controller.
func (c *Client) PublishAck(ctx context.Context, ack CommandAck) error {
	data, err := json.Marshal(ack)
	if err != nil {
		return fmt.Errorf("marshaling ack: %w", err)
	}

	if err := c.rdb.Publish(ctx, AckChannel, data).Err(); err != nil {
		return fmt.Errorf("publishing ack: %w", err)
	}

	c.log.Debug("published ack", "command_id", ack.CommandID, "success", ack.Success)
	return nil
}

// SubscribeCommands subscribes to the command channel for a specific network.
// Returns a channel that receives Command messages. The caller should
// cancel the context to stop the subscription.
func (c *Client) SubscribeCommands(ctx context.Context, network string) (<-chan Command, error) {
	channel := CmdChannel(network)
	sub := c.rdb.Subscribe(ctx, channel)

	// Wait for confirmation
	if _, err := sub.Receive(ctx); err != nil {
		return nil, fmt.Errorf("subscribing to %s: %w", channel, err)
	}

	cmdCh := make(chan Command, 32)

	go func() {
		defer close(cmdCh)
		defer sub.Close()

		ch := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				var cmd Command
				if err := json.Unmarshal([]byte(msg.Payload), &cmd); err != nil {
					c.log.Error("failed to unmarshal command", "error", err, "payload", msg.Payload)
					continue
				}
				cmdCh <- cmd
			}
		}
	}()

	c.log.Info("subscribed to commands", "channel", channel)
	return cmdCh, nil
}

// SubscribeAcks subscribes to the acknowledgment channel.
// Returns a channel that receives CommandAck messages.
func (c *Client) SubscribeAcks(ctx context.Context) (<-chan CommandAck, error) {
	sub := c.rdb.Subscribe(ctx, AckChannel)

	if _, err := sub.Receive(ctx); err != nil {
		return nil, fmt.Errorf("subscribing to %s: %w", AckChannel, err)
	}

	ackCh := make(chan CommandAck, 32)

	go func() {
		defer close(ackCh)
		defer sub.Close()

		ch := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				var ack CommandAck
				if err := json.Unmarshal([]byte(msg.Payload), &ack); err != nil {
					c.log.Error("failed to unmarshal ack", "error", err)
					continue
				}
				ackCh <- ack
			}
		}
	}()

	c.log.Info("subscribed to acks")
	return ackCh, nil
}

// SubscribeStatus subscribes to the status channel.
// Returns the raw go-redis PubSub for flexible consumption.
func (c *Client) SubscribeStatus(ctx context.Context) (*goredis.PubSub, error) {
	sub := c.rdb.Subscribe(ctx, StatusChannel)

	if _, err := sub.Receive(ctx); err != nil {
		return nil, fmt.Errorf("subscribing to %s: %w", StatusChannel, err)
	}

	c.log.Info("subscribed to status updates")
	return sub, nil
}
