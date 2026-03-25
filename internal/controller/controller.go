// Package controller implements the Funbot controller role, which
// connects to the home IRC network, accepts commands, and coordinates workers.
package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/venatiodecorus/funbot/internal/art"
	"github.com/venatiodecorus/funbot/internal/auth"
	"github.com/venatiodecorus/funbot/internal/config"
	"github.com/venatiodecorus/funbot/internal/health"
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
	scaler     *Scaler
	artRepo    *art.Repo
	artCatalog *art.Catalog
	log        *slog.Logger
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

	// Set up art repo and catalog
	var artRepo *art.Repo
	var artCatalog *art.Catalog
	if cfg.Art.RepoURL != "" && cfg.Art.LocalPath != "" {
		interval, err := time.ParseDuration(cfg.Art.UpdateInterval)
		if err != nil {
			interval = 1 * time.Hour
		}
		artRepo = art.NewRepo(cfg.Art.RepoURL, cfg.Art.LocalPath, interval, log)
		artCatalog = art.NewCatalog(cfg.Art.LocalPath, log)
	}

	// Initialize Kubernetes scaler (nil when not in cluster)
	scaler := NewScaler(log)

	ctrl := &Controller{
		cfg:        cfg,
		ircClient:  ircClient,
		authCheck:  authChecker,
		cmdCtx:     cmdCtx,
		dispatcher: dispatcher,
		redis:      redisClient,
		scaler:     scaler,
		artRepo:    artRepo,
		artCatalog: artCatalog,
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

// Run starts the controller. It blocks until the context is cancelled.
// The IRC connection automatically reconnects on disconnection.
func (c *Controller) Run(ctx context.Context) error {
	c.log.Info("starting controller",
		"home_network", c.cfg.Controller.HomeNetwork,
		"auth_nick", c.cfg.Controller.Auth.Nick,
	)

	// Initialize art repo if configured
	if c.artRepo != nil {
		if err := c.artRepo.Init(ctx); err != nil {
			c.log.Warn("failed to initialize art repo", "error", err)
		} else {
			if err := c.artCatalog.Refresh(); err != nil {
				c.log.Warn("failed to refresh art catalog", "error", err)
			}
			go c.artRepo.StartUpdater(ctx)
		}
	}

	// Start listening for acks from workers
	if c.redis != nil {
		go c.listenAcks(ctx)
	}

	// Start health check server
	healthSrv := health.New("", c.IsReady, c.log)
	healthSrv.Start()
	defer healthSrv.Shutdown()

	// Connect with automatic reconnection — controller should always stay connected
	return c.ircClient.ConnectWithRetry(ctx, 0)
}

// IsReady returns true when the controller is connected to IRC.
func (c *Controller) IsReady() bool {
	return c.ircClient.IsConnected()
}

// listenAcks subscribes to the ack channel and logs results.
// It automatically resubscribes if the connection is lost.
// Ack messages are also relayed back to the authorized user via IRC PM.
func (c *Controller) listenAcks(ctx context.Context) {
	const resubscribeDelay = 3 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		ackCh, err := c.redis.SubscribeAcks(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.log.Error("failed to subscribe to acks, retrying", "error", err, "delay", resubscribeDelay)
			select {
			case <-ctx.Done():
				return
			case <-time.After(resubscribeDelay):
				continue
			}
		}

		for {
			select {
			case <-ctx.Done():
				return
			case ack, ok := <-ackCh:
				if !ok {
					c.log.Warn("ack channel closed, resubscribing")
					select {
					case <-ctx.Done():
						return
					case <-time.After(resubscribeDelay):
					}
					break // resubscribe
				}
				c.log.Info("received ack",
					"command_id", ack.CommandID,
					"pod", ack.Pod,
					"success", ack.Success,
					"message", ack.Message,
				)
				// Relay ack result to authorized user
				if c.ircClient.IsConnected() {
					status := "OK"
					if !ack.Success {
						status = "FAIL"
					}
					msg := fmt.Sprintf("[%s] %s@%s: %s", status, ack.CommandID, ack.Pod, ack.Message)
					c.ircClient.Privmsg(c.cfg.Controller.Auth.Nick, msg)
				}
				continue
			}
			break
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
		c.log.Warn("attempted to publish command without Redis connection", "type", cmd.Type, "network", cmd.Network)
		return "Redis not connected — cannot route to workers"
	}

	cmd.ID = c.generateCommandID()

	if err := c.redis.PublishCommand(context.Background(), cmd); err != nil {
		c.log.Error("failed to publish command", "type", cmd.Type, "id", cmd.ID, "network", cmd.Network, "error", err)
		return fmt.Sprintf("Error sending command: %v", err)
	}

	c.log.Info("command published", "type", cmd.Type, "id", cmd.ID, "network", cmd.Network)
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
	c.dispatcher.Register("art", c.handleArt)
	c.dispatcher.Register("artlist", c.handleArtList)
	c.dispatcher.Register("artsearch", c.handleArtSearch)
	c.dispatcher.Register("scale", c.handleScale)
	c.dispatcher.Register("connect", c.handleConnect)
	c.dispatcher.Register("disconnect", c.handleDisconnect)
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

// handleArt implements !art [network] [#channel] [count] <artname>.
func (c *Controller) handleArt(args []string) string {
	if len(args) == 0 {
		return "Usage: !art [network] [#channel] [count] <artname>"
	}

	var network, channel, artName string
	var count int

	// Parse args: the last non-numeric, non-channel, non-network arg is the art name.
	// We parse in order: network, channel, count can appear in any order before artname.
	remaining := make([]string, 0, len(args))
	for _, arg := range args {
		if IsChannel(arg) {
			channel = arg
		} else if n, err := strconv.Atoi(arg); err == nil {
			count = n
		} else if network == "" {
			if _, exists := c.cfg.Networks[arg]; exists {
				network = arg
				continue
			}
			remaining = append(remaining, arg)
		} else {
			remaining = append(remaining, arg)
		}
	}

	if len(remaining) == 0 {
		return "No art name specified"
	}
	artName = remaining[len(remaining)-1]

	network = c.cmdCtx.ResolveNetwork(network)
	channel = c.cmdCtx.ResolveChannel(channel)

	if network == "" {
		return "No network specified and no context set"
	}
	if channel == "" {
		return "No channel specified and no context set"
	}

	// Verify art exists before sending to workers
	if c.artCatalog != nil {
		entries := c.artCatalog.FindByName(artName)
		if len(entries) == 0 {
			return fmt.Sprintf("Art %q not found. Use !artsearch to search.", artName)
		}
	}

	return c.publishCommand(fnredis.Command{
		Type:    "art",
		Network: network,
		Channel: channel,
		Count:   count,
		Args:    []string{artName},
	})
}

// handleArtList implements !artlist [category].
func (c *Controller) handleArtList(args []string) string {
	if c.artCatalog == nil {
		return "Art catalog not initialized"
	}

	if len(args) == 0 {
		// List categories
		cats := c.artCatalog.ListCategories()
		if len(cats) == 0 {
			return fmt.Sprintf("No art files found (%d total indexed)", c.artCatalog.Count())
		}

		var lines []string
		lines = append(lines, fmt.Sprintf("Art categories (%d files total):", c.artCatalog.Count()))
		for _, cat := range cats {
			entries := c.artCatalog.ListByCategory(cat)
			lines = append(lines, fmt.Sprintf("  %s (%d files)", cat, len(entries)))
		}

		// Limit output to avoid flooding
		if len(lines) > 30 {
			lines = lines[:30]
			lines = append(lines, "  ... (truncated, use !artlist <category> to browse)")
		}
		return strings.Join(lines, "\n")
	}

	// List files in a category
	category := args[0]
	entries := c.artCatalog.ListByCategory(category)
	if len(entries) == 0 {
		return fmt.Sprintf("No art files found in category %q", category)
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("Art in %q (%d files):", category, len(entries)))
	for _, e := range entries {
		lines = append(lines, fmt.Sprintf("  %s", e.Name))
	}

	if len(lines) > 30 {
		lines = lines[:30]
		lines = append(lines, "  ... (truncated)")
	}
	return strings.Join(lines, "\n")
}

// handleArtSearch implements !artsearch <query>.
func (c *Controller) handleArtSearch(args []string) string {
	if c.artCatalog == nil {
		return "Art catalog not initialized"
	}

	if len(args) == 0 {
		return "Usage: !artsearch <query>"
	}

	query := strings.Join(args, " ")
	results := c.artCatalog.Search(query)

	if len(results) == 0 {
		return fmt.Sprintf("No art files matching %q", query)
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("Search results for %q (%d matches):", query, len(results)))
	for _, e := range results {
		if e.Category != "" {
			lines = append(lines, fmt.Sprintf("  %s/%s", e.Category, e.Name))
		} else {
			lines = append(lines, fmt.Sprintf("  %s", e.Name))
		}
	}

	if len(lines) > 20 {
		lines = lines[:20]
		lines = append(lines, "  ... (truncated, refine your search)")
	}
	return strings.Join(lines, "\n")
}

// handleScale implements !scale <network> <replicas>.
func (c *Controller) handleScale(args []string) string {
	if c.scaler == nil {
		return "Scaling not available (not running in Kubernetes)"
	}

	if len(args) < 2 {
		return "Usage: !scale <network> <replicas>"
	}

	network := args[0]
	replicas, err := strconv.Atoi(args[1])
	if err != nil || replicas < 0 {
		return fmt.Sprintf("Invalid replica count: %s", args[1])
	}

	ctx := context.Background()

	exists, err := c.scaler.DeploymentExists(ctx, network)
	if err != nil || !exists {
		return fmt.Sprintf("No worker deployment found for network %q", network)
	}

	currentReplicas, err := c.scaler.GetReplicas(ctx, network)
	if err != nil {
		return fmt.Sprintf("Error getting current replicas: %v", err)
	}

	if err := c.scaler.Scale(ctx, network, int32(replicas)); err != nil {
		return fmt.Sprintf("Error scaling: %v", err)
	}

	return fmt.Sprintf("Scaled %s from %d to %d replicas", network, currentReplicas, replicas)
}

// handleConnect implements !connect <network> <server:port> [ssl].
// Creates a new worker Deployment for the network.
func (c *Controller) handleConnect(args []string) string {
	if len(args) < 2 {
		return "Usage: !connect <network> <server:port> [ssl]"
	}

	network := args[0]
	serverAddr := args[1]
	ssl := false
	if len(args) >= 3 && args[2] == "ssl" {
		ssl = true
	}

	// Check if network already exists in config
	if _, exists := c.cfg.Networks[network]; exists {
		return fmt.Sprintf("Network %q already exists in config", network)
	}

	// Add network to runtime config
	server, port, err := parseServerAddr(serverAddr)
	if err != nil {
		return fmt.Sprintf("Invalid server address %q: %v", serverAddr, err)
	}

	c.cfg.Networks[network] = config.Network{
		Servers:         []string{fmt.Sprintf("%s:%d", server, port)},
		SSL:             ssl,
		NickPrefix:      "fun",
		MaxClientsPerIP: 3,
		Channels:        []string{},
		FloodDelayMs:    1000,
	}

	if c.scaler == nil {
		return fmt.Sprintf("Network %q added to config (runtime only). Scaling not available outside Kubernetes — start a worker manually with FUNBOT_NETWORK=%s", network, network)
	}

	ctx := context.Background()

	// Determine the image to use from existing deployments
	image := "funbot:latest"
	deployments, err := c.scaler.ListWorkerDeployments(ctx)
	if err == nil && len(deployments) > 0 {
		containers := deployments[0].Spec.Template.Spec.Containers
		if len(containers) > 0 {
			image = containers[0].Image
		}
	}

	if err := c.scaler.CreateWorkerDeployment(ctx, network, image, 1); err != nil {
		return fmt.Sprintf("Network %q added to config but failed to create deployment: %v", network, err)
	}

	return fmt.Sprintf("Network %q added. Worker deployment created with 1 replica. Note: this is runtime-only and will not survive controller restart.", network)
}

// handleDisconnect implements !disconnect <network>.
// Removes the network's worker Deployment and cleans up.
func (c *Controller) handleDisconnect(args []string) string {
	if len(args) == 0 {
		return "Usage: !disconnect <network>"
	}

	network := args[0]

	// Don't allow disconnecting the home network
	if network == c.cfg.Controller.HomeNetwork {
		return "Cannot disconnect the home network"
	}

	ctx := context.Background()

	// Send disconnect command to workers so they QUIT cleanly
	if c.redis != nil {
		c.publishCommand(fnredis.Command{
			Type:    "disconnect",
			Network: network,
		})
	}

	// Delete the deployment if we have a scaler
	if c.scaler != nil {
		if err := c.scaler.DeleteWorkerDeployment(ctx, network); err != nil {
			c.log.Warn("failed to delete deployment during disconnect", "network", network, "error", err)
		}
	}

	// Remove from runtime config
	delete(c.cfg.Networks, network)

	return fmt.Sprintf("Disconnected from %s. Workers shutting down.", network)
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
