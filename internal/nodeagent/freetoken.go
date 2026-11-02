package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

// FreeToken defaults: `ft serve` listens on 1919 and runs 4 requests at once
// (--max-running-requests); the rest wait inside the engine.
const (
	FreeTokenEngineURL  = "http://127.0.0.1:1919"
	FreeTokenMaxRunning = 4
)

// FreeTokenInspector reads a FreeToken server (`ft serve`): one model per
// process, no /metrics, its queue only as the count of admitted requests.
type FreeTokenInspector struct {
	// MaxRunning is the server's --max-running-requests; admitted requests
	// beyond it are waiting. Zero means FreeTokenMaxRunning.
	MaxRunning uint32
}

// ErrFreeTokenFailed is a server whose /health reports a dead backend.
var ErrFreeTokenFailed = errors.New("freetoken server failed")

type freeTokenModels struct {
	Data []struct {
		ID            string `json:"id"`
		ContextLength uint32 `json:"context_length"`
	} `json:"data"`
}

type freeTokenHealth struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type freeTokenStats struct {
	Requests *struct {
		Active uint32 `json:"active"`
	} `json:"requests"`
}

func (fi *FreeTokenInspector) Inspect(ctx context.Context, endpoint, authToken string) ([]*pb.NodeModelSpec, error) {
	base := engineBaseURL(endpoint)
	var models freeTokenModels
	if err := getJSON(ctx, base+"/v1/models", authToken, 5*time.Second, &models); err != nil {
		return nil, fmt.Errorf("freetoken /v1/models: %w", err)
	}
	// /health needs no key; a server still loading may answer it with 503.
	var health freeTokenHealth
	if err := getJSON(ctx, base+"/health", "", 5*time.Second, &health, http.StatusServiceUnavailable); err != nil {
		return nil, fmt.Errorf("freetoken /health: %w", err)
	}
	if health.Status == "error" {
		return nil, fmt.Errorf("%w: %s", ErrFreeTokenFailed, health.Message)
	}

	specs := make([]*pb.NodeModelSpec, 0, len(models.Data))
	for _, m := range models.Data {
		ctxLen := m.ContextLength
		if ctxLen == 0 {
			ctxLen = 4096
		}
		specs = append(specs, &pb.NodeModelSpec{
			ModelId:         m.ID,
			RemoteModelName: m.ID,
			ContextLength:   ctxLen,
			IsWarm:          health.Status == "ok", // "loading" while weights are read
		})
	}
	return specs, nil
}

// Load splits /v1/stats requests.active, which counts every admitted request,
// into the MaxRunning the engine generates and the rest it holds back.
func (fi *FreeTokenInspector) Load(ctx context.Context, endpoint, authToken string) (EngineLoad, error) {
	var stats freeTokenStats
	if err := getJSON(ctx, engineBaseURL(endpoint)+"/v1/stats", authToken, time.Second, &stats); err != nil {
		return EngineLoad{}, fmt.Errorf("freetoken /v1/stats: %w", err)
	}
	if stats.Requests == nil {
		return EngineLoad{}, fmt.Errorf("freetoken /v1/stats has no requests")
	}
	limit := fi.MaxRunning
	if limit == 0 {
		limit = FreeTokenMaxRunning
	}
	active := stats.Requests.Active
	if active <= limit {
		return EngineLoad{Running: active}, nil
	}
	return EngineLoad{Running: limit, Waiting: active - limit}, nil
}

// getJSON decodes the body of a 200, or of one of the other statuses given.
func getJSON(ctx context.Context, url, authToken string, timeout time.Duration, into any, alsoOK ...int) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && !slices.Contains(alsoOK, resp.StatusCode) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10)) // keeps the connection reusable
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}
