// Package bot implements the single-process Funbot IRC bot that manages
// multiple client connections across networks using SOCKS5 proxies.
package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/venatiodecorus/funbot/internal/art"
	"github.com/venatiodecorus/funbot/internal/auth"
	"github.com/venatiodecorus/funbot/internal/config"
	"github.com/venatiodecorus/funbot/internal/irc"
	"github.com/venatiodecorus/funbot/internal/proxy"
)

// DefaultProxyRefreshInterval is how often to re-fetch proxies from the API.
const DefaultProxyRefreshInterval = 5 * time.Minute

// Bot is the single-process Funbot that manages all IRC connections.
type Bot struct {
	cfg        *config.Config
	homeClient *irc.Client
	authCheck  *auth.Checker
	cmdCtx     *CommandContext
	dispatcher *CommandDispatcher
	networks   map[string]*NetworkManager
	netMu      sync.RWMutex
	proxyPool  *proxy.Pool
	artRepo    *art.Repo
	artCatalog *art.Catalog
	log        *slog.Logger
}

// New creates a new Bot from the given configuration.
func New(cfg *config.Config, log *slog.Logger) (*Bot, error) {
	homeNet, ok := cfg.Networks[cfg.HomeNetwork]
	if !ok {
		return nil, fmt.Errorf("home network %q not found in config", cfg.HomeNetwork)
	}

	if len(homeNet.Servers) == 0 {
		return nil, fmt.Errorf("home network %q has no servers", cfg.HomeNetwork)
	}

	// Parse server address
	server, port, err := parseServerAddr(homeNet.Servers[0])
	if err != nil {
		return nil, fmt.Errorf("parsing home network server: %w", err)
	}

	ircCfg := irc.ClientConfig{
		ID:       cfg.HomeNetwork + "-ctrl",
		Network:  cfg.HomeNetwork,
		Server:   server,
		Port:     port,
		SSL:      homeNet.SSL,
		Nick:     homeNet.NickPrefix,
		User:     "funbot",
		Realname: "Funbot",
		Logger:   log,
	}

	homeClient := irc.New(ircCfg)
	authChecker := auth.New(cfg.Auth.Nick, cfg.Auth.Hostname)
	cmdCtx := NewCommandContext()

	prefix := cfg.CommandPrefix
	if prefix == "" {
		prefix = "!"
	}

	dispatcher := NewCommandDispatcher(prefix, cmdCtx, log)

	// Set up proxy pool
	proxyPool := proxy.NewPool(log)
	if cfg.Proxies.MaxRetries > 0 {
		proxyPool.SetMaxRetries(cfg.Proxies.MaxRetries)
	}

	switch cfg.Proxies.Source {
	case config.ProxySourceRotating:
		proxyPool.SetRotatingProxy(cfg.Proxies.RotatingAddr, cfg.Proxies.RotatingUser, cfg.Proxies.RotatingPass)
		log.Info("proxy pool configured with rotating proxy",
			"addr", cfg.Proxies.RotatingAddr)
	default:
		// "api" or empty (backwards compatible default)
		if cfg.Proxies.APIURL != "" {
			proxyPool.SetAPI(cfg.Proxies.APIURL, cfg.Proxies.Protocol, cfg.Proxies.MaxLatency)
		}
		if cfg.Proxies.MinPoolSize > 0 {
			proxyPool.SetMinPoolSize(cfg.Proxies.MinPoolSize)
		}
	}

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

	b := &Bot{
		cfg:        cfg,
		homeClient: homeClient,
		authCheck:  authChecker,
		cmdCtx:     cmdCtx,
		dispatcher: dispatcher,
		networks:   make(map[string]*NetworkManager),
		proxyPool:  proxyPool,
		artRepo:    artRepo,
		artCatalog: artCatalog,
		log:        log,
	}

	// Register commands
	b.registerCommands()

	// Set up PRIVMSG handler for incoming commands
	homeClient.OnPrivmsg(b.handlePrivmsg)

	// Auto-join configured channels on connect
	channels := homeNet.Channels
	homeClient.OnConnect(func() {
		for _, ch := range channels {
			homeClient.Join(ch)
		}
	})

	return b, nil
}

// Run starts the bot. It blocks until the context is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	b.log.Info("starting funbot",
		"home_network", b.cfg.HomeNetwork,
		"auth_nick", b.cfg.Auth.Nick,
		"proxies", b.proxyPool.Count(),
	)

	// Initialize art repo if configured
	if b.artRepo != nil {
		if err := b.artRepo.Init(ctx); err != nil {
			b.log.Warn("failed to initialize art repo", "error", err)
		} else {
			if err := b.artCatalog.Refresh(); err != nil {
				b.log.Warn("failed to refresh art catalog", "error", err)
			}
			go b.artRepo.StartUpdater(ctx)
		}
	}

	// Initialize proxy pool based on source type
	if b.proxyPool.IsRotating() {
		b.log.Info("using rotating proxy source, no API fetch needed")
	} else if b.cfg.Proxies.APIURL != "" {
		// API source: fetch initial proxies and start background refresher
		if err := b.proxyPool.FetchFromAPI(ctx); err != nil {
			b.log.Warn("failed to fetch initial proxies from API", "error", err)
		}

		refreshInterval := DefaultProxyRefreshInterval
		if b.cfg.Proxies.RefreshInterval != "" {
			if d, err := time.ParseDuration(b.cfg.Proxies.RefreshInterval); err == nil {
				refreshInterval = d
			}
		}
		go b.proxyPool.StartRefresher(ctx, refreshInterval)
	}

	// Start configured non-home networks
	for name, netCfg := range b.cfg.Networks {
		if name == b.cfg.HomeNetwork {
			continue
		}
		if netCfg.DefaultClients <= 0 {
			continue
		}
		nm, err := NewNetworkManager(name, netCfg, b.proxyPool, b.log)
		if err != nil {
			b.log.Warn("failed to create network manager", "network", name, "error", err)
			continue
		}
		if err := nm.Start(ctx); err != nil {
			b.log.Warn("failed to start network", "network", name, "error", err)
			continue
		}
		b.netMu.Lock()
		b.networks[name] = nm
		b.netMu.Unlock()
	}

	// Connect home client with automatic reconnection
	return b.homeClient.ConnectWithRetry(ctx, 0)
}

// handlePrivmsg processes incoming private messages, checking auth
// and dispatching commands.
func (b *Bot) handlePrivmsg(nick, hostname, target, message string) {
	// Only process private messages (target is our nick, not a channel)
	if IsChannel(target) {
		return
	}

	if !b.authCheck.IsAuthorized(nick, hostname) {
		b.log.Debug("unauthorized command attempt", "nick", nick, "hostname", hostname)
		return
	}

	response := b.dispatcher.Dispatch(message)
	if response != "" {
		for _, line := range strings.Split(response, "\n") {
			if line != "" {
				b.homeClient.Privmsg(nick, line)
			}
		}
	}
}

// registerCommands registers all command handlers.
func (b *Bot) registerCommands() {
	b.dispatcher.Register("status", b.handleStatus)
	b.dispatcher.Register("networks", b.handleNetworks)
	b.dispatcher.Register("join", b.handleJoin)
	b.dispatcher.Register("part", b.handlePart)
	b.dispatcher.Register("nick", b.handleNick)
	b.dispatcher.Register("keepnick", b.handleKeepNick)
	b.dispatcher.Register("pm", b.handlePM)
	b.dispatcher.Register("say", b.handleSay)
	b.dispatcher.Register("raw", b.handleRaw)
	b.dispatcher.Register("art", b.handleArt)
	b.dispatcher.Register("artlist", b.handleArtList)
	b.dispatcher.Register("artsearch", b.handleArtSearch)
	b.dispatcher.Register("connect", b.handleConnect)
	b.dispatcher.Register("disconnect", b.handleDisconnect)
	b.dispatcher.Register("addclients", b.handleAddClients)
	b.dispatcher.Register("rmclients", b.handleRmClients)
}

// getNetwork returns the NetworkManager for the given network name.
func (b *Bot) getNetwork(name string) *NetworkManager {
	b.netMu.RLock()
	defer b.netMu.RUnlock()
	return b.networks[name]
}

// isKnownNetwork returns true if the network name exists in config or active networks.
func (b *Bot) isKnownNetwork(name string) bool {
	if _, ok := b.cfg.Networks[name]; ok {
		return true
	}
	b.netMu.RLock()
	_, ok := b.networks[name]
	b.netMu.RUnlock()
	return ok
}

// handleStatus implements !status [network].
func (b *Bot) handleStatus(args []string) string {
	// Controller's own status line
	ctrlStatus := fmt.Sprintf("[*%s] Home: nick=%s, connected=%v, channels=%v",
		b.cfg.HomeNetwork,
		b.homeClient.Nick(),
		b.homeClient.IsConnected(),
		b.homeClient.Channels(),
	)

	// Proxy pool status
	var proxyStatus string
	if b.proxyPool.IsRotating() {
		proxyStatus = fmt.Sprintf("Proxies: rotating (%d active connections)",
			b.proxyPool.Count())
	} else {
		proxyStatus = fmt.Sprintf("Proxies: %d total, %d healthy",
			b.proxyPool.Count(), b.proxyPool.HealthyCount())
	}

	// Specific network detail
	if len(args) > 0 {
		network := b.cmdCtx.ResolveNetwork(args[0])
		return b.formatNetworkStatus(network)
	}

	// Full status
	b.netMu.RLock()
	var lines []string
	lines = append(lines, "--- Funbot Status ---")
	lines = append(lines, ctrlStatus)
	lines = append(lines, proxyStatus)

	if len(b.networks) == 0 {
		lines = append(lines, "No active networks")
	} else {
		for name, nm := range b.networks {
			state := nm.GetState()
			connected := 0
			var channels []string
			channelSet := make(map[string]bool)
			for _, c := range state.Clients {
				if c.Connected {
					connected++
				}
				for _, ch := range c.Channels {
					if !channelSet[ch] {
						channelSet[ch] = true
						channels = append(channels, ch)
					}
				}
			}
			chStr := "none"
			if len(channels) > 0 {
				chStr = strings.Join(channels, ", ")
			}
			lines = append(lines, fmt.Sprintf("[%s] %d/%d clients connected, channels: %s",
				name, connected, len(state.Clients), chStr))
		}
	}
	b.netMu.RUnlock()

	lines = append(lines, "Context: "+b.cmdCtx.String())
	return strings.Join(lines, "\n")
}

// formatNetworkStatus generates a detailed status for a specific network.
func (b *Bot) formatNetworkStatus(network string) string {
	nm := b.getNetwork(network)
	if nm == nil {
		return fmt.Sprintf("[%s] No active connections", network)
	}

	state := nm.GetState()
	var lines []string
	lines = append(lines, fmt.Sprintf("--- %s Status ---", network))

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
		lines = append(lines, fmt.Sprintf("  [%s] nick=%s %s channels=[%s]%s",
			client.ID, client.Nick, status, channels, extra))
	}

	return strings.Join(lines, "\n")
}

// handleNetworks implements !networks.
func (b *Bot) handleNetworks(args []string) string {
	var lines []string
	lines = append(lines, "Configured networks:")
	for name := range b.cfg.Networks {
		marker := " "
		if name == b.cfg.HomeNetwork {
			marker = "*"
		}
		lines = append(lines, fmt.Sprintf(" %s %s", marker, name))
	}

	// Show runtime-added networks
	b.netMu.RLock()
	for name := range b.networks {
		if _, exists := b.cfg.Networks[name]; !exists {
			lines = append(lines, fmt.Sprintf("   %s (runtime)", name))
		}
	}
	b.netMu.RUnlock()

	return strings.Join(lines, "\n")
}

// handleJoin implements !join [network] <#channel> [count].
func (b *Bot) handleJoin(args []string) string {
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

	network = b.cmdCtx.ResolveNetwork(network)
	channel = b.cmdCtx.ResolveChannel(channel)

	if channel == "" {
		return "No channel specified and no context set"
	}

	// If targeting home network, handle locally
	if network == "" || network == b.cfg.HomeNetwork {
		b.homeClient.Join(channel)
		return fmt.Sprintf("Joined %s on %s", channel, b.cfg.HomeNetwork)
	}

	// Route to network manager
	nm := b.getNetwork(network)
	if nm == nil {
		return fmt.Sprintf("Network %q is not connected. Use !connect first.", network)
	}

	clients := nm.SelectClients(count)
	if len(clients) == 0 {
		return "No connected clients available"
	}

	for _, c := range clients {
		c.Join(channel)
	}

	return fmt.Sprintf("Joined %d client(s) to %s on %s", len(clients), channel, network)
}

// handlePart implements !part [network] <#channel> [count|all].
func (b *Bot) handlePart(args []string) string {
	if len(args) == 0 {
		channel := b.cmdCtx.Channel()
		if channel == "" {
			return "Usage: !part [network] <#channel> [count|all]"
		}
		b.homeClient.Part(channel)
		return fmt.Sprintf("Parted %s", channel)
	}

	var network, channel string
	var count int

	for _, arg := range args {
		if IsChannel(arg) {
			channel = arg
		} else if arg == "all" {
			count = 0
		} else if n, err := strconv.Atoi(arg); err == nil {
			count = n
		} else if network == "" {
			network = arg
		}
	}

	network = b.cmdCtx.ResolveNetwork(network)
	channel = b.cmdCtx.ResolveChannel(channel)

	if channel == "" {
		return "No channel specified"
	}

	if network == "" || network == b.cfg.HomeNetwork {
		b.homeClient.Part(channel)
		return fmt.Sprintf("Parted %s on %s", channel, b.cfg.HomeNetwork)
	}

	nm := b.getNetwork(network)
	if nm == nil {
		return fmt.Sprintf("Network %q is not connected", network)
	}

	clients := nm.SelectClients(count)
	if len(clients) == 0 {
		return "No connected clients available"
	}

	for _, c := range clients {
		c.Part(channel)
	}

	return fmt.Sprintf("Parted %d client(s) from %s on %s", len(clients), channel, network)
}

// handleNick implements !nick [network] <client_id|all> <newnick>.
func (b *Bot) handleNick(args []string) string {
	if len(args) == 0 {
		return "Usage: !nick [network] <client_id|all> <newnick>"
	}

	// Single arg: change home client nick
	if len(args) == 1 {
		b.homeClient.SetNick(args[0])
		return fmt.Sprintf("Changing nick to %s", args[0])
	}

	// Check if first arg is a network
	network := ""
	cmdArgs := args
	if b.isKnownNetwork(args[0]) {
		network = args[0]
		cmdArgs = args[1:]
	} else {
		network = b.cmdCtx.Network()
	}

	if network == "" || network == b.cfg.HomeNetwork {
		if len(cmdArgs) > 0 {
			b.homeClient.SetNick(cmdArgs[len(cmdArgs)-1])
			return fmt.Sprintf("Changing nick to %s", cmdArgs[len(cmdArgs)-1])
		}
		return "Usage: !nick <newnick>"
	}

	nm := b.getNetwork(network)
	if nm == nil {
		return fmt.Sprintf("Network %q is not connected", network)
	}

	if len(cmdArgs) < 2 {
		return "Usage: !nick [network] <client_id|all> <newnick>"
	}

	target := cmdArgs[0]
	newNick := cmdArgs[1]

	if target == "all" {
		clients := nm.ConnectedClients()
		for i, c := range clients {
			c.SetNick(fmt.Sprintf("%s%d", newNick, i))
		}
		return fmt.Sprintf("Changing nick for %d clients to %s*", len(clients), newNick)
	}

	client := nm.ClientByID(target)
	if client == nil {
		return fmt.Sprintf("Client %s not found", target)
	}

	client.SetNick(newNick)
	return fmt.Sprintf("Changing nick for %s to %s", target, newNick)
}

// handleKeepNick implements !keepnick [network] <client_id> <desirednick|stop>.
func (b *Bot) handleKeepNick(args []string) string {
	if len(args) < 2 {
		return "Usage: !keepnick [network] <client_id> <desirednick|stop>"
	}

	network := ""
	cmdArgs := args

	if b.isKnownNetwork(args[0]) {
		network = args[0]
		cmdArgs = args[1:]
	} else {
		network = b.cmdCtx.Network()
	}

	if network == "" {
		return "No network specified and no context set"
	}

	nm := b.getNetwork(network)
	if nm == nil {
		return fmt.Sprintf("Network %q is not connected", network)
	}

	if len(cmdArgs) < 2 {
		return "Usage: !keepnick [network] <client_id> <desirednick|stop>"
	}

	clientID := cmdArgs[0]
	desiredNick := cmdArgs[1]

	if desiredNick == "stop" {
		return nm.StopKeepNick(clientID)
	}

	return nm.StartKeepNick(clientID, desiredNick)
}

// handlePM implements !pm [network] <user> <count> <message>.
func (b *Bot) handlePM(args []string) string {
	if len(args) < 3 {
		return "Usage: !pm [network] <user> <count> <message>"
	}

	network := ""
	remaining := args

	if b.isKnownNetwork(args[0]) {
		network = args[0]
		remaining = args[1:]
	} else {
		network = b.cmdCtx.Network()
	}

	if network == "" {
		return "No network specified and no context set"
	}

	if len(remaining) < 3 {
		return "Usage: !pm [network] <user> <count> <message>"
	}

	nm := b.getNetwork(network)
	if nm == nil {
		return fmt.Sprintf("Network %q is not connected", network)
	}

	target := remaining[0]
	count, err := strconv.Atoi(remaining[1])
	if err != nil {
		return fmt.Sprintf("Invalid count: %s", remaining[1])
	}
	message := strings.Join(remaining[2:], " ")

	clients := nm.SelectClients(count)
	if len(clients) == 0 {
		return "No connected clients available"
	}

	for _, c := range clients {
		c.Privmsg(target, message)
	}

	return fmt.Sprintf("Sent PM to %s from %d client(s) on %s", target, len(clients), network)
}

// handleSay implements !say [network] [#channel] <count> <message>.
func (b *Bot) handleSay(args []string) string {
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
			if b.isKnownNetwork(arg) {
				network = arg
				continue
			}
			messageStart = i
			break
		}
	}

	network = b.cmdCtx.ResolveNetwork(network)
	channel = b.cmdCtx.ResolveChannel(channel)

	if network == "" {
		return "No network specified and no context set"
	}
	if channel == "" {
		return "No channel specified and no context set"
	}
	if messageStart >= len(args) {
		return "No message specified"
	}

	nm := b.getNetwork(network)
	if nm == nil {
		return fmt.Sprintf("Network %q is not connected", network)
	}

	message := strings.Join(args[messageStart:], " ")

	clients := nm.SelectClients(count)
	if len(clients) == 0 {
		return "No connected clients available"
	}

	for _, c := range clients {
		c.Privmsg(channel, message)
	}

	return fmt.Sprintf("Sent message to %s from %d client(s) on %s", channel, len(clients), network)
}

// handleRaw implements !raw [network] <client_id|all> <raw command>.
func (b *Bot) handleRaw(args []string) string {
	if len(args) < 2 {
		return "Usage: !raw [network] <client_id|all> <raw command>"
	}

	network := ""
	cmdArgs := args

	if b.isKnownNetwork(args[0]) {
		network = args[0]
		cmdArgs = args[1:]
	} else {
		network = b.cmdCtx.Network()
	}

	if network == "" {
		return "No network specified and no context set"
	}

	nm := b.getNetwork(network)
	if nm == nil {
		return fmt.Sprintf("Network %q is not connected", network)
	}

	if len(cmdArgs) < 2 {
		return "Usage: !raw [network] <client_id|all> <raw command>"
	}

	target := cmdArgs[0]
	rawCmd := strings.Join(cmdArgs[1:], " ")

	if target == "all" {
		clients := nm.ConnectedClients()
		for _, c := range clients {
			c.SendRaw(rawCmd)
		}
		return fmt.Sprintf("Sent raw command to %d clients on %s", len(clients), network)
	}

	client := nm.ClientByID(target)
	if client == nil {
		return fmt.Sprintf("Client %s not found", target)
	}

	client.SendRaw(rawCmd)
	return fmt.Sprintf("Sent raw command via %s", target)
}

// handleArt implements !art [network] [#channel] [count] <artname>.
func (b *Bot) handleArt(args []string) string {
	if len(args) == 0 {
		return "Usage: !art [network] [#channel] [count] <artname>"
	}

	var network, channel, artName string
	var count int

	remaining := make([]string, 0, len(args))
	for _, arg := range args {
		if IsChannel(arg) {
			channel = arg
		} else if n, err := strconv.Atoi(arg); err == nil {
			count = n
		} else if network == "" {
			if b.isKnownNetwork(arg) {
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

	network = b.cmdCtx.ResolveNetwork(network)
	channel = b.cmdCtx.ResolveChannel(channel)

	if network == "" {
		return "No network specified and no context set"
	}
	if channel == "" {
		return "No channel specified and no context set"
	}

	// Verify art exists
	if b.artCatalog == nil {
		return "Art catalog not initialized"
	}
	entries := b.artCatalog.FindByName(artName)
	if len(entries) == 0 {
		return fmt.Sprintf("Art %q not found. Use !artsearch to search.", artName)
	}

	nm := b.getNetwork(network)
	if nm == nil {
		return fmt.Sprintf("Network %q is not connected", network)
	}

	// Load the art lines
	entry := entries[0]
	lines, err := art.LoadArt(entry.Path)
	if err != nil {
		return fmt.Sprintf("Error loading art %q: %v", artName, err)
	}
	if len(lines) == 0 {
		return fmt.Sprintf("Art %q is empty", artName)
	}

	clients := nm.SelectClients(count)
	if len(clients) == 0 {
		return "No connected clients available"
	}

	floodDelay := nm.Config().FloodDelay()
	if floodDelay <= 0 {
		floodDelay = 500 * time.Millisecond
	}

	player := art.NewPlayer(floodDelay, nm.FloodGuard(), b.log)

	go func() {
		if err := player.Play(context.Background(), channel, clients, lines); err != nil {
			b.log.Error("art playback error", "art", artName, "error", err)
		}
	}()

	return fmt.Sprintf("Playing %q (%d lines) in %s with %d client(s) on %s",
		artName, len(lines), channel, len(clients), network)
}

// handleArtList implements !artlist [category].
func (b *Bot) handleArtList(args []string) string {
	if b.artCatalog == nil {
		return "Art catalog not initialized"
	}

	if len(args) == 0 {
		cats := b.artCatalog.ListCategories()
		if len(cats) == 0 {
			return fmt.Sprintf("No art files found (%d total indexed)", b.artCatalog.Count())
		}

		var lines []string
		lines = append(lines, fmt.Sprintf("Art categories (%d files total):", b.artCatalog.Count()))
		for _, cat := range cats {
			entries := b.artCatalog.ListByCategory(cat)
			lines = append(lines, fmt.Sprintf("  %s (%d files)", cat, len(entries)))
		}

		if len(lines) > 30 {
			lines = lines[:30]
			lines = append(lines, "  ... (truncated, use !artlist <category> to browse)")
		}
		return strings.Join(lines, "\n")
	}

	category := args[0]
	entries := b.artCatalog.ListByCategory(category)
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
func (b *Bot) handleArtSearch(args []string) string {
	if b.artCatalog == nil {
		return "Art catalog not initialized"
	}

	if len(args) == 0 {
		return "Usage: !artsearch <query>"
	}

	query := strings.Join(args, " ")
	results := b.artCatalog.Search(query)

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

// handleConnect implements !connect <network> <server:port> [nick_prefix|nicks:strategy] [count] [ssl].
// Nick strategy can be specified as "nicks:random", "nicks:wordlist", or a plain string for prefix strategy.
func (b *Bot) handleConnect(args []string) string {
	if len(args) < 2 {
		return "Usage: !connect <network> <server:port> [nick_prefix|nicks:<strategy>] [count] [ssl]"
	}

	network := args[0]
	serverAddr := args[1]

	// Check if network is already active
	if nm := b.getNetwork(network); nm != nil {
		return fmt.Sprintf("Network %q is already connected", network)
	}

	nickPrefix := "fun"
	nickStrategy := config.NickStrategyPrefix
	count := 1
	ssl := false

	for _, arg := range args[2:] {
		if arg == "ssl" {
			ssl = true
		} else if strings.HasPrefix(arg, "nicks:") {
			strategy := strings.TrimPrefix(arg, "nicks:")
			switch config.NickStrategy(strategy) {
			case config.NickStrategyRandom:
				nickStrategy = config.NickStrategyRandom
			case config.NickStrategyWordlist:
				nickStrategy = config.NickStrategyWordlist
			case config.NickStrategyPrefix:
				nickStrategy = config.NickStrategyPrefix
			default:
				return fmt.Sprintf("Unknown nick strategy %q. Use: prefix, random, wordlist", strategy)
			}
		} else if n, err := strconv.Atoi(arg); err == nil {
			count = n
		} else {
			nickPrefix = arg
		}
	}

	// Parse and validate server address
	server, port, err := parseServerAddr(serverAddr)
	if err != nil {
		return fmt.Sprintf("Invalid server address %q: %v", serverAddr, err)
	}

	netCfg := config.Network{
		Servers:    []string{fmt.Sprintf("%s:%d", server, port)},
		SSL:        ssl,
		NickPrefix: nickPrefix,
		Nick: config.NickConfig{
			Strategy: nickStrategy,
			Prefix:   nickPrefix,
		},
		Channels:       []string{},
		FloodDelayMs:   1000,
		DefaultClients: count,
	}

	// Add to runtime config
	b.cfg.Networks[network] = netCfg

	nm, err := NewNetworkManager(network, netCfg, b.proxyPool, b.log)
	if err != nil {
		delete(b.cfg.Networks, network)
		return fmt.Sprintf("Failed to create network manager for %s: %v", network, err)
	}
	if err := nm.Start(context.Background()); err != nil {
		delete(b.cfg.Networks, network)
		return fmt.Sprintf("Failed to connect to %s: %v", network, err)
	}

	b.netMu.Lock()
	b.networks[network] = nm
	b.netMu.Unlock()

	return fmt.Sprintf("Connected to %s (%s:%d) with %d client(s). Note: runtime-only, will not survive restart.",
		network, server, port, count)
}

// handleDisconnect implements !disconnect <network>.
func (b *Bot) handleDisconnect(args []string) string {
	if len(args) == 0 {
		return "Usage: !disconnect <network>"
	}

	network := args[0]

	if network == b.cfg.HomeNetwork {
		return "Cannot disconnect the home network"
	}

	b.netMu.Lock()
	nm, ok := b.networks[network]
	if !ok {
		b.netMu.Unlock()
		return fmt.Sprintf("Network %q is not connected", network)
	}
	delete(b.networks, network)
	b.netMu.Unlock()

	nm.Stop()

	// Remove from runtime config
	delete(b.cfg.Networks, network)

	return fmt.Sprintf("Disconnected from %s", network)
}

// handleAddClients implements !addclients <network> <count>.
func (b *Bot) handleAddClients(args []string) string {
	if len(args) < 2 {
		return "Usage: !addclients <network> <count>"
	}

	network := b.cmdCtx.ResolveNetwork(args[0])
	count, err := strconv.Atoi(args[len(args)-1])
	if err != nil {
		return fmt.Sprintf("Invalid count: %s", args[len(args)-1])
	}

	nm := b.getNetwork(network)
	if nm == nil {
		return fmt.Sprintf("Network %q is not connected", network)
	}

	added, err := nm.AddClients(count)
	if err != nil {
		return fmt.Sprintf("Error adding clients: %v", err)
	}

	return fmt.Sprintf("Added %d client(s) to %s (requested %d)", added, network, count)
}

// handleRmClients implements !rmclients <network> <count>.
func (b *Bot) handleRmClients(args []string) string {
	if len(args) < 2 {
		return "Usage: !rmclients <network> <count>"
	}

	network := b.cmdCtx.ResolveNetwork(args[0])
	count, err := strconv.Atoi(args[len(args)-1])
	if err != nil {
		return fmt.Sprintf("Invalid count: %s", args[len(args)-1])
	}

	nm := b.getNetwork(network)
	if nm == nil {
		return fmt.Sprintf("Network %q is not connected", network)
	}

	removed := nm.RemoveClients(count)
	return fmt.Sprintf("Removed %d client(s) from %s", removed, network)
}
