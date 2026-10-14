package observability

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog/log"
)

// Server exposes Prometheus metrics on a dedicated internal HTTP listener.
type Server struct {
	srv *http.Server
}

// NewServer initializes the internal metrics server.
func NewServer(addr string) *Server {
	if addr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	return &Server{srv: srv}
}

// Start launches the metrics server in a background goroutine.
func (s *Server) Start() {
	if s == nil || s.srv == nil {
		return
	}
	go func() {
		log.Info().Str("addr", s.srv.Addr).Msg("metrics server listening")
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error().Err(err).Msg("metrics server error")
		}
	}()
}

// Shutdown gracefully shuts down the metrics server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}
