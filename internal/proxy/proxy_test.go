package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestPool creates a pool with some proxies pre-loaded for testing.
func newTestPool(t *testing.T, proxies ...*Proxy) *Pool {
	t.Helper()
	pool := NewPool(slog.Default())
	pool.proxies = proxies
	return pool
}

func makeProxy(host, port string) *Proxy {
	return &Proxy{
		Host:     host,
		Port:     port,
		Healthy:  true,
		Networks: make(map[string]bool),
	}
}

func TestFetchFromAPI(t *testing.T) {
	resp := apiResponse{
		Proxies: []apiProxy{
			{IP: "1.2.3.4", Port: 1080, Protocol: "socks5", Alive: true, Country: "US"},
			{IP: "5.6.7.8", Port: 1080, Protocol: "socks5", Alive: true, Country: "DE"},
			{IP: "9.9.9.9", Port: 1080, Protocol: "socks5", Alive: false, Country: "FR"}, // dead
		},
		Total: 3,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/proxies" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	pool := NewPool(slog.Default())
	pool.SetAPI(srv.URL, "socks5", 0)

	err := pool.FetchFromAPI(context.Background())
	if err != nil {
		t.Fatalf("FetchFromAPI error: %v", err)
	}

	// Should have 2 alive proxies (dead one filtered)
	if pool.Count() != 2 {
		t.Errorf("expected 2 proxies, got %d", pool.Count())
	}
	if pool.HealthyCount() != 2 {
		t.Errorf("expected 2 healthy, got %d", pool.HealthyCount())
	}
}

func TestFetchFromAPI_QueryParams(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(apiResponse{})
	}))
	defer srv.Close()

	pool := NewPool(slog.Default())
	pool.SetAPI(srv.URL, "socks5", 500)
	pool.FetchFromAPI(context.Background())

	if gotQuery == "" {
		t.Fatal("expected query parameters")
	}
	// Check that protocol and max_latency are passed
	if got := gotQuery; got == "" {
		t.Fatal("empty query")
	}
	// Just verify the key params are present
	if !contains(gotQuery, "protocol=socks5") {
		t.Errorf("expected protocol=socks5 in query, got %q", gotQuery)
	}
	if !contains(gotQuery, "max_latency=500") {
		t.Errorf("expected max_latency=500 in query, got %q", gotQuery)
	}
	if !contains(gotQuery, "limit=1000") {
		t.Errorf("expected limit=1000 in query, got %q", gotQuery)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsAt(s, substr))
}

func containsAt(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestFetchFromAPI_PreservesInUse(t *testing.T) {
	// Start with an in-use proxy
	existing := makeProxy("1.2.3.4", "1080")
	existing.Networks["efnet"] = true
	existing.UseCount = 5

	pool := newTestPool(t, existing)

	// API returns same proxy plus a new one
	resp := apiResponse{
		Proxies: []apiProxy{
			{IP: "1.2.3.4", Port: 1080, Protocol: "socks5", Alive: true},
			{IP: "5.6.7.8", Port: 1080, Protocol: "socks5", Alive: true},
		},
		Total: 2,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	pool.SetAPI(srv.URL, "", 0)
	if err := pool.FetchFromAPI(context.Background()); err != nil {
		t.Fatal(err)
	}

	if pool.Count() != 2 {
		t.Errorf("expected 2 proxies, got %d", pool.Count())
	}

	// Verify the in-use proxy was preserved with its state
	found := false
	for _, px := range pool.All() {
		if px.Host == "1.2.3.4" && px.Networks["efnet"] {
			found = true
			if px.UseCount != 5 {
				t.Errorf("expected preserved use count 5, got %d", px.UseCount)
			}
		}
	}
	if !found {
		t.Error("expected preserved in-use proxy with efnet assignment")
	}
}

func TestFetchFromAPI_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()

	pool := NewPool(slog.Default())
	pool.SetAPI(srv.URL, "", 0)

	err := pool.FetchFromAPI(context.Background())
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestFetchFromAPI_NoURL(t *testing.T) {
	pool := NewPool(slog.Default())
	err := pool.FetchFromAPI(context.Background())
	if err == nil {
		t.Fatal("expected error when no API URL configured")
	}
}

func TestPoolAcquireForNetwork(t *testing.T) {
	pool := newTestPool(t,
		makeProxy("1.2.3.4", "1080"),
		makeProxy("5.6.7.8", "1080"),
	)

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

	// Same proxy can serve a different network
	p3 := pool.AcquireForNetwork("undernet")
	if p3 == nil {
		t.Fatal("expected a proxy for different network")
	}
	if !p3.Networks["undernet"] {
		t.Error("expected proxy to be marked for undernet")
	}

	// All proxies used for efnet
	p4 := pool.AcquireForNetwork("efnet")
	if p4 != nil {
		t.Error("expected nil when all proxies are used for network")
	}
}

func TestPoolReleaseFromNetwork(t *testing.T) {
	pool := newTestPool(t,
		makeProxy("1.2.3.4", "1080"),
		makeProxy("5.6.7.8", "1080"),
	)

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
	pool := newTestPool(t,
		makeProxy("1.2.3.4", "1080"),
		makeProxy("5.6.7.8", "1080"),
		makeProxy("9.10.11.12", "1080"),
	)

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
		Country:  "US",
	}
	s := px.String()
	expected := "socks5://1.2.3.4:1080 (healthy, used 3 times, 1 networks US)"
	if s != expected {
		t.Errorf("unexpected string: %q, want %q", s, expected)
	}
}

func TestProxyNetworkCount(t *testing.T) {
	px := makeProxy("1.2.3.4", "1080")

	if px.NetworkCount() != 0 {
		t.Errorf("expected 0 networks, got %d", px.NetworkCount())
	}

	px.Networks["efnet"] = true
	px.Networks["undernet"] = true

	if px.NetworkCount() != 2 {
		t.Errorf("expected 2 networks, got %d", px.NetworkCount())
	}
}
