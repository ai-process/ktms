package nodeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
)

type EngineInspector interface {
	Inspect(ctx context.Context, endpoint, authToken string) ([]*pb.NodeModelSpec, error)
}

func GetInspector(engine string) EngineInspector {
	switch strings.ToLower(engine) {
	case "ollama":
		return &OllamaInspector{}
	case "vllm":
		return &VLLMInspector{}
	case "tgi":
		return &TGIInspector{}
	case "llamacpp":
		return &LlamaCppInspector{}
	case "decisions":
		return &DecisionsInspector{}
	default:
		return &GenericOpenAIInspector{}
	}
}

// Ollama Inspector
type OllamaInspector struct{}

type ollamaTagsResponse struct {
	Models []struct {
		Name    string `json:"name"`
		Model   string `json:"model"`
		Details struct {
			Format            string `json:"format"`
			Family            string `json:"family"`
			ParameterSize     string `json:"parameter_size"`
			QuantizationLevel string `json:"quantization_level"`
		} `json:"details"`
	} `json:"models"`
}

type ollamaPsResponse struct {
	Models []struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	} `json:"models"`
}

func (oi *OllamaInspector) Inspect(ctx context.Context, endpoint, authToken string) ([]*pb.NodeModelSpec, error) {
	baseURL := strings.TrimRight(endpoint, "/")
	if strings.HasSuffix(baseURL, "/v1") {
		baseURL = strings.TrimSuffix(baseURL, "/v1")
	}

	client := &http.Client{Timeout: 5 * time.Second}

	// 1. Fetch available models from /api/tags
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama /api/tags: %w", err)
	}
	defer resp.Body.Close()

	var tagsResp ollamaTagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tagsResp); err != nil {
		return nil, fmt.Errorf("decode ollama /api/tags: %w", err)
	}

	// 2. Fetch loaded/running models from /api/ps
	warmMap := make(map[string]bool)
	psReq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/ps", nil)
	if err == nil {
		if authToken != "" {
			psReq.Header.Set("Authorization", "Bearer "+authToken)
		}
		if psResp, err := client.Do(psReq); err == nil {
			var psData ollamaPsResponse
			_ = json.NewDecoder(psResp.Body).Decode(&psData)
			psResp.Body.Close()
			for _, m := range psData.Models {
				warmMap[m.Name] = true
				warmMap[m.Model] = true
			}
		}
	}

	var specs []*pb.NodeModelSpec
	for _, m := range tagsResp.Models {
		isWarm := warmMap[m.Name] || warmMap[m.Model]
		specs = append(specs, &pb.NodeModelSpec{
			ModelId:         m.Name,
			RemoteModelName: m.Name,
			ContextLength:   4096,
			IsWarm:          isWarm,
			Quantization:    m.Details.QuantizationLevel,
			Family:          m.Details.Family,
		})
	}
	return specs, nil
}

// vLLM Inspector
type VLLMInspector struct{}

type vllmModelsResponse struct {
	Data []struct {
		ID           string   `json:"id"`
		Capabilities []string `json:"capabilities"`
	} `json:"data"`
}

func (vi *VLLMInspector) Inspect(ctx context.Context, endpoint, authToken string) ([]*pb.NodeModelSpec, error) {
	baseURL := strings.TrimRight(endpoint, "/")
	if !strings.HasSuffix(baseURL, "/v1") {
		baseURL += "/v1"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var mResp vllmModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&mResp); err != nil {
		return nil, err
	}

	var specs []*pb.NodeModelSpec
	for _, m := range mResp.Data {
		specs = append(specs, &pb.NodeModelSpec{
			ModelId:         m.ID,
			RemoteModelName: m.ID,
			ContextLength:   8192,
			IsWarm:          true, // vLLM serves loaded weights
		})
	}
	return specs, nil
}

// TGI Inspector
type TGIInspector struct{}

type tgiInfoResponse struct {
	ModelID             string `json:"model_id"`
	MaxTotalTokens      uint32 `json:"max_total_tokens"`
	MaxInputTokens      uint32 `json:"max_input_length"`
	ModelSha            string `json:"model_sha"`
}

func (ti *TGIInspector) Inspect(ctx context.Context, endpoint, authToken string) ([]*pb.NodeModelSpec, error) {
	baseURL := strings.TrimRight(endpoint, "/")
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/info", nil)
	if err != nil {
		return nil, err
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var info tgiInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, err
	}

	return []*pb.NodeModelSpec{
		{
			ModelId:         info.ModelID,
			RemoteModelName: info.ModelID,
			ContextLength:   info.MaxTotalTokens,
			IsWarm:          true,
		},
	}, nil
}

// LlamaCpp Inspector
type LlamaCppInspector struct{}

type llamaCppPropsResponse struct {
	DefaultGenerationSettings struct {
		NCtx uint32 `json:"n_ctx"`
	} `json:"default_generation_settings"`
}

func (li *LlamaCppInspector) Inspect(ctx context.Context, endpoint, authToken string) ([]*pb.NodeModelSpec, error) {
	baseURL := strings.TrimRight(endpoint, "/")
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/props", nil)
	if err != nil {
		return nil, err
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var props llamaCppPropsResponse
	if err := json.NewDecoder(resp.Body).Decode(&props); err != nil {
		return nil, err
	}

	return []*pb.NodeModelSpec{
		{
			ModelId:         "llamacpp-model",
			RemoteModelName: "llamacpp-model",
			ContextLength:   props.DefaultGenerationSettings.NCtx,
			IsWarm:          true,
		},
	}, nil
}

// Generic OpenAI Inspector
type GenericOpenAIInspector struct{}

func (goi *GenericOpenAIInspector) Inspect(ctx context.Context, endpoint, authToken string) ([]*pb.NodeModelSpec, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	baseURL := strings.TrimRight(u.String(), "/")
	if !strings.HasSuffix(baseURL, "/v1") {
		baseURL += "/v1"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var mResp vllmModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&mResp); err != nil {
		return nil, err
	}

	var specs []*pb.NodeModelSpec
	for _, m := range mResp.Data {
		specs = append(specs, &pb.NodeModelSpec{
			ModelId:         m.ID,
			RemoteModelName: m.ID,
			ContextLength:   4096,
			IsWarm:          true,
			Capabilities:    m.Capabilities,
		})
	}
	return specs, nil
}

// DecisionsInspector inspects engines serving decision models (/v1/decisions).
type DecisionsInspector struct{}

func (di *DecisionsInspector) Inspect(ctx context.Context, endpoint, authToken string) ([]*pb.NodeModelSpec, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	baseURL := strings.TrimRight(u.String(), "/")
	if !strings.HasSuffix(baseURL, "/v1") {
		baseURL += "/v1"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var mResp vllmModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&mResp); err != nil {
		return nil, err
	}

	var specs []*pb.NodeModelSpec
	for _, m := range mResp.Data {
		caps := m.Capabilities
		if len(caps) == 0 {
			caps = []string{"judge"}
		}
		specs = append(specs, &pb.NodeModelSpec{
			ModelId:         m.ID,
			RemoteModelName: m.ID,
			ContextLength:   4096,
			IsWarm:          true,
			Capabilities:    caps,
		})
	}
	return specs, nil
}
