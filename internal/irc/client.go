// Package irc provides a wrapper around the girc library for managing
// individual IRC client connections.
package irc

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/lrstanley/girc"
	"golang.org/x/net/proxy"
)

// ProxyProto identifies the proxy protocol to use.
const (
	ProxyProtoSOCKS5 = "socks5"
	ProxyProtoHTTP   = "http"
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

	mu         sync.RWMutex
	connected  bool
	nick       string
	channels   []string
	proxyProto string // proxy protocol: "socks5" (default) or "http"
	proxyAddr  string // proxy address (host:port), empty if direct
	proxyUser  string // proxy username
	proxyPass  string // proxy password

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
	ProxyProto string        // Proxy protocol: "socks5" (default) or "http"
	ProxyAddr  string        // Proxy address (host:port), empty for direct
	ProxyUser  string        // Proxy username (optional)
	ProxyPass  string        // Proxy password (optional)
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
		id:         cfg.ID,
		network:    cfg.Network,
		gircCli:    gircCli,
		cfg:        cfg,
		flood:      flood,
		log:        log,
		nick:       cfg.Nick,
		proxyProto: cfg.ProxyProto,
		proxyAddr:  cfg.ProxyAddr,
		proxyUser:  cfg.ProxyUser,
		proxyPass:  cfg.ProxyPass,
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
// connection will be routed through the proxy (SOCKS5 or HTTP CONNECT).
func (c *Client) Connect(ctx context.Context) error {
	c.log.Info("connecting to IRC", "proxy_proto", c.proxyProto, "proxy", c.proxyAddr)

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

// createProxyDialer creates a proxy dialer based on the configured protocol.
// Supports SOCKS5 (default) and HTTP CONNECT proxies.
func (c *Client) createProxyDialer() (proxy.Dialer, error) {
	switch c.proxyProto {
	case ProxyProtoHTTP:
		return &httpConnectDialer{
			proxyAddr: c.proxyAddr,
			user:      c.proxyUser,
			pass:      c.proxyPass,
			timeout:   30 * time.Second,
		}, nil
	default:
		// SOCKS5 (default)
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
}

// httpConnectDialer implements proxy.Dialer using the HTTP CONNECT method.
// This tunnels a raw TCP connection through an HTTP proxy, allowing
// non-HTTP protocols (like IRC) to pass through.
type httpConnectDialer struct {
	proxyAddr string
	user      string
	pass      string
	timeout   time.Duration
}

// Dial connects to the proxy and issues an HTTP CONNECT request to tunnel
// to the target address. The returned net.Conn is the raw tunneled connection.
func (d *httpConnectDialer) Dial(network, addr string) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", d.proxyAddr, d.timeout)
	if err != nil {
		return nil, fmt.Errorf("connecting to HTTP proxy %s: %w", d.proxyAddr, err)
	}

	// Build the CONNECT request
	req, err := http.NewRequest(http.MethodConnect, "http://"+addr, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("building CONNECT request: %w", err)
	}
	req.Host = addr

	if d.user != "" {
		creds := base64.StdEncoding.EncodeToString([]byte(d.user + ":" + d.pass))
		req.Header.Set("Proxy-Authorization", "Basic "+creds)
	}

	// Set a deadline for the CONNECT handshake
	if err := conn.SetDeadline(time.Now().Add(d.timeout)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("setting deadline: %w", err)
	}

	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("sending CONNECT to %s: %w", d.proxyAddr, err)
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("reading CONNECT response from %s: %w", d.proxyAddr, err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("HTTP CONNECT to %s via %s failed: %s", addr, d.proxyAddr, resp.Status)
	}

	// Clear the deadline — the tunnel is now established
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("clearing deadline: %w", err)
	}

	return conn, nil
}

// ProxyAddr returns the proxy address used for this connection,
// or an empty string if using a direct connection.
func (c *Client) ProxyAddr() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.proxyAddr
}

// SetProxy updates the proxy address, protocol, and optional credentials
// used for future connection attempts. This should be called between retry
// cycles when rotating to a new proxy. Proto should be "socks5" or "http".
func (c *Client) SetProxy(proto, addr, user, pass string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.proxyProto = proto
	c.proxyAddr = addr
	c.proxyUser = user
	c.proxyPass = pass
	c.cfg.ProxyProto = proto
	c.cfg.ProxyAddr = addr
	c.cfg.ProxyUser = user
	c.cfg.ProxyPass = pass
	c.log.Info("proxy updated", "proto", proto, "proxy", addr)
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
