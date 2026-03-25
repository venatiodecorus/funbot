// Package irc provides a wrapper around the girc library for managing
// individual IRC client connections.
package irc

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"sync"

	"github.com/lrstanley/girc"
	"golang.org/x/net/proxy"
)

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
	log     *slog.Logger

	mu        sync.RWMutex
	connected bool
	nick      string
	channels  []string
	proxyAddr string // SOCKS5 proxy used for this connection (empty if direct)
	proxyUser string // SOCKS5 proxy username
	proxyPass string // SOCKS5 proxy password

	onPrivmsg MessageHandler
	onConnect ConnectHandler
}

// ClientConfig holds the configuration for creating a new IRC client.
type ClientConfig struct {
	ID        string
	Network   string
	Server    string
	Port      int
	SSL       bool
	Nick      string
	User      string
	Realname  string
	Logger    *slog.Logger
	ProxyAddr string // SOCKS5 proxy address (host:port), empty for direct
	ProxyUser string // SOCKS5 proxy username (optional)
	ProxyPass string // SOCKS5 proxy password (optional)
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

	c := &Client{
		id:        cfg.ID,
		network:   cfg.Network,
		gircCli:   gircCli,
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
		c.gircCli.Close()
		return ctx.Err()
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("connecting to IRC: %w", err)
		}
		return nil
	}
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

// Close cleanly disconnects from IRC.
func (c *Client) Close() {
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
func (c *Client) Privmsg(target, message string) {
	c.gircCli.Cmd.Message(target, message)
}

// SendRaw sends a raw IRC command.
func (c *Client) SendRaw(raw string) {
	c.gircCli.Cmd.SendRaw(raw)
}

// OnPrivmsg sets the handler for incoming PRIVMSG events.
func (c *Client) OnPrivmsg(handler MessageHandler) {
	c.onPrivmsg = handler
}

// OnConnect sets the handler for successful IRC connection.
func (c *Client) OnConnect(handler ConnectHandler) {
	c.onConnect = handler
}

// GircClient returns the underlying girc client. This should only be
// used within the irc package for advanced operations.
func (c *Client) GircClient() *girc.Client {
	return c.gircCli
}
