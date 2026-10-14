package nodeagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"time"

	"github.com/rs/zerolog/log"
)

// How the node becomes reachable by the proxy.
const (
	ExposeNone        = "none"        // the operator supplies --endpoint
	ExposeCloudflared = "cloudflared" // gateway on localhost behind a quick tunnel
)

// startExposure brings up the gateway and tunnel and returns the stream of
// public URLs, or nil when nothing is exposed. It replaces the registered
// token with a fresh access key that only the gateway accepts.
func (a *Agent) startExposure(ctx context.Context) (<-chan string, error) {
	switch a.cfg.Expose {
	case "", ExposeNone:
		return nil, nil
	case ExposeCloudflared:
	default:
		return nil, fmt.Errorf("unknown --expose %q (want %s or %s)", a.cfg.Expose, ExposeNone, ExposeCloudflared)
	}

	bin := a.cfg.CloudflaredBin
	if bin == "" {
		p, err := exec.LookPath("cloudflared")
		if err != nil {
			return nil, errors.New("cloudflared not found on PATH: install it (macOS: brew install cloudflared) or pass --cloudflared-path, or use --expose=none with your own --endpoint")
		}
		bin = p
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate node access key: %w", err)
	}
	key := hex.EncodeToString(raw)
	a.mu.Lock()
	a.cfg.NodeAuthToken = key
	a.mu.Unlock()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for gateway: %w", err)
	}
	srv := &http.Server{Handler: NewGateway(a.cfg.EngineURL, key, a.cfg.EngineToken), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error().Err(err).Msg("gateway stopped")
		}
	}()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info().Str("listen", ln.Addr().String()).Str("engine_url", a.cfg.EngineURL).Msg("gateway up")

	urls := make(chan string, 4)
	tr := &tunnelRunner{binary: bin, probe: a.probe}
	go tr.run(ctx, ln.Addr().String(), urls)
	return urls, nil
}
