//go:build integration

// Package integration contains integration tests for Funbot using a real
// IRC server (ergo) and Redis.
//
// These tests require docker compose services running:
//
//	docker compose -f test/integration/docker-compose.yaml up -d
//
// Run tests with:
//
//	go test -tags integration -v ./test/integration/
package integration

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/venatiodecorus/funbot/internal/config"
	"github.com/venatiodecorus/funbot/internal/irc"
	fnredis "github.com/venatiodecorus/funbot/internal/redis"
	"github.com/venatiodecorus/funbot/internal/worker"
)

const (
	testIRCServer  = "localhost"
	testIRCPort    = 6667
	testRedisAddr  = "localhost:6379"
	testNetwork    = "testnet"
	connectTimeout = 10 * time.Second
)

// testConfig returns a config suitable for integration testing.
func testConfig() *config.Config {
	return &config.Config{
		Role: "worker",
		Controller: config.ControllerConfig{
			HomeNetwork: testNetwork,
			Auth: config.AuthConfig{
				Nick:     "admin",
				Hostname: "localhost",
			},
			CommandPrefix: "!",
		},
		Redis: config.RedisConfig{
			Address: testRedisAddr,
		},
		Networks: map[string]config.Network{
			testNetwork: {
				Servers:         []string{fmt.Sprintf("%s:%d", testIRCServer, testIRCPort)},
				SSL:             false,
				NickPrefix:      "funtest",
				MaxClientsPerIP: 3,
				Channels:        []string{"#test"},
				FloodDelayMs:    200,
			},
		},
		Logging: config.LoggingConfig{
			Level:  "debug",
			Format: "text",
		},
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
}

// skipIfNoServices skips the test if required services aren't running.
func skipIfNoServices(t *testing.T) {
	t.Helper()

	log := testLogger()

	// Check Redis
	redisCfg := config.RedisConfig{Address: testRedisAddr}
	rc, err := fnredis.New(redisCfg, log)
	if err != nil {
		t.Skipf("Redis not available: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rc.Ping(ctx); err != nil {
		rc.Close()
		t.Skipf("Redis not reachable: %v", err)
	}
	rc.Close()

	// Check IRC server with a quick connection attempt
	client := irc.New(irc.ClientConfig{
		ID:      "probe",
		Network: "probe",
		Server:  testIRCServer,
		Port:    testIRCPort,
		Nick:    "probe",
		User:    "probe",
		Logger:  log,
	})

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer probeCancel()

	connected := make(chan struct{}, 1)
	client.OnConnect(func() {
		connected <- struct{}{}
	})

	go func() {
		_ = client.Connect(probeCtx)
	}()

	select {
	case <-connected:
		client.Quit("probe done")
	case <-probeCtx.Done():
		t.Skip("IRC server not reachable")
	}
}

// TestSingleClientConnect verifies a single IRC client can connect,
// get a nick, and join a channel.
func TestSingleClientConnect(t *testing.T) {
	skipIfNoServices(t)
	log := testLogger()

	client := irc.New(irc.ClientConfig{
		ID:       "inttest-0",
		Network:  testNetwork,
		Server:   testIRCServer,
		Port:     testIRCPort,
		Nick:     "funtest0",
		User:     "funbot",
		Realname: "Integration Test",
		Logger:   log,
	})

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	connected := make(chan struct{}, 1)
	client.OnConnect(func() {
		client.Join("#inttest")
		connected <- struct{}{}
	})

	go func() {
		if err := client.Connect(ctx); err != nil && ctx.Err() == nil {
			t.Errorf("Connect() error: %v", err)
		}
	}()

	select {
	case <-connected:
		// Verify state
		if !client.IsConnected() {
			t.Error("expected IsConnected() = true")
		}
		if client.Nick() == "" {
			t.Error("expected non-empty nick")
		}
		t.Logf("connected as %s", client.Nick())

		// Verify channel join
		time.Sleep(200 * time.Millisecond) // give time for server response
		channels := client.Channels()
		if len(channels) == 0 {
			t.Error("expected at least one channel")
		}

		client.Quit("test done")
	case <-ctx.Done():
		t.Fatal("timeout waiting for connection")
	}
}

// TestMultipleClientConnect verifies multiple clients can connect simultaneously.
func TestMultipleClientConnect(t *testing.T) {
	skipIfNoServices(t)
	log := testLogger()

	const numClients = 3
	clients := make([]*irc.Client, numClients)
	connected := make(chan string, numClients)

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	for i := 0; i < numClients; i++ {
		clients[i] = irc.New(irc.ClientConfig{
			ID:       fmt.Sprintf("inttest-%d", i),
			Network:  testNetwork,
			Server:   testIRCServer,
			Port:     testIRCPort,
			Nick:     fmt.Sprintf("funmc%d", i),
			User:     "funbot",
			Realname: "Integration Test",
			Logger:   log,
		})

		idx := i
		clients[i].OnConnect(func() {
			connected <- fmt.Sprintf("inttest-%d", idx)
		})

		go func(c *irc.Client) {
			if err := c.Connect(ctx); err != nil && ctx.Err() == nil {
				t.Errorf("Connect() error: %v", err)
			}
		}(clients[i])
	}

	// Wait for all to connect
	connectedCount := 0
	for connectedCount < numClients {
		select {
		case id := <-connected:
			connectedCount++
			t.Logf("client %s connected (%d/%d)", id, connectedCount, numClients)
		case <-ctx.Done():
			t.Fatalf("timeout: only %d/%d clients connected", connectedCount, numClients)
		}
	}

	// Clean up
	for _, c := range clients {
		c.Quit("test done")
	}
}

// TestClientReconnect verifies that ConnectWithRetry reconnects after disconnect.
func TestClientReconnect(t *testing.T) {
	skipIfNoServices(t)
	log := testLogger()

	client := irc.New(irc.ClientConfig{
		ID:       "inttest-reconn",
		Network:  testNetwork,
		Server:   testIRCServer,
		Port:     testIRCPort,
		Nick:     "funreconn",
		User:     "funbot",
		Realname: "Integration Test",
		Logger:   log,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	connectCount := 0
	var mu sync.Mutex
	connectCh := make(chan struct{}, 10)

	client.OnConnect(func() {
		mu.Lock()
		connectCount++
		count := connectCount
		mu.Unlock()
		t.Logf("connected (count=%d)", count)
		connectCh <- struct{}{}
	})

	// Start ConnectWithRetry in background
	go func() {
		_ = client.ConnectWithRetry(ctx, 0)
	}()

	// Wait for first connection
	select {
	case <-connectCh:
		t.Log("first connection established")
	case <-ctx.Done():
		t.Fatal("timeout waiting for first connection")
	}

	// Force disconnect
	client.Close()
	t.Log("forced disconnect")

	// Wait for reconnection
	select {
	case <-connectCh:
		t.Log("reconnected successfully")
	case <-ctx.Done():
		t.Fatal("timeout waiting for reconnection")
	}

	mu.Lock()
	finalCount := connectCount
	mu.Unlock()

	if finalCount < 2 {
		t.Errorf("expected at least 2 connections, got %d", finalCount)
	}

	cancel()
}

// TestWorkerCommandFlow verifies commands flow from Redis to the worker executor.
func TestWorkerCommandFlow(t *testing.T) {
	skipIfNoServices(t)
	log := testLogger()
	cfg := testConfig()

	// Create Redis client
	rc, err := fnredis.New(cfg.Redis, log)
	if err != nil {
		t.Fatalf("failed to create Redis client: %v", err)
	}
	defer rc.Close()

	// Start worker in background
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w, err := worker.New(cfg, testNetwork, rc, log)
	if err != nil {
		t.Fatalf("failed to create worker: %v", err)
	}

	workerDone := make(chan error, 1)
	go func() {
		workerDone <- w.Run(ctx)
	}()

	// Wait for worker to be ready (at least one client connected)
	time.Sleep(5 * time.Second)

	// Send a join command via Redis
	joinCmd := fnredis.Command{
		Type:    "join",
		Network: testNetwork,
		Channel: "#cmdtest",
		Count:   1,
	}

	if err := rc.PublishCommand(ctx, joinCmd); err != nil {
		t.Fatalf("failed to publish command: %v", err)
	}

	// Subscribe to acks and wait for response
	ackCh, err := rc.SubscribeAcks(ctx)
	if err != nil {
		t.Fatalf("failed to subscribe to acks: %v", err)
	}

	select {
	case ack := <-ackCh:
		t.Logf("received ack: success=%v, message=%s", ack.Success, ack.Message)
		if !ack.Success {
			t.Errorf("expected success=true, got false: %s", ack.Message)
		}
	case <-time.After(10 * time.Second):
		t.Error("timeout waiting for ack")
	}

	cancel()

	// Wait for worker to exit
	select {
	case err := <-workerDone:
		if err != nil {
			t.Logf("worker exited with: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("timeout waiting for worker to exit")
	}
}

// TestStatusReporting verifies workers publish state to Redis.
func TestStatusReporting(t *testing.T) {
	skipIfNoServices(t)
	log := testLogger()
	cfg := testConfig()

	rc, err := fnredis.New(cfg.Redis, log)
	if err != nil {
		t.Fatalf("failed to create Redis client: %v", err)
	}
	defer rc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w, err := worker.New(cfg, testNetwork, rc, log)
	if err != nil {
		t.Fatalf("failed to create worker: %v", err)
	}

	go func() {
		_ = w.Run(ctx)
	}()

	// Wait for worker to connect and publish state
	time.Sleep(15 * time.Second)

	// Check Redis for state
	states, err := rc.GetNetworkStates(ctx, testNetwork)
	if err != nil {
		t.Fatalf("failed to get network states: %v", err)
	}

	if len(states) == 0 {
		t.Fatal("expected at least one pod state in Redis")
	}

	state := states[0]
	t.Logf("pod state: pod=%s, network=%s, clients=%d", state.Pod, state.Network, len(state.Clients))

	if state.Network != testNetwork {
		t.Errorf("expected network=%s, got %s", testNetwork, state.Network)
	}
	if len(state.Clients) == 0 {
		t.Error("expected at least one client in state")
	}

	// Check that at least one client is connected
	connectedCount := 0
	for _, cs := range state.Clients {
		if cs.Connected {
			connectedCount++
		}
	}
	if connectedCount == 0 {
		t.Error("expected at least one connected client")
	}

	t.Logf("found %d connected clients out of %d total", connectedCount, len(state.Clients))

	cancel()
}
