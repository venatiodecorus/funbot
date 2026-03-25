// Package health provides HTTP liveness and readiness probe endpoints
// for Kubernetes health checks.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	// DefaultAddr is the default listen address for health checks.
	DefaultAddr = ":8080"

	// ShutdownTimeout is the time allowed for the HTTP server to drain.
	ShutdownTimeout = 5 * time.Second
)

// ReadinessChecker is a function that returns true if the service is ready
// to receive traffic.
type ReadinessChecker func() bool

// Server runs an HTTP server that exposes /healthz and /readyz endpoints.
type Server struct {
	httpServer *http.Server
	ready      ReadinessChecker
	log        *slog.Logger
	mu         sync.RWMutex
	alive      bool
}

// New creates a new health check server.
// The readyFn is called on each /readyz request to determine readiness.
func New(addr string, readyFn ReadinessChecker, log *slog.Logger) *Server {
	if addr == "" {
		addr = DefaultAddr
	}

	s := &Server{
		ready: readyFn,
		log:   log.With("component", "health"),
		alive: true,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleLiveness)
	mux.HandleFunc("/readyz", s.handleReadiness)

	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	return s
}

// Start begins listening for health check requests in a goroutine.
// It returns immediately. Use Shutdown to stop the server.
func (s *Server) Start() {
	go func() {
		s.log.Info("health check server starting", "addr", s.httpServer.Addr)
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.log.Error("health check server error", "error", err)
		}
	}()
}

// Shutdown gracefully stops the health check server.
func (s *Server) Shutdown() {
	s.mu.Lock()
	s.alive = false
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), ShutdownTimeout)
	defer cancel()

	if err := s.httpServer.Shutdown(ctx); err != nil {
		s.log.Error("health check server shutdown error", "error", err)
	}
	s.log.Info("health check server stopped")
}

// handleLiveness responds to /healthz. Returns 200 if the process is alive.
func (s *Server) handleLiveness(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	alive := s.alive
	s.mu.RUnlock()

	resp := map[string]string{"status": "ok"}
	status := http.StatusOK

	if !alive {
		resp["status"] = "shutting_down"
		status = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

// handleReadiness responds to /readyz. Returns 200 if the service is
// ready to receive traffic (e.g., IRC connected, Redis reachable).
func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	alive := s.alive
	s.mu.RUnlock()

	if !alive {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "shutting_down"})
		return
	}

	ready := s.ready != nil && s.ready()

	resp := map[string]string{}
	status := http.StatusOK

	if ready {
		resp["status"] = "ready"
	} else {
		resp["status"] = "not_ready"
		status = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(resp)
}

// String returns a description of the health server for logging.
func (s *Server) String() string {
	return fmt.Sprintf("health server on %s", s.httpServer.Addr)
}
