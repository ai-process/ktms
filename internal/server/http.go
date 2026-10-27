package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/ai-process/ktms/internal/observability"
)

// Pinger is the readiness dependency (the pgx pool in production).
type Pinger interface {
	Ping(ctx context.Context) error
}

// Registrar mounts more handlers on the HTTP server, such as the MCP gateway.
type Registrar interface {
	Register(mux *http.ServeMux)
}

// HTTPServer serves the health endpoints and, when configured, the OpenAI-compatible API.
type HTTPServer struct {
	degradedFor time.Duration // how long after the last good ping a down database still reads as degraded
	lastPingOK  atomic.Int64  // unix nanos of the last successful ping
	server      *http.Server
	pinger      Pinger
	addr        string
}

// NewHTTPServer creates the HTTP server. api may be nil (health only);
// requestTimeout bounds one API call, which waits on an upstream model.
func NewHTTPServer(pinger Pinger, addr string, api *OpenAIAPI, requestTimeout time.Duration, more ...Registrar) *HTTPServer {
	s := &HTTPServer{pinger: pinger, addr: addr}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	if api != nil {
		api.Register(mux)
	}
	for _, r := range more {
		r.Register(mux)
	}

	s.server = &http.Server{
		Addr:         addr,
		Handler:      otelhttp.NewHandler(mux, "http.server"),
		ReadTimeout:  requestTimeout,
		WriteTimeout: requestTimeout,
		IdleTimeout:  120 * time.Second,
	}
	return s
}

// Start starts the HTTP server
func (s *HTTPServer) Start() error {
	log.Info().Str("addr", s.addr).Msg("starting HTTP server")
	return s.server.ListenAndServe()
}

// Shutdown gracefully shuts down the HTTP server
func (s *HTTPServer) Shutdown(ctx context.Context) error {
	log.Info().Msg("shutting down HTTP server")
	return s.server.Shutdown(ctx)
}

// Version is the build's release version, reported by /healthz.
var Version = "dev"

func (s *HTTPServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": Version})
}

// WithDegradedReadiness keeps /readyz at 200 for up to window after the last
// good ping, while recently verified keys still work (AUTH_STALE_IF_ERROR): a
// 503 would pull every replica at once. Past the window nothing is served.
func (s *HTTPServer) WithDegradedReadiness(window time.Duration) *HTTPServer {
	s.degradedFor = window
	s.lastPingOK.Store(time.Now().UnixNano()) // boot loaded config, so the database was just up
	return s
}

func (s *HTTPServer) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// Under the probe's own 5s timeout, so a slow database reads as down, not as a hung probe.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	w.Header().Set("Content-Type", "application/json")
	if s.pinger == nil {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ready"}`))
		return
	}
	if err := s.pinger.Ping(ctx); err != nil {
		observability.DependencyErrorsTotal.WithLabelValues("postgres", "readiness").Inc()
		log.Warn().Err(err).Msg("readiness check: database unavailable")
		if s.degradedFor > 0 && time.Since(time.Unix(0, s.lastPingOK.Load())) < s.degradedFor {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"degraded","database":"unavailable"}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"not ready","error":"database unavailable"}`))
		return
	}
	s.lastPingOK.Store(time.Now().UnixNano())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ready"}`))
}
