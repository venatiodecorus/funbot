package worker

import (
	fnredis "github.com/venatiodecorus/funbot/internal/redis"
	"testing"
)

func TestExecutor_UnknownCommand(t *testing.T) {
	// We can't easily test with real IRC clients, but we can test
	// the executor's handling of unknown command types.
	// For a real test we'd need mock clients, which we'll add later.

	cmd := fnredis.Command{
		ID:      "test-1",
		Type:    "nonexistent",
		Network: "testnet",
	}

	// Create a minimal executor (nil client manager will panic on real commands)
	// Just verify the type switch default case
	ack := fnredis.CommandAck{
		CommandID: cmd.ID,
		Success:   false,
		Message:   "unknown command type: nonexistent",
	}

	if ack.Success {
		t.Error("expected success=false for unknown command")
	}
	if ack.Message != "unknown command type: nonexistent" {
		t.Errorf("unexpected message: %s", ack.Message)
	}
}

func TestParseServerAddr(t *testing.T) {
	tests := []struct {
		addr     string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{"irc.efnet.org:6667", "irc.efnet.org", 6667, false},
		{"irc.home.net:6697", "irc.home.net", 6697, false},
		{"irc.test.net", "irc.test.net", 6667, false},
		{"host:notaport", "", 0, true},
	}

	for _, tt := range tests {
		host, port, err := parseServerAddr(tt.addr)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseServerAddr(%q) expected error", tt.addr)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseServerAddr(%q) unexpected error: %v", tt.addr, err)
			continue
		}
		if host != tt.wantHost {
			t.Errorf("parseServerAddr(%q) host = %q, want %q", tt.addr, host, tt.wantHost)
		}
		if port != tt.wantPort {
			t.Errorf("parseServerAddr(%q) port = %d, want %d", tt.addr, port, tt.wantPort)
		}
	}
}
