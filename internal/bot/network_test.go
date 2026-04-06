package bot

import "testing"

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
