package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/registry"
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
	Model struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"model"`
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
	var health freeTokenHealth
	if err := getJSON(ctx, base+"/health", "", 5*time.Second, &health); err != nil {
		return nil, fmt.Errorf("freetoken /health: %w", err)
	}
	if health.Status == "error" {
		return nil, fmt.Errorf("freetoken /health: %s", health.Message)
	}
	// response_format json_object/json_schema is rejected: the router asks in the prompt.
	caps := []string{registry.CapabilityNoStructuredOutput}
	var stats freeTokenStats
	if getJSON(ctx, base+"/v1/stats", authToken, 5*time.Second, &stats) == nil && slices.Contains(stats.Model.InputModalities, "image") {
		caps = append(caps, registry.CapabilityVision)
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
			Capabilities:    slices.Clone(caps),
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

func getJSON(ctx context.Context, url, authToken string, timeout time.Duration, into any) error {
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
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}
