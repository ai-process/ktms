package latent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const maxEncoderResponseBytes = 64 << 20

// HTTPEncoder calls an OpenAI-compatible /v1/embeddings endpoint, which is
// what a node-agent hosting a JEPA encoder (or any embedding server) serves.
type HTTPEncoder struct {
	url     string
	model   string
	apiKey  string
	timeout time.Duration
	client  *http.Client
}

// NewHTTPEncoder targets url with the given model name; timeout bounds one
// Encode call so a slow encoder cannot stall the request path.
func NewHTTPEncoder(url, model, apiKey string, timeout time.Duration, client *http.Client) *HTTPEncoder {
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPEncoder{url: url, model: model, apiKey: apiKey, timeout: timeout, client: client}
}

type embeddingsRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embeddingsResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Encode sends all texts in one request and returns unit-length vectors.
func (e *HTTPEncoder) Encode(ctx context.Context, texts []string) ([]Embedding, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if e.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.timeout)
		defer cancel()
	}
	body, err := json.Marshal(embeddingsRequest{Model: e.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("latent: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("latent: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("latent: encoder request: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxEncoderResponseBytes))
	if cerr := resp.Body.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("latent: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := raw
		if len(snippet) > 256 {
			snippet = snippet[:256]
		}
		return nil, fmt.Errorf("latent: encoder returned %d: %s", resp.StatusCode, snippet)
	}
	var parsed embeddingsResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("latent: decode response: %w", err)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("%w: encoder returned %d vectors for %d inputs", ErrInvalidVector, len(parsed.Data), len(texts))
	}
	out := make([]Embedding, len(texts))
	dims := -1
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(texts) || out[d.Index].Vector != nil {
			return nil, fmt.Errorf("%w: bad or duplicate index %d", ErrInvalidVector, d.Index)
		}
		if dims >= 0 && len(d.Embedding) != dims {
			return nil, fmt.Errorf("%w: mixed dimensions %d and %d", ErrInvalidVector, dims, len(d.Embedding))
		}
		dims = len(d.Embedding)
		if err := Normalize(d.Embedding); err != nil {
			return nil, err
		}
		out[d.Index] = Embedding{Model: e.model, Vector: d.Embedding}
	}
	return out, nil
}
