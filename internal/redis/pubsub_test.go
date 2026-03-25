package redis

import (
	"encoding/json"
	"testing"
)

func TestCommandMarshal(t *testing.T) {
	cmd := Command{
		ID:      "abc123",
		Type:    "join",
		Network: "efnet",
		Channel: "#test",
		Count:   3,
		Args:    []string{"#test"},
	}

	data, err := json.Marshal(cmd)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var decoded Command
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.ID != cmd.ID {
		t.Errorf("ID mismatch: %q vs %q", decoded.ID, cmd.ID)
	}
	if decoded.Type != cmd.Type {
		t.Errorf("Type mismatch: %q vs %q", decoded.Type, cmd.Type)
	}
	if decoded.Network != cmd.Network {
		t.Errorf("Network mismatch: %q vs %q", decoded.Network, cmd.Network)
	}
	if decoded.Channel != cmd.Channel {
		t.Errorf("Channel mismatch: %q vs %q", decoded.Channel, cmd.Channel)
	}
	if decoded.Count != cmd.Count {
		t.Errorf("Count mismatch: %d vs %d", decoded.Count, cmd.Count)
	}
}

func TestCommandAckMarshal(t *testing.T) {
	ack := CommandAck{
		CommandID: "abc123",
		Pod:       "pod-xyz",
		Network:   "efnet",
		Success:   true,
		Message:   "joined 3 clients to #test",
	}

	data, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var decoded CommandAck
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.CommandID != ack.CommandID {
		t.Errorf("CommandID mismatch")
	}
	if decoded.Success != ack.Success {
		t.Errorf("Success mismatch")
	}
	if decoded.Message != ack.Message {
		t.Errorf("Message mismatch")
	}
}

func TestPodStateMarshal(t *testing.T) {
	state := PodState{
		Pod:     "pod-abc",
		Network: "efnet",
		Clients: []ClientState{
			{
				ID:        "efnet-0",
				Nick:      "fun0",
				Connected: true,
				Channels:  []string{"#test"},
			},
			{
				ID:        "efnet-1",
				Nick:      "fun1",
				Connected: false,
				Channels:  nil,
				Proxy:     "socks5://1.2.3.4:1080",
			},
		},
	}

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var decoded PodState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.Pod != state.Pod {
		t.Errorf("Pod mismatch")
	}
	if len(decoded.Clients) != 2 {
		t.Fatalf("expected 2 clients, got %d", len(decoded.Clients))
	}
	if decoded.Clients[0].Nick != "fun0" {
		t.Errorf("Client 0 nick mismatch")
	}
	if decoded.Clients[1].Proxy != "socks5://1.2.3.4:1080" {
		t.Errorf("Client 1 proxy mismatch")
	}
}
