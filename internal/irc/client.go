// Package irc provides a wrapper around the girc library for managing
// individual IRC client connections.
package irc

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/lrstanley/girc"
	"golang.org/x/net/proxy"
)

// Reconnection defaults.
const (
	DefaultReconnectBaseDelay = 2 * time.Second
	DefaultReconnectMaxDelay  = 5 * time.Minute
	DefaultReconnectMaxRetry  = 0 // 0 means retry forever
)

// DisconnectHandler is called when the client disconnects from IRC.
// The error parameter is nil if the disconnect was intentional.
type DisconnectHandler func(err error)

// MessageHandler is called when the client receives a PRIVMSG.
// Parameters: nick, hostname, target, message.
type MessageHandler func(nick, hostname, target, message string)

// ConnectHandler is called when the client successfully connects to IRC.
type ConnectHandler func()

// Client wraps a single girc IRC connection.
type Client struct {
	id      string
	network string
	gircCli *girc.Client
	cfg     ClientConfig // saved for reconnection
	flood   *FloodControl
	log     *slog.Logger

	mu        sync.RWMutex
	connected bool
	nick      string
	channels  []string
	proxyAddr string // SOCKS5 proxy used for this connection (empty if direct)
	proxyUser string // SOCKS5 proxy username
	proxyPass string // SOCKS5 proxy password

	onPrivmsg    MessageHandler
	onConnect    ConnectHandler
	onDisconnect DisconnectHandler
}

// ClientConfig holds the configuration for creating a new IRC client.
type ClientConfig struct {
	ID         string
	Network    string
	Server     string
	Port       int
	SSL        bool
	Nick       string
	User       string
	Realname   string
	Logger     *slog.Logger
	FloodDelay time.Duration // Minimum delay between messages (0 = no limit)
	ProxyAddr  string        // SOCKS5 proxy address (host:port), empty for direct
	ProxyUser  string        // SOCKS5 proxy username (optional)
	ProxyPass  string        // SOCKS5 proxy password (optional)
}

// New creates a new IRC client with the given configuration.
func New(cfg ClientConfig) *Client {
	gircConfig := girc.Config{
		Server: cfg.Server,
		Port:   cfg.Port,
		Nick:   cfg.Nick,
		User:   cfg.User,
		Name:   cfg.Realname,
		SSL:    cfg.SSL,
	}

	if cfg.SSL {
		gircConfig.TLSConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	gircCli := girc.New(gircConfig)

	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("client_id", cfg.ID, "network", cfg.Network)

	var flood *FloodControl
	if cfg.FloodDelay > 0 {
		flood = NewFloodControl(cfg.FloodDelay)
	}

	c := &Client{
		id:        cfg.ID,
		network:   cfg.Network,
		gircCli:   gircCli,
		cfg:       cfg,
		flood:     flood,
		log:       log,
		nick:      cfg.Nick,
		proxyAddr: cfg.ProxyAddr,
		proxyUser: cfg.ProxyUser,
		proxyPass: cfg.ProxyPass,
	}

	// Register handlers
	gircCli.Handlers.AddBg(girc.CONNECTED, func(gc *girc.Client, e girc.Event) {
		c.mu.Lock()
		c.connected = true
		c.nick = gc.GetNick()
		c.mu.Unlock()
		c.log.Info("connected to IRC", "nick", gc.GetNick())

		if c.onConnect != nil {
			c.onConnect()
		}
	})

	gircCli.Handlers.AddBg(girc.DISCONNECTED, func(gc *girc.Client, e girc.Event) {
		c.mu.Lock()
		c.connected = false
		c.mu.Unlock()
		c.log.Info("disconnected from IRC")

		if c.onDisconnect != nil {
			c.onDisconnect(nil)
		}
	})

	gircCli.Handlers.AddBg(girc.PRIVMSG, func(gc *girc.Client, e girc.Event) {
		if c.onPrivmsg != nil {
			hostname := ""
			if e.Source != nil {
				hostname = e.Source.Host
			}
			c.onPrivmsg(e.Source.Name, hostname, e.Params[0], e.Last())
		}
	})

	gircCli.Handlers.AddBg(girc.NICK, func(gc *girc.Client, e girc.Event) {
		if e.Source.Name == c.Nick() || e.Last() == gc.GetNick() {
			c.mu.Lock()
			c.nick = gc.GetNick()
			c.mu.Unlock()
			c.log.Info("nick changed", "new_nick", gc.GetNick())
		}
	})

	return c
}

// Connect establishes the IRC connection. It blocks until the connection
// is closed or the context is cancelled. If a proxy is configured, the
// connection will be routed through the SOCKS5 proxy.
func (c *Client) Connect(ctx context.Context) error {
	c.log.Info("connecting to IRC", "proxy", c.proxyAddr)

	errCh := make(chan error, 1)
	go func() {
		if c.proxyAddr != "" {
			dialer, err := c.createProxyDialer()
			if err != nil {
				errCh <- fmt.Errorf("creating proxy dialer: %w", err)
				return
			}
			errCh <- c.gircCli.DialerConnect(dialer)
		} else {
			errCh <- c.gircCli.Connect()
		}
	}()

	select {
	case <-ctx.Done():
		c.Quit("shutting down")
		return ctx.Err()
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("connecting to IRC: %w", err)
		}
		return nil
	}
}

// ConnectWithRetry connects to IRC with automatic reconnection on disconnect.
// It uses exponential backoff between retry attempts. It blocks until the
// context is cancelled or maxRetries is exceeded (0 = retry forever).
func (c *Client) ConnectWithRetry(ctx context.Context, maxRetries int) error {
	baseDelay := DefaultReconnectBaseDelay
	maxDelay := DefaultReconnectMaxDelay
	attempt := 0

	for {
		err := c.Connect(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		attempt++
		if err != nil {
			c.log.Warn("connection failed",
				"attempt", attempt,
				"error", err,
			)
		} else {
			// Connection was established but then dropped
			c.log.Warn("connection lost, will reconnect",
				"attempt", attempt,
			)
		}

		if maxRetries > 0 && attempt >= maxRetries {
			return fmt.Errorf("max reconnection attempts (%d) reached: %w", maxRetries, err)
		}

		// Exponential backoff with jitter cap
		delay := time.Duration(float64(baseDelay) * math.Pow(1.5, float64(attempt-1)))
		if delay > maxDelay {
			delay = maxDelay
		}

		c.log.Info("reconnecting",
			"delay", delay,
			"attempt", attempt,
		)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
			// Reset the girc client for a fresh connection
			c.resetGircClient()
		}
	}
}

// resetGircClient creates a fresh girc client for reconnection, preserving
// all existing handlers and configuration.
func (c *Client) resetGircClient() {
	c.log.Debug("resetting IRC client for reconnection")
	gircConfig := girc.Config{
		Server: c.cfg.Server,
		Port:   c.cfg.Port,
		Nick:   c.cfg.Nick,
		User:   c.cfg.User,
		Name:   c.cfg.Realname,
		SSL:    c.cfg.SSL,
	}

	if c.cfg.SSL {
		gircConfig.TLSConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	gircCli := girc.New(gircConfig)

	// Re-register core handlers
	gircCli.Handlers.AddBg(girc.CONNECTED, func(gc *girc.Client, e girc.Event) {
		c.mu.Lock()
		c.connected = true
		c.nick = gc.GetNick()
		c.mu.Unlock()
		c.log.Info("connected to IRC", "nick", gc.GetNick())

		if c.onConnect != nil {
			c.onConnect()
		}
	})

	gircCli.Handlers.AddBg(girc.DISCONNECTED, func(gc *girc.Client, e girc.Event) {
		c.mu.Lock()
		c.connected = false
		c.mu.Unlock()
		c.log.Info("disconnected from IRC")

		if c.onDisconnect != nil {
			c.onDisconnect(nil)
		}
	})

	gircCli.Handlers.AddBg(girc.PRIVMSG, func(gc *girc.Client, e girc.Event) {
		if c.onPrivmsg != nil {
			hostname := ""
			if e.Source != nil {
				hostname = e.Source.Host
			}
			c.onPrivmsg(e.Source.Name, hostname, e.Params[0], e.Last())
		}
	})

	gircCli.Handlers.AddBg(girc.NICK, func(gc *girc.Client, e girc.Event) {
		if e.Source.Name == c.Nick() || e.Last() == gc.GetNick() {
			c.mu.Lock()
			c.nick = gc.GetNick()
			c.mu.Unlock()
			c.log.Info("nick changed", "new_nick", gc.GetNick())
		}
	})

	c.mu.Lock()
	c.gircCli = gircCli
	c.connected = false
	c.mu.Unlock()
}

// createProxyDialer creates a SOCKS5 proxy dialer from the client config.
func (c *Client) createProxyDialer() (proxy.Dialer, error) {
	var auth *proxy.Auth
	if c.proxyUser != "" {
		auth = &proxy.Auth{
			User:     c.proxyUser,
			Password: c.proxyPass,
		}
	}
	dialer, err := proxy.SOCKS5("tcp", c.proxyAddr, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("creating SOCKS5 dialer for %s: %w", c.proxyAddr, err)
	}
	return dialer, nil
}

// ProxyAddr returns the SOCKS5 proxy address used for this connection,
// or an empty string if using a direct connection.
func (c *Client) ProxyAddr() string {
	return c.proxyAddr
}

// Quit sends an IRC QUIT command with the given reason, then closes the
// connection. This is the preferred way to disconnect during graceful
// shutdown, as it notifies the IRC server and other users.
func (c *Client) Quit(reason string) {
	if c.IsConnected() {
		c.log.Info("sending QUIT", "reason", reason)
		_ = c.gircCli.Cmd.SendRaw("QUIT :" + reason)
		// Give the server a moment to process the QUIT before closing
		time.Sleep(500 * time.Millisecond)
	}
	c.gircCli.Close()
}

// Close disconnects from IRC. For graceful shutdown, prefer Quit() which
// sends a QUIT message first.
func (c *Client) Close() {
	c.log.Debug("closing IRC connection")
	c.gircCli.Close()
}

// ID returns the client's identifier.
func (c *Client) ID() string {
	return c.id
}

// Network returns the network name this client is connected to.
func (c *Client) Network() string {
	return c.network
}

// Nick returns the client's current nick.
func (c *Client) Nick() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.nick
}

// IsConnected returns whether the client is currently connected.
func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

// Channels returns the list of channels the client has joined.
func (c *Client) Channels() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]string, len(c.channels))
	copy(result, c.channels)
	return result
}

// SetNick attempts to change the client's nick.
func (c *Client) SetNick(nick string) {
	c.gircCli.Cmd.Nick(nick)
}

// Join joins an IRC channel.
func (c *Client) Join(channel string) {
	c.gircCli.Cmd.Join(channel)
	c.mu.Lock()
	c.channels = append(c.channels, channel)
	c.mu.Unlock()
	c.log.Info("joined channel", "channel", channel)
}

// Part leaves an IRC channel.
func (c *Client) Part(channel string) {
	c.gircCli.Cmd.Part(channel)
	c.mu.Lock()
	for i, ch := range c.channels {
		if ch == channel {
			c.channels = append(c.channels[:i], c.channels[i+1:]...)
			break
		}
	}
	c.mu.Unlock()
	c.log.Info("parted channel", "channel", channel)
}

// Privmsg sends a PRIVMSG to a target (channel or user).
// If flood control is enabled, it waits until it's safe to send.
func (c *Client) Privmsg(target, message string) {
	if c.flood != nil {
		c.flood.Wait()
	}
	c.gircCli.Cmd.Message(target, message)
}

// PrivmsgNoFlood sends a PRIVMSG without applying flood control.
// This is used by the art player which manages its own timing.
func (c *Client) PrivmsgNoFlood(target, message string) {
	c.gircCli.Cmd.Message(target, message)
}

// SendRaw sends a raw IRC command.
// If flood control is enabled, it waits until it's safe to send.
func (c *Client) SendRaw(raw string) {
	if c.flood != nil {
		c.flood.Wait()
	}
	c.log.Debug("sending raw command", "raw", raw)
	_ = c.gircCli.Cmd.SendRaw(raw)
}

// FloodDelay returns the configured flood delay, or 0 if not set.
func (c *Client) FloodDelay() time.Duration {
	if c.flood != nil {
		return c.flood.Delay()
	}
	return 0
}

// OnPrivmsg sets the handler for incoming PRIVMSG events.
func (c *Client) OnPrivmsg(handler MessageHandler) {
	c.onPrivmsg = handler
}

// OnConnect sets the handler for successful IRC connection.
func (c *Client) OnConnect(handler ConnectHandler) {
	c.onConnect = handler
}

// OnDisconnect sets the handler for IRC disconnection events.
func (c *Client) OnDisconnect(handler DisconnectHandler) {
	c.onDisconnect = handler
}

// GircClient returns the underlying girc client. This should only be
// used within the irc package for advanced operations.
func (c *Client) GircClient() *girc.Client {
	return c.gircCli
}
