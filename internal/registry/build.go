package registry

import (
	"context"
	"fmt"
	"net/url"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/azure"
	"github.com/openai/openai-go/v3/option"
	"google.golang.org/genai"

	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/llm/adapters"
	"github.com/ai-process/llm-proxy/internal/proxydb"
)

// NodePoolResolver builds an adapter for a local node pool model.
type NodePoolResolver interface {
	PoolAdapter(m *proxydb.Model) (llm.ClientAdapter, error)
}

// Adapter returns the vendor adapter for a (client, model) pair, built with
// the client's own decrypted credential and cached for the snapshot's
// lifetime. Concurrent first calls may build twice; LoadOrStore keeps one.
func (s *Snapshot) Adapter(apiKeyID string, m *proxydb.Model) (llm.ClientAdapter, error) {
	cacheKey := apiKeyID + "\x00" + m.ID
	if cached, ok := s.adapters.Load(cacheKey); ok {
		return cached.(llm.ClientAdapter), nil
	}

	if m.Vendor == VendorLocal {
		if s.nodePool != nil {
			adapter, err := s.nodePool.PoolAdapter(m)
			if err != nil {
				return nil, err
			}
			if s.tracker != nil {
				adapter = adapters.NewTrackingAdapter(adapter, s.tracker, m.Vendor, m.ID)
			}
			actual, _ := s.adapters.LoadOrStore(cacheKey, adapter)
			return actual.(llm.ClientAdapter), nil
		}
		return nil, fmt.Errorf("local node pool not configured")
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

// ConfigureOpenAI applies the model's capabilities to an OpenAI-compatible
// adapter. Pool adapters pass the node's engine; direct vendors pass "".
func ConfigureOpenAI(a *adapters.OpenAIAdapter, m *proxydb.Model, engine string) {
	a.SetSoftSchema(SoftSchema(m))
	if HasCapability(m, CapabilityNoThinking) {
		a.SetThinkingOff(ThinkingOff(m.Vendor, engine))
	}
	// The registry id is the model to call, whichever modality it serves.
	if HasCapability(m, CapabilityTTS) {
		a.SetTTS(m.ID, openAITTSVoice)
	}
	if HasCapability(m, CapabilityImage) {
		a.SetImageModel(m.ID)
	}
}

// buildAdapter constructs the raw vendor adapter. In-process throttles are
// no-ops: RPM and budgets are enforced in Redis, shared across replicas.
func buildAdapter(m *proxydb.Model, key string) (llm.ClientAdapter, error) {
	noThrottle := llm.NewThrottleControl(0, 60)
	switch m.Vendor {
	case VendorOpenAI, VendorDeepSeek, VendorOllama, VendorGroq, VendorMistral:
		opts := []option.RequestOption{option.WithAPIKey(key)}
		endpoint := m.Endpoint
		if endpoint == "" {
			switch m.Vendor {
			case VendorDeepSeek:
				endpoint = DeepSeekDefaultEndpoint
			case VendorGroq:
				endpoint = GroqDefaultEndpoint
			case VendorMistral:
				endpoint = MistralDefaultEndpoint
			}
		}
		if endpoint != "" {
			opts = append(opts, option.WithBaseURL(endpoint))
		}
		client := openai.NewClient(opts...)
		adapter := adapters.NewOpenAIAdapter(&client, m.ID, noThrottle)
		if m.Vendor == VendorOpenAI {
			decEndpoint := endpoint
			if decEndpoint == "" {
				decEndpoint = OpenAIDefaultEndpoint
			}
			adapter.SetDecisionsConfig(decEndpoint, key)
		}
		ConfigureOpenAI(adapter, m, "")
		return adapter, nil
	case VendorAzure:
		if m.Endpoint == "" {
			return nil, fmt.Errorf("azure models require an endpoint (e.g. https://<resource>.openai.azure.com)")
		}
		endpoint := m.Endpoint
		apiVersion := AzureDefaultAPIVersion
		if u, err := url.Parse(endpoint); err == nil && u.Query().Get("api-version") != "" {
			apiVersion = u.Query().Get("api-version")
			q := u.Query()
			q.Del("api-version")
			u.RawQuery = q.Encode()
			endpoint = u.String()
		}
		opts := []option.RequestOption{
			azure.WithEndpoint(endpoint, apiVersion),
			azure.WithAPIKey(key),
		}
		client := openai.NewClient(opts...)
		adapter := adapters.NewOpenAIAdapter(&client, m.ID, noThrottle)
		ConfigureOpenAI(adapter, m, "")
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
	case VendorAnthropic:
		opts := []anthropicoption.RequestOption{anthropicoption.WithAPIKey(key)}
		endpoint := m.Endpoint
		if endpoint == "" {
			endpoint = AnthropicDefaultEndpoint
		}
		opts = append(opts, anthropicoption.WithBaseURL(endpoint))
		client := anthropic.NewClient(opts...)
		adapter := adapters.NewAnthropicAdapter(&client, m.ID, noThrottle)
		adapter.SetSoftSchema(SoftSchema(m))
		return adapter, nil
	default:
		return nil, fmt.Errorf("unknown vendor %q", m.Vendor)
	}
}
