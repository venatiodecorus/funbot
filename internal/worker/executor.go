package worker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	fnredis "github.com/venatiodecorus/funbot/internal/redis"
)

// Executor handles incoming commands from Redis and applies them
// to the local client manager.
type Executor struct {
	cm  *ClientManager
	ctx context.Context // parent context for long-running operations like keepnick
	log *slog.Logger
}

// NewExecutor creates a new command executor for the given client manager.
func NewExecutor(ctx context.Context, cm *ClientManager, log *slog.Logger) *Executor {
	return &Executor{
		cm:  cm,
		ctx: ctx,
		log: log.With("component", "executor", "network", cm.Network()),
	}
}

// Execute processes a command and returns an ack with the result.
func (e *Executor) Execute(cmd fnredis.Command) fnredis.CommandAck {
	e.log.Info("executing command", "type", cmd.Type, "id", cmd.ID)

	ack := fnredis.CommandAck{
		CommandID: cmd.ID,
		Pod:       e.cm.PodName(),
		Network:   e.cm.Network(),
	}

	switch cmd.Type {
	case "join":
		ack.Message = e.execJoin(cmd)
		ack.Success = true
	case "part":
		ack.Message = e.execPart(cmd)
		ack.Success = true
	case "nick":
		ack.Message = e.execNick(cmd)
		ack.Success = true
	case "keepnick":
		ack.Message = e.execKeepNick(cmd)
		ack.Success = true
	case "pm":
		ack.Message = e.execPM(cmd)
		ack.Success = true
	case "say":
		ack.Message = e.execSay(cmd)
		ack.Success = true
	case "raw":
		ack.Message = e.execRaw(cmd)
		ack.Success = true
	default:
		ack.Success = false
		ack.Message = fmt.Sprintf("unknown command type: %s", cmd.Type)
	}

	return ack
}

// execJoin joins clients to a channel.
func (e *Executor) execJoin(cmd fnredis.Command) string {
	channel := cmd.Channel
	if channel == "" && len(cmd.Args) > 0 {
		channel = cmd.Args[0]
	}
	if channel == "" {
		return "no channel specified"
	}

	clients := e.cm.SelectClients(cmd.Count)
	if len(clients) == 0 {
		return "no connected clients available"
	}

	for _, c := range clients {
		c.Join(channel)
	}

	return fmt.Sprintf("joined %d client(s) to %s", len(clients), channel)
}

// execPart parts clients from a channel.
func (e *Executor) execPart(cmd fnredis.Command) string {
	channel := cmd.Channel
	if channel == "" && len(cmd.Args) > 0 {
		channel = cmd.Args[0]
	}
	if channel == "" {
		return "no channel specified"
	}

	count := cmd.Count
	if len(cmd.Args) > 1 && cmd.Args[1] == "all" {
		count = 0 // 0 means all
	}

	clients := e.cm.SelectClients(count)
	if len(clients) == 0 {
		return "no connected clients available"
	}

	for _, c := range clients {
		c.Part(channel)
	}

	return fmt.Sprintf("parted %d client(s) from %s", len(clients), channel)
}

// execNick changes the nick of a client or all clients.
func (e *Executor) execNick(cmd fnredis.Command) string {
	if len(cmd.Args) < 2 {
		return "usage: nick <client_id|all> <newnick>"
	}

	target := cmd.Args[0]
	newNick := cmd.Args[1]

	if target == "all" {
		clients := e.cm.ConnectedClients()
		for i, c := range clients {
			c.SetNick(fmt.Sprintf("%s%d", newNick, i))
		}
		return fmt.Sprintf("changing nick for %d clients to %s*", len(clients), newNick)
	}

	client := e.cm.ClientByID(target)
	if client == nil {
		return fmt.Sprintf("client %s not found", target)
	}

	client.SetNick(newNick)
	return fmt.Sprintf("changing nick for %s to %s", target, newNick)
}

// execKeepNick starts or stops keepnick for a client.
func (e *Executor) execKeepNick(cmd fnredis.Command) string {
	if len(cmd.Args) < 2 {
		return "usage: keepnick <client_id> <desirednick|stop>"
	}

	clientID := cmd.Args[0]
	desiredNick := cmd.Args[1]

	if desiredNick == "stop" {
		return e.cm.StopKeepNick(clientID)
	}

	return e.cm.StartKeepNick(e.ctx, clientID, desiredNick)
}

// execPM sends a private message from multiple clients.
func (e *Executor) execPM(cmd fnredis.Command) string {
	target := cmd.Target
	message := cmd.Message

	if target == "" || message == "" {
		return "usage: pm requires target and message"
	}

	clients := e.cm.SelectClients(cmd.Count)
	if len(clients) == 0 {
		return "no connected clients available"
	}

	for _, c := range clients {
		c.Privmsg(target, message)
	}

	return fmt.Sprintf("sent PM to %s from %d client(s)", target, len(clients))
}

// execSay sends a message to a channel from multiple clients.
func (e *Executor) execSay(cmd fnredis.Command) string {
	channel := cmd.Channel
	message := cmd.Message

	if channel == "" || message == "" {
		return "usage: say requires channel and message"
	}

	clients := e.cm.SelectClients(cmd.Count)
	if len(clients) == 0 {
		return "no connected clients available"
	}

	for _, c := range clients {
		c.Privmsg(channel, message)
	}

	return fmt.Sprintf("sent message to %s from %d client(s)", channel, len(clients))
}

// execRaw sends a raw IRC command.
func (e *Executor) execRaw(cmd fnredis.Command) string {
	if len(cmd.Args) < 2 {
		return "usage: raw <client_id|all> <raw command>"
	}

	target := cmd.Args[0]
	rawCmd := strings.Join(cmd.Args[1:], " ")

	if target == "all" {
		clients := e.cm.ConnectedClients()
		for _, c := range clients {
			c.SendRaw(rawCmd)
		}
		return fmt.Sprintf("sent raw command to %d clients", len(clients))
	}

	client := e.cm.ClientByID(target)
	if client == nil {
		return fmt.Sprintf("client %s not found", target)
	}

	client.SendRaw(rawCmd)
	return fmt.Sprintf("sent raw command via %s", target)
}
