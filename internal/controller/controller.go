// Package controller implements the Funbot controller role, which
// connects to the home IRC network, accepts commands, and coordinates workers.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/venatiodecorus/funbot/internal/auth"
	"github.com/venatiodecorus/funbot/internal/config"
	"github.com/venatiodecorus/funbot/internal/irc"
)

// Controller manages the home network IRC connection and command handling.
type Controller struct {
	cfg        *config.Config
	ircClient  *irc.Client
	authCheck  *auth.Checker
	cmdCtx     *CommandContext
	dispatcher *CommandDispatcher
	log        *slog.Logger
}

// New creates a new Controller from the given configuration.
func New(cfg *config.Config, log *slog.Logger) (*Controller, error) {
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
		log:        log,
	}

	// Register Phase 1 commands
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

	return c.ircClient.Connect(ctx)
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

// registerCommands registers all Phase 1 command handlers.
func (c *Controller) registerCommands() {
	c.dispatcher.Register("status", c.handleStatus)
	c.dispatcher.Register("networks", c.handleNetworks)
	c.dispatcher.Register("join", c.handleJoin)
	c.dispatcher.Register("part", c.handlePart)
	c.dispatcher.Register("nick", c.handleNick)
}

// handleStatus implements !status [network].
func (c *Controller) handleStatus(args []string) string {
	// Phase 1: Just report the controller's own status
	client := c.ircClient
	channels := client.Channels()

	status := "--- Funbot Status ---\n"
	status += fmt.Sprintf("[%s] Controller: nick=%s, connected=%v, channels=%v",
		c.cfg.Controller.HomeNetwork,
		client.Nick(),
		client.IsConnected(),
		channels,
	)
	status += fmt.Sprintf("\nContext: %s", c.cmdCtx.String())

	return status
}

// handleNetworks implements !networks.
func (c *Controller) handleNetworks(args []string) string {
	// Phase 1: Just list configured networks
	var lines []string
	for name := range c.cfg.Networks {
		marker := " "
		if name == c.cfg.Controller.HomeNetwork {
			marker = "*"
		}
		lines = append(lines, fmt.Sprintf(" %s %s", marker, name))
	}
	return "Configured networks:\n" + strings.Join(lines, "\n")
}

// handleJoin implements !join [network] <#channel> [count].
func (c *Controller) handleJoin(args []string) string {
	if len(args) == 0 {
		return "Usage: !join [network] <#channel> [count]"
	}

	// Phase 1: Only handle local joins on the home network
	var channel string

	for _, arg := range args {
		if IsChannel(arg) {
			channel = arg
			break
		}
	}

	if channel == "" {
		// Try context channel
		channel = c.cmdCtx.Channel()
	}

	if channel == "" {
		return "No channel specified and no context set"
	}

	c.ircClient.Join(channel)
	return fmt.Sprintf("Joined %s", channel)
}

// handlePart implements !part [network] <#channel>.
func (c *Controller) handlePart(args []string) string {
	if len(args) == 0 {
		// Try context channel
		channel := c.cmdCtx.Channel()
		if channel == "" {
			return "Usage: !part [network] <#channel>"
		}
		c.ircClient.Part(channel)
		return fmt.Sprintf("Parted %s", channel)
	}

	var channel string
	for _, arg := range args {
		if IsChannel(arg) {
			channel = arg
			break
		}
	}

	if channel == "" {
		return "No channel specified"
	}

	c.ircClient.Part(channel)
	return fmt.Sprintf("Parted %s", channel)
}

// handleNick implements !nick [network] <newnick>.
func (c *Controller) handleNick(args []string) string {
	if len(args) == 0 {
		return "Usage: !nick [network] <newnick>"
	}

	// Phase 1: Just change the controller's nick
	newNick := args[len(args)-1]
	c.ircClient.SetNick(newNick)
	return fmt.Sprintf("Changing nick to %s", newNick)
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
