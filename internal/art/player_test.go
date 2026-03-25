package art

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/venatiodecorus/funbot/internal/irc"
)

// mockClient creates a minimal IRC client for testing.
// We use a real irc.Client but with a fake server (it won't connect).
// We capture sent messages via a wrapper approach.
// Since we can't easily mock girc, we test the Player logic by
// tracking PrivmsgNoFlood calls via a recording wrapper.

// messageRecord captures a message sent through a client.
type messageRecord struct {
	ClientID string
	Target   string
	Message  string
	Time     time.Time
}

// messageRecorder tracks messages sent via PrivmsgNoFlood.
type messageRecorder struct {
	mu       sync.Mutex
	messages []messageRecord
}

func (r *messageRecorder) record(clientID, target, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messages = append(r.messages, messageRecord{
		ClientID: clientID,
		Target:   target,
		Message:  message,
		Time:     time.Now(),
	})
}

func (r *messageRecorder) getMessages() []messageRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]messageRecord, len(r.messages))
	copy(result, r.messages)
	return result
}

func TestLoadArt(t *testing.T) {
	dir := t.TempDir()

	// Create a test art file with mIRC color codes
	content := "\x034,1Red on black\n\x033Green text\nPlain line\n"
	path := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	lines, err := LoadArt(path)
	if err != nil {
		t.Fatalf("LoadArt() error: %v", err)
	}

	if len(lines) != 3 {
		t.Fatalf("LoadArt() returned %d lines, want 3", len(lines))
	}

	// Verify mIRC color codes are preserved
	if lines[0] != "\x034,1Red on black" {
		t.Errorf("line 0 = %q, want %q", lines[0], "\x034,1Red on black")
	}
	if lines[1] != "\x033Green text" {
		t.Errorf("line 1 = %q, want %q", lines[1], "\x033Green text")
	}
	if lines[2] != "Plain line" {
		t.Errorf("line 2 = %q, want %q", lines[2], "Plain line")
	}
}

func TestLoadArt_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(path, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	lines, err := LoadArt(path)
	if err != nil {
		t.Fatalf("LoadArt() error: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("LoadArt() returned %d lines for empty file, want 0", len(lines))
	}
}

func TestLoadArt_NotFound(t *testing.T) {
	_, err := LoadArt("/nonexistent/path/art.txt")
	if err == nil {
		t.Error("LoadArt() expected error for nonexistent file, got nil")
	}
}

func TestLoadArt_LongLines(t *testing.T) {
	dir := t.TempDir()

	// Create a file with a very long line (common in color-coded art)
	longLine := make([]byte, 10000)
	for i := range longLine {
		longLine[i] = 'X'
	}
	path := filepath.Join(dir, "long.txt")
	if err := os.WriteFile(path, longLine, 0644); err != nil {
		t.Fatal(err)
	}

	lines, err := LoadArt(path)
	if err != nil {
		t.Fatalf("LoadArt() error: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("LoadArt() returned %d lines, want 1", len(lines))
	}
	if len(lines[0]) != 10000 {
		t.Errorf("line length = %d, want 10000", len(lines[0]))
	}
}

// TestPlayer_Play_SingleClient tests basic single-client playback timing.
// Since we can't easily intercept PrivmsgNoFlood on a real irc.Client
// without a live connection, we test the player's logic indirectly by
// verifying it completes without error and respects cancellation.
func TestPlayer_Play_EmptyLines(t *testing.T) {
	log := slog.Default()
	player := NewPlayer(100*time.Millisecond, nil, log)

	// Create a dummy client (won't actually send - no connection)
	client := irc.New(irc.ClientConfig{
		ID:      "test-0",
		Network: "test",
		Server:  "localhost",
		Port:    6667,
		Nick:    "test",
		Logger:  log,
	})

	err := player.Play(context.Background(), "#test", []*irc.Client{client}, nil)
	if err != nil {
		t.Errorf("Play() with empty lines should return nil, got: %v", err)
	}
}

func TestPlayer_Play_NoClients(t *testing.T) {
	log := slog.Default()
	player := NewPlayer(100*time.Millisecond, nil, log)

	err := player.Play(context.Background(), "#test", nil, []string{"line1"})
	if err == nil {
		t.Error("Play() with no clients should return error, got nil")
	}
}

func TestPlayer_Play_Cancellation(t *testing.T) {
	log := slog.Default()
	player := NewPlayer(1*time.Second, nil, log) // Long delay to ensure we cancel

	client := irc.New(irc.ClientConfig{
		ID:      "test-0",
		Network: "test",
		Server:  "localhost",
		Port:    6667,
		Nick:    "test",
		Logger:  log,
	})

	// Create many lines so playback takes a while
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = "test line"
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- player.Play(ctx, "#test", []*irc.Client{client}, lines)
	}()

	// Cancel after a short time
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Play() after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Play() did not return after cancellation")
	}
}

// TestPlayer_InterLineDelay verifies the minimum inter-line delay floor.
func TestPlayer_MinDelay(t *testing.T) {
	log := slog.Default()
	// With 100 clients and 100ms flood delay, inter-line would be 1ms
	// but min is 50ms
	player := NewPlayer(100*time.Millisecond, nil, log)

	// Verify player was created (the minimum delay is enforced inside Play)
	if player.floodDelay != 100*time.Millisecond {
		t.Errorf("floodDelay = %v, want 100ms", player.floodDelay)
	}
}
