package auth

import "testing"

func TestIsAuthorized(t *testing.T) {
	checker := New("admin", "admin.host.com")

	tests := []struct {
		name     string
		nick     string
		hostname string
		want     bool
	}{
		{"exact match", "admin", "admin.host.com", true},
		{"wrong nick", "other", "admin.host.com", false},
		{"wrong hostname", "admin", "other.host.com", false},
		{"both wrong", "other", "other.host.com", false},
		{"empty nick", "", "admin.host.com", false},
		{"empty hostname", "admin", "", false},
		{"both empty", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checker.IsAuthorized(tt.nick, tt.hostname)
			if got != tt.want {
				t.Errorf("IsAuthorized(%q, %q) = %v, want %v",
					tt.nick, tt.hostname, got, tt.want)
			}
		})
	}
}
