package registry

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/azure"
	"github.com/openai/openai-go/v3/option"
	"google.golang.org/genai"

	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/llm/adapters"
	"github.com/ai-process/ktms/internal/pricing"
	"github.com/ai-process/ktms/internal/proxydb"
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
				adapter = adapters.NewTrackingAdapter(adapter, s.tracker, m.Vendor, m.ID).WithPricer(modelPricer(m))
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
		adapter = adapters.NewTrackingAdapter(adapter, s.tracker, m.Vendor, m.ID).WithPricer(modelPricer(m))
	}
	actual, _ := s.adapters.LoadOrStore(cacheKey, adapter)
	return actual.(llm.ClientAdapter), nil
}

// modelPricer prices usage events at the rate in force when the call started.
func modelPricer(m *proxydb.Model) adapters.Pricer {
	return func(usage llm.TokensUsage, startedAt time.Time) int64 {
		return pricing.Cost(m, usage, pricing.ModeAt(m, startedAt))
	}
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
	// FreeToken rejects response_format: the schema goes in the prompt. The
	// router checks the reply only when the model declares no_structured_output.
	a.SetSoftSchema(SoftSchema(m) || strings.EqualFold(engine, EngineFreeToken))
	a.SetSampling(SamplingSupport(m))
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
// no-ops: RPM and budgets are enforced in Redis, shared across replicas. SDK
// retries are off: the router retries and falls back, one call per attempt.
func buildAdapter(m *proxydb.Model, key string) (llm.ClientAdapter, error) {
	noThrottle := llm.NewThrottleControl(0, 60)
	switch m.Vendor {
	case VendorOpenAI, VendorDeepSeek, VendorOllama, VendorGroq, VendorMistral:
		opts := []option.RequestOption{option.WithAPIKey(key), option.WithHTTPClient(llm.VendorHTTPClient()), option.WithMaxRetries(0)}
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
			option.WithHTTPClient(llm.VendorHTTPClient()),
			option.WithMaxRetries(0),
		}
		client := openai.NewClient(opts...)
		adapter := adapters.NewOpenAIAdapter(&client, m.ID, noThrottle)
		ConfigureOpenAI(adapter, m, "")
		return adapter, nil
	case VendorGoogle:
		// A Gemini decision model answers on the Decisions API, which the genai
		// client does not speak; a chat model declaring judge still yields judgments.
		if IsGeminiDecisionModel(m) {
			return adapters.NewGeminiDecisionsAdapter(m.Endpoint, key, m.ID), nil
		}
		config := &genai.ClientConfig{APIKey: key, Backend: genai.BackendGeminiAPI, HTTPClient: llm.VendorHTTPClient()}
		if m.Endpoint != "" {
			config.HTTPOptions = genai.HTTPOptions{BaseURL: m.Endpoint}
		}
		client, err := genai.NewClient(context.Background(), config)
		if err != nil {
			return nil, fmt.Errorf("genai client: %w", err)
		}
		adapter := adapters.NewGoogleAdapter(client, m.ID, noThrottle)
		adapter.SetSoftSchema(SoftSchema(m))
		adapter.SetSampling(SamplingSupport(m))
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
		return adapters.NewTypeSafeAdapter(endpoint, key, m.ID).WithLongContext(HasCapability(m, CapabilityLongContext)), nil
	case VendorLiquid, VendorPerplexity, VendorMicrosoft, VendorCloudflare:
		return buildSystemOne(m, key)
	case VendorAnthropic:
		opts := []anthropicoption.RequestOption{anthropicoption.WithAPIKey(key), anthropicoption.WithHTTPClient(llm.VendorHTTPClient()), anthropicoption.WithMaxRetries(0)}
		endpoint := m.Endpoint
		if endpoint == "" {
			endpoint = AnthropicDefaultEndpoint
		}
		opts = append(opts, anthropicoption.WithBaseURL(endpoint))
		client := anthropic.NewClient(opts...)
		adapter := adapters.NewAnthropicAdapter(&client, m.ID, noThrottle)
		adapter.SetSoftSchema(SoftSchema(m))
		adapter.SetSampling(SamplingSupport(m))
		adapter.SetPromptCache(HasCapability(m, CapabilityPromptCache))
		return adapter, nil
	case VendorBedrock:
		parts := strings.Split(key, ":")
		if len(parts) < 2 {
			return nil, fmt.Errorf("bedrock credentials format: <access_key_id>:<secret_access_key>[:<region>[:<session_token>]]")
		}
		accessKey := parts[0]
		secretKey := parts[1]
		region := BedrockDefaultRegion
		sessionToken := ""
		if len(parts) >= 3 && parts[2] != "" {
			region = parts[2]
		}
		if len(parts) >= 4 {
			sessionToken = parts[3]
		}
		cfg, err := config.LoadDefaultConfig(context.Background(),
			config.WithRegion(region),
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, sessionToken)),
			config.WithRetryMaxAttempts(1),
		)
		if err != nil {
			return nil, fmt.Errorf("bedrock aws config: %w", err)
		}
		var optFns []func(*bedrockruntime.Options)
		if m.Endpoint != "" {
			optFns = append(optFns, func(o *bedrockruntime.Options) {
				o.BaseEndpoint = aws.String(m.Endpoint)
			})
		}
		client := bedrockruntime.NewFromConfig(cfg, optFns...)
		adapter := adapters.NewBedrockAdapter(client, m.ID, noThrottle)
		adapter.SetSoftSchema(SoftSchema(m))
		adapter.SetSampling(SamplingSupport(m))
		return adapter, nil
	default:
		return nil, fmt.Errorf("unknown vendor %q", m.Vendor)
	}
}

// IsGeminiDecisionModel reports a google model served by the Decisions API:
// a judge whose id names a decision model (gemini-decision-1).
func IsGeminiDecisionModel(m *proxydb.Model) bool {
	return m.Vendor == VendorGoogle && HasCapability(m, CapabilityJudge) && strings.HasPrefix(m.ID, "gemini-decision")
}

// buildSystemOne builds a decision vendor's System One adapter from its profile.
func buildSystemOne(m *proxydb.Model, key string) (llm.ClientAdapter, error) {
	endpoint := m.Endpoint
	var profile adapters.SystemOneProfile
	switch m.Vendor {
	case VendorLiquid:
		profile = adapters.LiquidProfile
		if endpoint == "" {
			endpoint = LiquidDefaultEndpoint
		}
	case VendorPerplexity:
		profile = adapters.PerplexityProfile
		if endpoint == "" {
			endpoint = PerplexityDefaultEndpoint
		}
	case VendorMicrosoft:
		profile = adapters.MicrosoftProfile
		if endpoint == "" {
			return nil, fmt.Errorf("microsoft models require an endpoint (the Foundry resource, e.g. https://<resource>.services.ai.azure.com)")
		}
	case VendorCloudflare:
		profile = adapters.CloudflareProfile
		if account, token, ok := strings.Cut(key, ":"); ok {
			if account == "" {
				return nil, fmt.Errorf("cloudflare key has no account id before the colon: want <account_id>:<api_token>")
			}
			if token == "" {
				return nil, fmt.Errorf("cloudflare key %q has no api token after the account id", account+":")
			}
			key = token
			if endpoint == "" {
				endpoint = fmt.Sprintf(CloudflareEndpointFormat, url.PathEscape(account))
			}
		}
		if endpoint == "" {
			return nil, fmt.Errorf("cloudflare models need a key of the form <account_id>:<api_token> or an endpoint naming the account")
		}
	default:
		return nil, fmt.Errorf("%s is not a System One vendor", m.Vendor)
	}
	return adapters.NewSystemOneAdapter(profile, endpoint, key, m.ID).WithLongContext(HasCapability(m, CapabilityLongContext)), nil
}
