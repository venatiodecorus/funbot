// Package controller implements the Funbot controller role, which
// connects to the home IRC network, accepts commands, and coordinates workers.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/venatiodecorus/funbot/internal/auth"
	"github.com/venatiodecorus/funbot/internal/config"
	"github.com/venatiodecorus/funbot/internal/irc"
	fnredis "github.com/venatiodecorus/funbot/internal/redis"
)

// Controller manages the home network IRC connection and command handling.
type Controller struct {
	cfg        *config.Config
	ircClient  *irc.Client
	authCheck  *auth.Checker
	cmdCtx     *CommandContext
	dispatcher *CommandDispatcher
	redis      *fnredis.Client
	log        *slog.Logger
	cmdCounter atomic.Uint64
}

// New creates a new Controller from the given configuration.
func New(cfg *config.Config, redisClient *fnredis.Client, log *slog.Logger) (*Controller, error) {
	homeNet, ok := cfg.Networks[cfg.Controller.HomeNetwork]
	if !ok {
		return nil, fmt.Errorf("home network %q not found in config", cfg.Controller.HomeNetwork)
	}

	if len(homeNet.Servers) == 0 {
		return nil, fmt.Errorf("home network %q has no servers", cfg.Controller.HomeNetwork)
	}

	// Parse server address
	server, port, err := parseServerAddr(homeNet.Servers[0])
	if err != nil {
		return nil, fmt.Errorf("parsing home network server: %w", err)
	}

	ircCfg := irc.ClientConfig{
		ID:       cfg.Controller.HomeNetwork + "-ctrl",
		Network:  cfg.Controller.HomeNetwork,
		Server:   server,
		Port:     port,
		SSL:      homeNet.SSL,
		Nick:     homeNet.NickPrefix,
		User:     "funbot",
		Realname: "Funbot Controller",
		Logger:   log,
	}

	ircClient := irc.New(ircCfg)
	authChecker := auth.New(cfg.Controller.Auth.Nick, cfg.Controller.Auth.Hostname)
	cmdCtx := NewCommandContext()

	prefix := cfg.Controller.CommandPrefix
	if prefix == "" {
		prefix = "!"
	}

	dispatcher := NewCommandDispatcher(prefix, cmdCtx, log)

	ctrl := &Controller{
		cfg:        cfg,
		ircClient:  ircClient,
		authCheck:  authChecker,
		cmdCtx:     cmdCtx,
		dispatcher: dispatcher,
		redis:      redisClient,
		log:        log,
	}

	// Register commands
	ctrl.registerCommands()

	// Set up PRIVMSG handler for incoming commands
	ircClient.OnPrivmsg(ctrl.handlePrivmsg)

	// Auto-join configured channels on connect
	channels := homeNet.Channels
	ircClient.OnConnect(func() {
		for _, ch := range channels {
			ircClient.Join(ch)
		}
	})

	return ctrl, nil
}

// Run starts the controller. It blocks until the context is cancelled
// or the IRC connection is lost.
func (c *Controller) Run(ctx context.Context) error {
	c.log.Info("starting controller",
		"home_network", c.cfg.Controller.HomeNetwork,
		"auth_nick", c.cfg.Controller.Auth.Nick,
	)

	// Start listening for acks from workers
	if c.redis != nil {
		go c.listenAcks(ctx)
	}

	return c.ircClient.Connect(ctx)
}

// listenAcks subscribes to the ack channel and logs results.
// In the future this could be used to relay results back to the user.
func (c *Controller) listenAcks(ctx context.Context) {
	ackCh, err := c.redis.SubscribeAcks(ctx)
	if err != nil {
		c.log.Error("failed to subscribe to acks", "error", err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ack, ok := <-ackCh:
			if !ok {
				return
			}
			c.log.Info("received ack",
				"command_id", ack.CommandID,
				"pod", ack.Pod,
				"success", ack.Success,
				"message", ack.Message,
			)
		}
	}
}

// handlePrivmsg processes incoming private messages, checking auth
// and dispatching commands.
func (c *Controller) handlePrivmsg(nick, hostname, target, message string) {
	// Only process private messages (target is our nick, not a channel)
	if IsChannel(target) {
		return
	}

	if !c.authCheck.IsAuthorized(nick, hostname) {
		c.log.Debug("unauthorized command attempt", "nick", nick, "hostname", hostname)
		return
	}

	response := c.dispatcher.Dispatch(message)
	if response != "" {
		// Send each line as a separate message
		for _, line := range strings.Split(response, "\n") {
			if line != "" {
				c.ircClient.Privmsg(nick, line)
			}
		}
	}
}

// generateCommandID creates a unique command ID.
func (c *Controller) generateCommandID() string {
	return uuid.New().String()[:8]
}

// publishCommand sends a command to workers via Redis.
func (c *Controller) publishCommand(cmd fnredis.Command) string {
	if c.redis == nil {
		return "Redis not connected — cannot route to workers"
	}

	cmd.ID = c.generateCommandID()

	if err := c.redis.PublishCommand(context.Background(), cmd); err != nil {
		return fmt.Sprintf("Error sending command: %v", err)
	}

	return fmt.Sprintf("Command sent [%s] to %s workers", cmd.ID, cmd.Network)
}

// registerCommands registers all command handlers.
func (c *Controller) registerCommands() {
	c.dispatcher.Register("status", c.handleStatus)
	c.dispatcher.Register("networks", c.handleNetworks)
	c.dispatcher.Register("join", c.handleJoin)
	c.dispatcher.Register("part", c.handlePart)
	c.dispatcher.Register("nick", c.handleNick)
	c.dispatcher.Register("keepnick", c.handleKeepNick)
	c.dispatcher.Register("pm", c.handlePM)
	c.dispatcher.Register("say", c.handleSay)
	c.dispatcher.Register("raw", c.handleRaw)
}

// handleStatus implements !status [network].
func (c *Controller) handleStatus(args []string) string {
	ctx := context.Background()

	// Controller's own status line
	ctrlStatus := fmt.Sprintf("[*%s] Controller: nick=%s, connected=%v, channels=%v",
		c.cfg.Controller.HomeNetwork,
		c.ircClient.Nick(),
		c.ircClient.IsConnected(),
		c.ircClient.Channels(),
	)

	if c.redis == nil {
		return "--- Funbot Status ---\n" + ctrlStatus + "\nRedis not connected"
	}

	// Specific network detail
	if len(args) > 0 {
		network := c.cmdCtx.ResolveNetwork(args[0])
		return formatNetworkStatus(ctx, c.redis, network)
	}

	// Full status
	workerStatus := formatFullStatus(ctx, c.redis, c.cfg.Controller.HomeNetwork)
	return ctrlStatus + "\n" + workerStatus + "\nContext: " + c.cmdCtx.String()
}

// handleNetworks implements !networks.
func (c *Controller) handleNetworks(args []string) string {
	var lines []string
	lines = append(lines, "Configured networks:")
	for name := range c.cfg.Networks {
		marker := " "
		if name == c.cfg.Controller.HomeNetwork {
			marker = "*"
		}
		lines = append(lines, fmt.Sprintf(" %s %s", marker, name))
	}

	// Also show networks that have active workers (may be runtime-added)
	if c.redis != nil {
		allStates, err := c.redis.GetAllNetworkStates(context.Background())
		if err == nil {
			for network := range allStates {
				if _, exists := c.cfg.Networks[network]; !exists {
					lines = append(lines, fmt.Sprintf("   %s (runtime)", network))
				}
			}
		}
	}

	return strings.Join(lines, "\n")
}

// handleJoin implements !join [network] <#channel> [count].
func (c *Controller) handleJoin(args []string) string {
	if len(args) == 0 {
		return "Usage: !join [network] <#channel> [count]"
	}

	var network, channel string
	var count int

	for _, arg := range args {
		if IsChannel(arg) {
			channel = arg
		} else if n, err := strconv.Atoi(arg); err == nil {
			count = n
		} else if network == "" {
			network = arg
		}
	}

	network = c.cmdCtx.ResolveNetwork(network)
	channel = c.cmdCtx.ResolveChannel(channel)

	if channel == "" {
		return "No channel specified and no context set"
	}

	// If targeting home network and no Redis, handle locally
	if network == "" || network == c.cfg.Controller.HomeNetwork {
		c.ircClient.Join(channel)
		return fmt.Sprintf("Joined %s on %s", channel, c.cfg.Controller.HomeNetwork)
	}

	// Route to workers via Redis
	return c.publishCommand(fnredis.Command{
		Type:    "join",
		Network: network,
		Channel: channel,
		Count:   count,
	})
}

// handlePart implements !part [network] <#channel> [count|all].
func (c *Controller) handlePart(args []string) string {
	if len(args) == 0 {
		channel := c.cmdCtx.Channel()
		if channel == "" {
			return "Usage: !part [network] <#channel> [count|all]"
		}
		c.ircClient.Part(channel)
		return fmt.Sprintf("Parted %s", channel)
	}

	var network, channel string
	var count int

	for _, arg := range args {
		if IsChannel(arg) {
			channel = arg
		} else if arg == "all" {
			count = 0 // 0 means all in the executor
		} else if n, err := strconv.Atoi(arg); err == nil {
			count = n
		} else if network == "" {
			network = arg
		}
	}

	network = c.cmdCtx.ResolveNetwork(network)
	channel = c.cmdCtx.ResolveChannel(channel)

	if channel == "" {
		return "No channel specified"
	}

	if network == "" || network == c.cfg.Controller.HomeNetwork {
		c.ircClient.Part(channel)
		return fmt.Sprintf("Parted %s on %s", channel, c.cfg.Controller.HomeNetwork)
	}

	return c.publishCommand(fnredis.Command{
		Type:    "part",
		Network: network,
		Channel: channel,
		Count:   count,
		Args:    []string{channel},
	})
}

// handleNick implements !nick [network] <client_id|all> <newnick>.
func (c *Controller) handleNick(args []string) string {
	if len(args) == 0 {
		return "Usage: !nick [network] <client_id|all> <newnick>"
	}

	// Single arg: just change controller nick
	if len(args) == 1 {
		c.ircClient.SetNick(args[0])
		return fmt.Sprintf("Changing nick to %s", args[0])
	}

	// Check if first arg is a network
	network := ""
	cmdArgs := args
	if _, exists := c.cfg.Networks[args[0]]; exists {
		network = args[0]
		cmdArgs = args[1:]
	} else {
		network = c.cmdCtx.Network()
	}

	if network == "" || network == c.cfg.Controller.HomeNetwork {
		if len(cmdArgs) > 0 {
			c.ircClient.SetNick(cmdArgs[len(cmdArgs)-1])
			return fmt.Sprintf("Changing nick to %s", cmdArgs[len(cmdArgs)-1])
		}
		return "Usage: !nick <newnick>"
	}

	return c.publishCommand(fnredis.Command{
		Type:    "nick",
		Network: network,
		Args:    cmdArgs,
	})
}

// handlePM implements !pm [network] <user> <count> <message>.
func (c *Controller) handlePM(args []string) string {
	if len(args) < 3 {
		return "Usage: !pm [network] <user> <count> <message>"
	}

	network := ""
	remaining := args

	// Check if first arg is a network
	if _, exists := c.cfg.Networks[args[0]]; exists {
		network = args[0]
		remaining = args[1:]
	} else {
		network = c.cmdCtx.Network()
	}

	if network == "" {
		return "No network specified and no context set"
	}

	if len(remaining) < 3 {
		return "Usage: !pm [network] <user> <count> <message>"
	}

	target := remaining[0]
	count, err := strconv.Atoi(remaining[1])
	if err != nil {
		return fmt.Sprintf("Invalid count: %s", remaining[1])
	}
	message := strings.Join(remaining[2:], " ")

	return c.publishCommand(fnredis.Command{
		Type:    "pm",
		Network: network,
		Target:  target,
		Count:   count,
		Message: message,
	})
}

// handleSay implements !say [network] [#channel] <count> <message>.
func (c *Controller) handleSay(args []string) string {
	if len(args) < 2 {
		return "Usage: !say [network] [#channel] <count> <message>"
	}

	var network, channel string
	var count int
	var messageStart int

	for i, arg := range args {
		if IsChannel(arg) {
			channel = arg
		} else if n, err := strconv.Atoi(arg); err == nil && count == 0 {
			count = n
			messageStart = i + 1
		} else if network == "" && !IsChannel(arg) {
			if _, exists := c.cfg.Networks[arg]; exists {
				network = arg
				continue
			}
			// Not a network, treat as start of message
			messageStart = i
			break
		}
	}

	network = c.cmdCtx.ResolveNetwork(network)
	channel = c.cmdCtx.ResolveChannel(channel)

	if network == "" {
		return "No network specified and no context set"
	}
	if channel == "" {
		return "No channel specified and no context set"
	}
	if messageStart >= len(args) {
		return "No message specified"
	}

	message := strings.Join(args[messageStart:], " ")

	return c.publishCommand(fnredis.Command{
		Type:    "say",
		Network: network,
		Channel: channel,
		Count:   count,
		Message: message,
	})
}

// handleRaw implements !raw [network] <client_id|all> <raw command>.
func (c *Controller) handleRaw(args []string) string {
	if len(args) < 2 {
		return "Usage: !raw [network] <client_id|all> <raw command>"
	}

	network := ""
	cmdArgs := args

	if _, exists := c.cfg.Networks[args[0]]; exists {
		network = args[0]
		cmdArgs = args[1:]
	} else {
		network = c.cmdCtx.Network()
	}

	if network == "" {
		return "No network specified and no context set"
	}

	return c.publishCommand(fnredis.Command{
		Type:    "raw",
		Network: network,
		Args:    cmdArgs,
	})
}

// handleKeepNick implements !keepnick [network] <client_id> <desirednick|stop>.
func (c *Controller) handleKeepNick(args []string) string {
	if len(args) < 2 {
		return "Usage: !keepnick [network] <client_id> <desirednick|stop>"
	}

	network := ""
	cmdArgs := args

	// Check if first arg is a network
	if _, exists := c.cfg.Networks[args[0]]; exists {
		network = args[0]
		cmdArgs = args[1:]
	} else {
		network = c.cmdCtx.Network()
	}

	if network == "" {
		return "No network specified and no context set"
	}

	if len(cmdArgs) < 2 {
		return "Usage: !keepnick [network] <client_id> <desirednick|stop>"
	}

	return c.publishCommand(fnredis.Command{
		Type:    "keepnick",
		Network: network,
		Args:    cmdArgs,
	})
}

// parseServerAddr splits "host:port" into components.
func parseServerAddr(addr string) (string, int, error) {
	parts := strings.SplitN(addr, ":", 2)
	if len(parts) != 2 {
		return addr, 6667, nil // default IRC port
	}
	port, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, fmt.Errorf("invalid port in %q: %w", addr, err)
	}
	return parts[0], port, nil
}
