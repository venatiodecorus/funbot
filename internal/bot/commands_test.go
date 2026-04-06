package bot

import (
	"log/slog"
	"strings"
	"testing"
)

func newTestDispatcher() *CommandDispatcher {
	ctx := NewCommandContext()
	log := slog.Default()
	return NewCommandDispatcher("!", ctx, log)
}

func TestDispatch_NotACommand(t *testing.T) {
	d := newTestDispatcher()
	if got := d.Dispatch("hello world"); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestDispatch_UnknownCommand(t *testing.T) {
	d := newTestDispatcher()
	got := d.Dispatch("!foobar")
	if !strings.Contains(got, "Unknown command") {
		t.Errorf("expected 'Unknown command', got %q", got)
	}
}

func TestDispatch_Context(t *testing.T) {
	d := newTestDispatcher()

	// Show empty context
	got := d.Dispatch("!context")
	if !strings.Contains(got, "no context set") {
		t.Errorf("expected 'no context set', got %q", got)
	}

	// Set network only
	got = d.Dispatch("!context efnet")
	if !strings.Contains(got, "efnet") {
		t.Errorf("expected 'efnet' in response, got %q", got)
	}

	// Set network + channel
	got = d.Dispatch("!context undernet #test")
	if !strings.Contains(got, "undernet") || !strings.Contains(got, "#test") {
		t.Errorf("expected 'undernet' and '#test', got %q", got)
	}

	// Clear
	got = d.Dispatch("!context clear")
	if !strings.Contains(got, "cleared") {
		t.Errorf("expected 'cleared', got %q", got)
	}
}

func TestDispatch_CustomHandler(t *testing.T) {
	d := newTestDispatcher()
	d.Register("ping", func(args []string) string {
		return "pong"
	})

	got := d.Dispatch("!ping")
	if got != "pong" {
		t.Errorf("expected 'pong', got %q", got)
	}
}

func TestDispatch_Help(t *testing.T) {
	d := newTestDispatcher()
	got := d.Dispatch("!help")
	if !strings.Contains(got, "Available commands") {
		t.Errorf("expected 'Available commands', got %q", got)
	}
	if !strings.Contains(got, "!context") {
		t.Errorf("expected '!context' in help, got %q", got)
	}
}

func TestIsChannel(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"#test", true},
		{"&local", true},
		{"user", false},
		{"", false},
	}

	for _, tt := range tests {
		if got := IsChannel(tt.input); got != tt.want {
			t.Errorf("IsChannel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}
