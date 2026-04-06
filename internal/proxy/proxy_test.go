package proxy

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestParseProxy(t *testing.T) {
	tests := []struct {
		input    string
		wantHost string
		wantPort string
		wantUser string
		wantErr  bool
	}{
		{"socks5://1.2.3.4:1080", "1.2.3.4", "1080", "", false},
		{"socks5://user:pass@1.2.3.4:1080", "1.2.3.4", "1080", "user", false},
		{"1.2.3.4:1080", "1.2.3.4", "1080", "", false},
		{"socks5://host.example.com:9050", "host.example.com", "9050", "", false},
		{"socks5://1.2.3.4", "1.2.3.4", "1080", "", false}, // default port
		{"http://1.2.3.4:8080", "", "", "", true},          // wrong scheme
	}

	for _, tt := range tests {
		px, err := parseProxy(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseProxy(%q) expected error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseProxy(%q) unexpected error: %v", tt.input, err)
			continue
		}
		if px.Host != tt.wantHost {
			t.Errorf("parseProxy(%q) host = %q, want %q", tt.input, px.Host, tt.wantHost)
		}
		if px.Port != tt.wantPort {
			t.Errorf("parseProxy(%q) port = %q, want %q", tt.input, px.Port, tt.wantPort)
		}
		if px.Username != tt.wantUser {
			t.Errorf("parseProxy(%q) user = %q, want %q", tt.input, px.Username, tt.wantUser)
		}
		if !px.Healthy {
			t.Errorf("parseProxy(%q) should be healthy by default", tt.input)
		}
		if px.Networks == nil {
			t.Errorf("parseProxy(%q) Networks map should be initialized", tt.input)
		}
	}
}

func TestPoolLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxies.txt")

	content := `# Comment line
socks5://1.2.3.4:1080
socks5://user:pass@5.6.7.8:9050

socks5://invalid-no-scheme
socks5://9.10.11.12:1080
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	pool := NewPool(slog.Default())
	if err := pool.LoadFromFile(path); err != nil {
		t.Fatalf("LoadFromFile error: %v", err)
	}

	// Should have loaded 4 valid proxies (the bare one gets socks5:// prepended)
	if pool.Count() != 4 {
		t.Errorf("expected 4 proxies, got %d", pool.Count())
	}
	if pool.HealthyCount() != 4 {
		t.Errorf("expected 4 healthy, got %d", pool.HealthyCount())
	}
}

func TestPoolLoadFromList(t *testing.T) {
	pool := NewPool(slog.Default())
	list := []string{
		"socks5://1.2.3.4:1080",
		"socks5://5.6.7.8:1080",
	}
	if err := pool.LoadFromList(list); err != nil {
		t.Fatalf("LoadFromList error: %v", err)
	}

	if pool.Count() != 2 {
		t.Errorf("expected 2 proxies, got %d", pool.Count())
	}
}

func TestPoolAcquireForNetwork(t *testing.T) {
	pool := NewPool(slog.Default())
	_ = pool.LoadFromList([]string{
		"socks5://1.2.3.4:1080",
		"socks5://5.6.7.8:1080",
	})

	p1 := pool.AcquireForNetwork("efnet")
	if p1 == nil {
		t.Fatal("expected a proxy")
	}
	if p1.UseCount != 1 {
		t.Errorf("expected use count 1, got %d", p1.UseCount)
	}
	if !p1.Networks["efnet"] {
		t.Error("expected proxy to be marked for efnet")
	}

	p2 := pool.AcquireForNetwork("efnet")
	if p2 == nil {
		t.Fatal("expected a proxy")
	}
	if p1.Host == p2.Host && p1.Port == p2.Port {
		t.Error("expected different proxy on second acquire for same network")
	}

	p3 := pool.AcquireForNetwork("undernet")
	if p3 == nil {
		t.Fatal("expected a proxy for different network")
	}
	if !p3.Networks["undernet"] {
		t.Error("expected proxy to be marked for undernet")
	}

	p4 := pool.AcquireForNetwork("efnet")
	if p4 != nil {
		t.Error("expected nil when all proxies are used for network")
	}
}

func TestPoolReleaseFromNetwork(t *testing.T) {
	pool := NewPool(slog.Default())
	_ = pool.LoadFromList([]string{
		"socks5://1.2.3.4:1080",
		"socks5://5.6.7.8:1080",
	})

	p1 := pool.AcquireForNetwork("efnet")
	if p1 == nil {
		t.Fatal("expected a proxy")
	}

	pool.ReleaseFromNetwork(p1, "efnet", true)
	if p1.Healthy {
		t.Error("expected proxy to be marked unhealthy")
	}
	if p1.Networks["efnet"] {
		t.Error("expected proxy to be released from efnet")
	}

	if pool.HealthyCount() != 1 {
		t.Errorf("expected 1 healthy, got %d", pool.HealthyCount())
	}
}

func TestPoolAvailableForNetwork(t *testing.T) {
	pool := NewPool(slog.Default())
	_ = pool.LoadFromList([]string{
		"socks5://1.2.3.4:1080",
		"socks5://5.6.7.8:1080",
		"socks5://9.10.11.12:1080",
	})

	if pool.AvailableForNetwork("efnet") != 3 {
		t.Errorf("expected 3 available for efnet, got %d", pool.AvailableForNetwork("efnet"))
	}

	pool.AcquireForNetwork("efnet")
	if pool.AvailableForNetwork("efnet") != 2 {
		t.Errorf("expected 2 available for efnet, got %d", pool.AvailableForNetwork("efnet"))
	}

	if pool.AvailableForNetwork("undernet") != 3 {
		t.Errorf("expected 3 available for undernet, got %d", pool.AvailableForNetwork("undernet"))
	}
}

func TestPoolEmpty(t *testing.T) {
	pool := NewPool(slog.Default())

	if pool.Count() != 0 {
		t.Errorf("expected 0 proxies")
	}
	if pool.HealthyCount() != 0 {
		t.Errorf("expected 0 healthy")
	}

	p := pool.AcquireForNetwork("efnet")
	if p != nil {
		t.Error("expected nil from empty pool")
	}
}

func TestProxyString(t *testing.T) {
	px := &Proxy{
		Host:     "1.2.3.4",
		Port:     "1080",
		Healthy:  true,
		UseCount: 3,
		Networks: map[string]bool{"efnet": true},
	}
	s := px.String()
	expected := "socks5://1.2.3.4:1080 (healthy, used 3 times, 1 networks)"
	if s != expected {
		t.Errorf("unexpected string: %q, want %q", s, expected)
	}
}

func TestProxyHasAuth(t *testing.T) {
	px1 := &Proxy{Username: "user", Password: "pass"}
	if !px1.HasAuth() {
		t.Error("expected HasAuth to be true")
	}

	px2 := &Proxy{}
	if px2.HasAuth() {
		t.Error("expected HasAuth to be false")
	}
}

func TestPoolReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxies.txt")

	if err := os.WriteFile(path, []byte("socks5://1.2.3.4:1080\n"), 0644); err != nil {
		t.Fatal(err)
	}

	pool := NewPool(slog.Default())
	if err := pool.LoadFromFile(path); err != nil {
		t.Fatal(err)
	}
	if pool.Count() != 1 {
		t.Fatalf("expected 1 proxy, got %d", pool.Count())
	}

	if err := os.WriteFile(path, []byte("socks5://1.2.3.4:1080\nsocks5://5.6.7.8:1080\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := pool.Reload(); err != nil {
		t.Fatal(err)
	}
	if pool.Count() != 2 {
		t.Errorf("expected 2 proxies after reload, got %d", pool.Count())
	}
}

func TestProxyNetworkCount(t *testing.T) {
	px := &Proxy{
		Host:     "1.2.3.4",
		Port:     "1080",
		Healthy:  true,
		Networks: make(map[string]bool),
	}

	if px.NetworkCount() != 0 {
		t.Errorf("expected 0 networks, got %d", px.NetworkCount())
	}

	px.Networks["efnet"] = true
	px.Networks["undernet"] = true

	if px.NetworkCount() != 2 {
		t.Errorf("expected 2 networks, got %d", px.NetworkCount())
	}
}
