package registry

import (
	"context"
	"fmt"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"google.golang.org/genai"

	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/llm/adapters"
	"github.com/ai-process/llm-proxy/internal/proxydb"
)

// Adapter returns the vendor adapter for a (client, model) pair, built with
// the client's own decrypted credential and cached for the snapshot's
// lifetime. Concurrent first calls may build twice; LoadOrStore keeps one.
func (s *Snapshot) Adapter(apiKeyID string, m *proxydb.Model) (llm.ClientAdapter, error) {
	cacheKey := apiKeyID + "\x00" + m.ID
	if cached, ok := s.adapters.Load(cacheKey); ok {
		return cached.(llm.ClientAdapter), nil
	}

	key, ok := s.VendorKey(apiKeyID, m.Vendor)
	if !ok {
		return nil, fmt.Errorf("no %s vendor key for this client", m.Vendor)
	}
	adapter, err := buildAdapter(m, key)
	if err != nil {
		return nil, err
	}
	if s.tracker != nil {
		adapter = adapters.NewTrackingAdapter(adapter, s.tracker, m.Vendor, m.ID)
	}
	actual, _ := s.adapters.LoadOrStore(cacheKey, adapter)
	return actual.(llm.ClientAdapter), nil
}

// Default voices for models that declare the tts capability. The registry
// stores one model id per row and that id is the TTS model itself, so only the
// voice needs a default; make it a model field if a caller ever needs a choice.
const (
	googleTTSVoice = "Aoede"
	openAITTSVoice = "marin"
)

// buildAdapter constructs the raw vendor adapter. In-process throttles are
// no-ops: RPM and budgets are enforced in Redis, shared across replicas.
func buildAdapter(m *proxydb.Model, key string) (llm.ClientAdapter, error) {
	noThrottle := llm.NewThrottleControl(0, 60)
	switch m.Vendor {
	case VendorOpenAI, VendorDeepSeek:
		opts := []option.RequestOption{option.WithAPIKey(key)}
		endpoint := m.Endpoint
		if endpoint == "" && m.Vendor == VendorDeepSeek {
			endpoint = DeepSeekDefaultEndpoint
		}
		if endpoint != "" {
			opts = append(opts, option.WithBaseURL(endpoint))
		}
		client := openai.NewClient(opts...)
		adapter := adapters.NewOpenAIAdapter(&client, m.ID, noThrottle)
		adapter.SetSoftSchema(SoftSchema(m))
		adapter.SetNoThinking(HasCapability(m, CapabilityNoThinking))
		// The registry id is the model to call, whichever modality it serves.
		if HasCapability(m, CapabilityTTS) {
			adapter.SetTTS(m.ID, openAITTSVoice)
		}
		if HasCapability(m, CapabilityImage) {
			adapter.SetImageModel(m.ID)
		}
		return adapter, nil
	case VendorGoogle:
		config := &genai.ClientConfig{APIKey: key, Backend: genai.BackendGeminiAPI}
		if m.Endpoint != "" {
			config.HTTPOptions = genai.HTTPOptions{BaseURL: m.Endpoint}
		}
		client, err := genai.NewClient(context.Background(), config)
		if err != nil {
			return nil, fmt.Errorf("genai client: %w", err)
		}
		adapter := adapters.NewGoogleAdapter(client, m.ID, noThrottle)
		adapter.SetSoftSchema(SoftSchema(m))
		if HasCapability(m, CapabilityTTS) {
			adapter.SetTTS(m.ID, googleTTSVoice)
		}
		if HasCapability(m, CapabilityImage) {
			adapter.SetImageModel(m.ID)
		}
		return adapter, nil
	case VendorTypeSafe:
		endpoint := m.Endpoint
		if endpoint == "" {
			endpoint = TypeSafeDefaultEndpoint
		}
		return adapters.NewTypeSafeAdapter(endpoint, key, m.ID), nil
	default:
		return nil, fmt.Errorf("unknown vendor %q", m.Vendor)
	}
}
