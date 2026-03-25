package redis

import (
	"testing"
)

func TestStateKey(t *testing.T) {
	key := StateKey("efnet", "pod-abc123")
	expected := "funbot:state:efnet:pod-abc123"
	if key != expected {
		t.Errorf("expected %q, got %q", expected, key)
	}
}

func TestCmdChannel(t *testing.T) {
	ch := CmdChannel("efnet")
	expected := "funbot:cmd:efnet"
	if ch != expected {
		t.Errorf("expected %q, got %q", expected, ch)
	}
}

func TestParseNetworkFromKey(t *testing.T) {
	tests := []struct {
		key  string
		want string
	}{
		{"funbot:state:efnet:pod-abc", "efnet"},
		{"funbot:state:undernet:pod-xyz", "undernet"},
		{"funbot:state:", ""},
	}

	for _, tt := range tests {
		got := parseNetworkFromKey(tt.key)
		if got != tt.want {
			t.Errorf("parseNetworkFromKey(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}
