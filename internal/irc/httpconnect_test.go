package irc

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// TestHTTPConnectDialer tests the HTTP CONNECT proxy dialer against a mock
// HTTP proxy that accepts CONNECT requests and tunnels data through.
func TestHTTPConnectDialer(t *testing.T) {
	// Start a mock target server (simulates an IRC server)
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start target listener: %v", err)
	}
	defer targetLn.Close()

	go func() {
		conn, err := targetLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Echo back whatever we receive
		_, _ = io.Copy(conn, conn)
	}()

	targetAddr := targetLn.Addr().String()

	// Start a mock HTTP CONNECT proxy
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start proxy listener: %v", err)
	}
	defer proxyLn.Close()

	go func() {
		for {
			clientConn, err := proxyLn.Accept()
			if err != nil {
				return
			}
			go serveMockProxy(clientConn, "")
		}
	}()

	proxyAddr := proxyLn.Addr().String()

	dialer := &httpConnectDialer{
		proxyAddr: proxyAddr,
		timeout:   5 * time.Second,
	}

	conn, err := dialer.Dial("tcp", targetAddr)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer conn.Close()

	// Write data through the tunnel and verify echo
	testData := "HELLO IRC\r\n"
	_, err = conn.Write([]byte(testData))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	buf := make([]byte, len(testData))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := io.ReadFull(conn, buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if string(buf[:n]) != testData {
		t.Errorf("expected echo %q, got %q", testData, string(buf[:n]))
	}
}

// TestHTTPConnectDialerWithAuth tests that authentication credentials are
// sent correctly via the Proxy-Authorization header.
func TestHTTPConnectDialerWithAuth(t *testing.T) {
	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start target listener: %v", err)
	}
	defer targetLn.Close()

	go func() {
		conn, err := targetLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	targetAddr := targetLn.Addr().String()

	authCh := make(chan string, 1)

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start proxy listener: %v", err)
	}
	defer proxyLn.Close()

	go func() {
		clientConn, err := proxyLn.Accept()
		if err != nil {
			return
		}
		serveMockProxyCapture(clientConn, authCh)
	}()

	proxyAddr := proxyLn.Addr().String()

	dialer := &httpConnectDialer{
		proxyAddr: proxyAddr,
		user:      "testuser",
		pass:      "testpass",
		timeout:   5 * time.Second,
	}

	conn, err := dialer.Dial("tcp", targetAddr)
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	conn.Close()

	select {
	case gotAuth := <-authCh:
		// dGVzdHVzZXI6dGVzdHBhc3M= is base64("testuser:testpass")
		expected := "Basic dGVzdHVzZXI6dGVzdHBhc3M="
		if gotAuth != expected {
			t.Errorf("expected auth header %q, got %q", expected, gotAuth)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for auth header")
	}
}

// TestHTTPConnectDialerRejectsNon200 verifies that non-200 CONNECT responses
// cause a connection error.
func TestHTTPConnectDialerRejectsNon200(t *testing.T) {
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start proxy listener: %v", err)
	}
	defer proxyLn.Close()

	go func() {
		conn, err := proxyLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf)
		_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\n\r\n"))
	}()

	proxyAddr := proxyLn.Addr().String()

	dialer := &httpConnectDialer{
		proxyAddr: proxyAddr,
		timeout:   5 * time.Second,
	}

	_, err = dialer.Dial("tcp", "1.2.3.4:6667")
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("expected error to contain 403, got: %v", err)
	}
}

// serveMockProxy handles a single CONNECT request by reading the HTTP request,
// parsing the target host from the CONNECT line, and tunneling to it.
// If requireAuth is non-empty, the proxy rejects requests without that exact
// Proxy-Authorization header.
func serveMockProxy(clientConn net.Conn, requireAuth string) {
	defer clientConn.Close()

	buf := make([]byte, 4096)
	n, err := clientConn.Read(buf)
	if err != nil {
		return
	}
	request := string(buf[:n])

	// Check auth if required
	if requireAuth != "" {
		if !strings.Contains(request, "Proxy-Authorization: "+requireAuth) {
			_, _ = clientConn.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\n\r\n"))
			return
		}
	}

	// Parse the target from "CONNECT host:port HTTP/1.1"
	lines := strings.Split(request, "\r\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "CONNECT ") {
		_, _ = clientConn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		return
	}
	parts := strings.Fields(lines[0])
	if len(parts) < 2 {
		_, _ = clientConn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		return
	}
	targetAddr := parts[1]

	targetConn, err := net.DialTimeout("tcp", targetAddr, 2*time.Second)
	if err != nil {
		_, _ = clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer targetConn.Close()

	_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// Tunnel bidirectionally
	go func() { _, _ = io.Copy(targetConn, clientConn) }()
	_, _ = io.Copy(clientConn, targetConn)
}

// serveMockProxyCapture is like serveMockProxy but also sends the
// Proxy-Authorization header value on the provided channel before tunneling.
func serveMockProxyCapture(clientConn net.Conn, authCh chan<- string) {
	defer clientConn.Close()

	buf := make([]byte, 4096)
	n, err := clientConn.Read(buf)
	if err != nil {
		authCh <- ""
		return
	}
	request := string(buf[:n])

	// Extract auth header
	authHeader := ""
	for _, line := range strings.Split(request, "\r\n") {
		if strings.HasPrefix(line, "Proxy-Authorization: ") {
			authHeader = strings.TrimPrefix(line, "Proxy-Authorization: ")
		}
	}

	// Send the captured header immediately (before tunneling blocks)
	authCh <- authHeader

	// Parse target
	parts := strings.Fields(strings.Split(request, "\r\n")[0])
	if len(parts) < 2 {
		_, _ = clientConn.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		return
	}
	targetAddr := parts[1]

	targetConn, err := net.DialTimeout("tcp", targetAddr, 2*time.Second)
	if err != nil {
		_, _ = clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer targetConn.Close()

	_, _ = clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	go func() { _, _ = io.Copy(targetConn, clientConn) }()
	_, _ = io.Copy(clientConn, targetConn)
}
