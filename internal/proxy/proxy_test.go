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
		Count: 3,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/proxies" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
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
		_ = json.NewEncoder(w).Encode(apiResponse{})
	}))
	defer srv.Close()

	pool := NewPool(slog.Default())
	pool.SetAPI(srv.URL, "socks5", 500)
	_ = pool.FetchFromAPI(context.Background())

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
		Count: 2,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(resp)
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
		_, _ = w.Write([]byte("internal error"))
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

func TestPoolFailCountAndPurge(t *testing.T) {
	pool := newTestPool(t,
		makeProxy("1.2.3.4", "1080"),
		makeProxy("5.6.7.8", "1080"),
	)
	pool.maxRetries = 2

	p1 := pool.AcquireForNetwork("efnet")
	if p1 == nil {
		t.Fatal("expected a proxy")
	}

	// First failure: FailCount=1, still in pool (marked unhealthy)
	pool.ReleaseFromNetwork(p1, "efnet", true)
	if p1.FailCount != 1 {
		t.Errorf("expected fail count 1, got %d", p1.FailCount)
	}
	if p1.Healthy {
		t.Error("expected proxy to be marked unhealthy after first failure")
	}
	if pool.Count() != 2 {
		t.Errorf("expected 2 proxies still in pool, got %d", pool.Count())
	}

	// Re-acquire (it's unhealthy so can't acquire it), but let's manually reset
	// and simulate another failure cycle
	p1.Healthy = true
	p1.Networks["efnet"] = true

	// Second failure: FailCount=2, still in pool
	pool.ReleaseFromNetwork(p1, "efnet", true)
	if p1.FailCount != 2 {
		t.Errorf("expected fail count 2, got %d", p1.FailCount)
	}
	if pool.Count() != 2 {
		t.Errorf("expected 2 proxies still in pool, got %d", pool.Count())
	}

	// Third failure: FailCount=3, exceeds maxRetries=2, should be purged
	p1.Healthy = true
	p1.Networks["efnet"] = true
	pool.ReleaseFromNetwork(p1, "efnet", true)
	if pool.Count() != 1 {
		t.Errorf("expected 1 proxy after purge, got %d", pool.Count())
	}
}

func TestPoolFailCountResetOnSuccess(t *testing.T) {
	pool := newTestPool(t, makeProxy("1.2.3.4", "1080"))
	pool.maxRetries = 2

	p1 := pool.AcquireForNetwork("efnet")
	if p1 == nil {
		t.Fatal("expected a proxy")
	}

	// Fail once
	pool.ReleaseFromNetwork(p1, "efnet", true)
	if p1.FailCount != 1 {
		t.Errorf("expected fail count 1, got %d", p1.FailCount)
	}

	// Successful release resets counter
	p1.Healthy = true
	p1.Networks["efnet"] = true
	pool.ReleaseFromNetwork(p1, "efnet", false)
	if p1.FailCount != 0 {
		t.Errorf("expected fail count reset to 0, got %d", p1.FailCount)
	}
}

func TestPoolReleaseByAddressPurge(t *testing.T) {
	pool := newTestPool(t,
		makeProxy("1.2.3.4", "1080"),
		makeProxy("5.6.7.8", "1080"),
	)
	pool.maxRetries = 0 // purge on first failure

	p1 := pool.AcquireForNetwork("efnet")
	if p1 == nil {
		t.Fatal("expected a proxy")
	}
	addr := p1.ProxyAddress()

	// One failure with maxRetries=0: FailCount=1 > 0 = purged
	pool.ReleaseByAddressFromNetwork(addr, "efnet", true)
	if pool.Count() != 1 {
		t.Errorf("expected 1 proxy after purge, got %d", pool.Count())
	}
}

func TestEnsureAvailable(t *testing.T) {
	fetchCount := 0
	resp := apiResponse{
		Proxies: []apiProxy{
			{IP: "1.2.3.4", Port: 1080, Protocol: "socks5", Alive: true},
			{IP: "5.6.7.8", Port: 1080, Protocol: "socks5", Alive: true},
			{IP: "9.10.11.12", Port: 1080, Protocol: "socks5", Alive: true},
		},
		Count: 3,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	pool := NewPool(slog.Default())
	pool.SetAPI(srv.URL, "", 0)

	// Pool is empty, EnsureAvailable should trigger a fetch
	avail, err := pool.EnsureAvailable(context.Background(), "efnet", 2)
	if err != nil {
		t.Fatalf("EnsureAvailable error: %v", err)
	}
	if avail < 2 {
		t.Errorf("expected at least 2 available, got %d", avail)
	}
	if fetchCount == 0 {
		t.Error("expected at least one API fetch")
	}
}

func TestEnsureAvailable_AlreadySufficient(t *testing.T) {
	fetchCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount++
		_ = json.NewEncoder(w).Encode(apiResponse{})
	}))
	defer srv.Close()

	pool := newTestPool(t,
		makeProxy("1.2.3.4", "1080"),
		makeProxy("5.6.7.8", "1080"),
	)
	pool.SetAPI(srv.URL, "", 0)

	// Already have 2 available, asking for 2 should not fetch
	avail, err := pool.EnsureAvailable(context.Background(), "efnet", 2)
	if err != nil {
		t.Fatalf("EnsureAvailable error: %v", err)
	}
	if avail < 2 {
		t.Errorf("expected at least 2 available, got %d", avail)
	}
	if fetchCount != 0 {
		t.Errorf("expected no API fetches, got %d", fetchCount)
	}
}

func TestRefillIfNeeded(t *testing.T) {
	fetchCount := 0
	resp := apiResponse{
		Proxies: []apiProxy{
			{IP: "1.2.3.4", Port: 1080, Protocol: "socks5", Alive: true},
			{IP: "5.6.7.8", Port: 1080, Protocol: "socks5", Alive: true},
		},
		Count: 2,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount++
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	pool := NewPool(slog.Default())
	pool.SetAPI(srv.URL, "", 0)
	pool.SetMinPoolSize(5)

	// Pool is empty, healthy count (0) < minPoolSize (5) -> should fetch
	err := pool.RefillIfNeeded(context.Background())
	if err != nil {
		t.Fatalf("RefillIfNeeded error: %v", err)
	}
	if fetchCount == 0 {
		t.Error("expected an API fetch")
	}
}

func TestRefillIfNeeded_Disabled(t *testing.T) {
	fetchCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount++
		_ = json.NewEncoder(w).Encode(apiResponse{})
	}))
	defer srv.Close()

	pool := NewPool(slog.Default())
	pool.SetAPI(srv.URL, "", 0)
	// minPoolSize defaults to 0 (disabled)

	err := pool.RefillIfNeeded(context.Background())
	if err != nil {
		t.Fatalf("RefillIfNeeded error: %v", err)
	}
	if fetchCount != 0 {
		t.Errorf("expected no API fetches when min_pool_size=0, got %d", fetchCount)
	}
}

// --- Rotating proxy tests ---

func TestRotatingPoolAcquire(t *testing.T) {
	pool := NewPool(slog.Default())
	pool.SetRotatingProxy("socks5", "gate.proxycheap.com:10000", "testuser", "testpass")

	if !pool.IsRotating() {
		t.Fatal("expected pool to be rotating")
	}
	if pool.Source() != SourceRotating {
		t.Errorf("expected source %q, got %q", SourceRotating, pool.Source())
	}

	// Acquire should always succeed
	p1 := pool.AcquireForNetwork("efnet")
	if p1 == nil {
		t.Fatal("expected a proxy from rotating pool")
	}
	if p1.Host != "gate.proxycheap.com" {
		t.Errorf("expected host gate.proxycheap.com, got %s", p1.Host)
	}
	if p1.Port != "10000" {
		t.Errorf("expected port 10000, got %s", p1.Port)
	}
	if p1.User != "testuser" {
		t.Errorf("expected user testuser, got %s", p1.User)
	}
	if p1.Pass != "testpass" {
		t.Errorf("expected pass testpass, got %s", p1.Pass)
	}
	if p1.Proto != "socks5" {
		t.Errorf("expected proto socks5, got %s", p1.Proto)
	}
	if !p1.Rotating {
		t.Error("expected proxy to be marked as rotating")
	}
	if !p1.Networks["efnet"] {
		t.Error("expected proxy to be assigned to efnet")
	}
	if pool.Count() != 1 {
		t.Errorf("expected 1 active connection, got %d", pool.Count())
	}
}

func TestRotatingPoolMultipleAcquire(t *testing.T) {
	pool := NewPool(slog.Default())
	pool.SetRotatingProxy("socks5", "gate.proxycheap.com:10000", "user", "pass")

	// Acquiring multiple proxies for the same network should work (each is
	// a separate connection through the rotating endpoint).
	p1 := pool.AcquireForNetwork("efnet")
	p2 := pool.AcquireForNetwork("efnet")
	p3 := pool.AcquireForNetwork("efnet")

	if p1 == nil || p2 == nil || p3 == nil {
		t.Fatal("expected all acquires to succeed")
	}

	// Each should be a distinct Proxy instance even though they share the address
	if p1 == p2 || p2 == p3 || p1 == p3 {
		t.Error("expected distinct Proxy instances for each acquire")
	}

	// All should have the same address
	if p1.ProxyAddress() != p2.ProxyAddress() || p2.ProxyAddress() != p3.ProxyAddress() {
		t.Error("expected all proxies to share the rotating endpoint address")
	}

	if pool.Count() != 3 {
		t.Errorf("expected 3 active connections, got %d", pool.Count())
	}
}

func TestRotatingPoolRelease(t *testing.T) {
	pool := NewPool(slog.Default())
	pool.SetRotatingProxy("socks5", "gate.proxycheap.com:10000", "user", "pass")

	p1 := pool.AcquireForNetwork("efnet")
	if p1 == nil {
		t.Fatal("expected a proxy")
	}
	if pool.Count() != 1 {
		t.Errorf("expected 1 active connection, got %d", pool.Count())
	}

	// Releasing a rotating proxy should always remove it from the pool
	pool.ReleaseFromNetwork(p1, "efnet", false)
	if pool.Count() != 0 {
		t.Errorf("expected 0 active connections after release, got %d", pool.Count())
	}
}

func TestRotatingPoolReleaseByAddress(t *testing.T) {
	pool := NewPool(slog.Default())
	pool.SetRotatingProxy("socks5", "gate.proxycheap.com:10000", "user", "pass")

	p1 := pool.AcquireForNetwork("efnet")
	p2 := pool.AcquireForNetwork("undernet")
	if p1 == nil || p2 == nil {
		t.Fatal("expected proxies")
	}
	if pool.Count() != 2 {
		t.Errorf("expected 2 active connections, got %d", pool.Count())
	}

	// Release the efnet one by address -- should only remove one entry
	pool.ReleaseByAddressFromNetwork(p1.ProxyAddress(), "efnet", false)
	if pool.Count() != 1 {
		t.Errorf("expected 1 active connection after releasing efnet, got %d", pool.Count())
	}

	// The remaining one should be the undernet proxy
	remaining := pool.All()
	if len(remaining) != 1 || !remaining[0].Networks["undernet"] {
		t.Error("expected remaining proxy to be the undernet connection")
	}
}

func TestRotatingPoolReleaseByAddressFailed(t *testing.T) {
	pool := NewPool(slog.Default())
	pool.SetRotatingProxy("socks5", "gate.proxycheap.com:10000", "user", "pass")

	p1 := pool.AcquireForNetwork("efnet")
	if p1 == nil {
		t.Fatal("expected a proxy")
	}

	// Even a failed release should remove the entry for rotating proxies
	pool.ReleaseByAddressFromNetwork(p1.ProxyAddress(), "efnet", true)
	if pool.Count() != 0 {
		t.Errorf("expected 0 active connections after failed release, got %d", pool.Count())
	}
}

func TestRotatingPoolAvailableForNetwork(t *testing.T) {
	pool := NewPool(slog.Default())
	pool.SetRotatingProxy("socks5", "gate.proxycheap.com:10000", "user", "pass")

	// Rotating pools report large availability
	avail := pool.AvailableForNetwork("efnet")
	if avail < 100 {
		t.Errorf("expected large availability for rotating pool, got %d", avail)
	}
}

func TestRotatingPoolEnsureAvailable(t *testing.T) {
	pool := NewPool(slog.Default())
	pool.SetRotatingProxy("socks5", "gate.proxycheap.com:10000", "user", "pass")

	// Should always succeed without API calls
	avail, err := pool.EnsureAvailable(context.Background(), "efnet", 10)
	if err != nil {
		t.Fatalf("EnsureAvailable error: %v", err)
	}
	if avail < 10 {
		t.Errorf("expected at least 10 available, got %d", avail)
	}
}

func TestRotatingPoolFetchFromAPINoOp(t *testing.T) {
	pool := NewPool(slog.Default())
	pool.SetRotatingProxy("socks5", "gate.proxycheap.com:10000", "user", "pass")

	// FetchFromAPI should be a no-op for rotating pools
	err := pool.FetchFromAPI(context.Background())
	if err != nil {
		t.Fatalf("expected no error from FetchFromAPI on rotating pool, got %v", err)
	}
}

func TestRotatingPoolRefillIfNeededNoOp(t *testing.T) {
	pool := NewPool(slog.Default())
	pool.SetRotatingProxy("socks5", "gate.proxycheap.com:10000", "user", "pass")
	pool.SetMinPoolSize(100) // even with a min pool size set

	err := pool.RefillIfNeeded(context.Background())
	if err != nil {
		t.Fatalf("expected no error from RefillIfNeeded on rotating pool, got %v", err)
	}
}

func TestRotatingProxyString(t *testing.T) {
	px := &Proxy{
		Host:     "gate.proxycheap.com",
		Port:     "10000",
		User:     "testuser",
		Pass:     "testpass",
		Healthy:  true,
		Rotating: true,
		UseCount: 1,
		Networks: map[string]bool{"efnet": true},
	}
	s := px.String()
	expected := "socks5://testuser@gate.proxycheap.com:10000 (healthy, used 1 times, 1 networks rotating)"
	if s != expected {
		t.Errorf("unexpected string: %q, want %q", s, expected)
	}
}

func TestRotatingPoolNoAddr(t *testing.T) {
	pool := NewPool(slog.Default())
	pool.SetSource(SourceRotating)
	// No address configured

	p := pool.AcquireForNetwork("efnet")
	if p != nil {
		t.Error("expected nil when rotating address is not configured")
	}
}

func TestSetSourcePreservesAPIBehavior(t *testing.T) {
	pool := NewPool(slog.Default())
	// Default should be API source
	if pool.Source() != SourceAPI {
		t.Errorf("expected default source %q, got %q", SourceAPI, pool.Source())
	}
	if pool.IsRotating() {
		t.Error("expected pool not to be rotating by default")
	}
}

func TestRotatingPoolHTTPProto(t *testing.T) {
	pool := NewPool(slog.Default())
	pool.SetRotatingProxy("http", "proxy-us.proxy-cheap.com:9595", "user", "pass")

	px := pool.AcquireForNetwork("efnet")
	if px == nil {
		t.Fatal("expected a proxy")
	}
	if px.Proto != "http" {
		t.Errorf("expected proto http, got %s", px.Proto)
	}
	if px.Host != "proxy-us.proxy-cheap.com" {
		t.Errorf("expected host proxy-us.proxy-cheap.com, got %s", px.Host)
	}
	if px.Port != "9595" {
		t.Errorf("expected port 9595, got %s", px.Port)
	}
}

func TestRotatingPoolDefaultProto(t *testing.T) {
	pool := NewPool(slog.Default())
	// Empty proto should default to socks5
	pool.SetRotatingProxy("", "gate.proxycheap.com:10000", "user", "pass")

	px := pool.AcquireForNetwork("efnet")
	if px == nil {
		t.Fatal("expected a proxy")
	}
	if px.Proto != "socks5" {
		t.Errorf("expected proto to default to socks5, got %s", px.Proto)
	}
}
