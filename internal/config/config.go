package config

import (
	"os"
	"strconv"
	"time"
)

// Config holds the application configuration. Load never fails; main.go
// fatals on missing required values.
type Config struct {
	DatabaseURL string
	// EncryptionKeys is the versioned AES-256-GCM keyring spec
	// ("1:<base64 32B>,2:..."); vendor keys are unreadable without it.
	EncryptionKeys     string
	GRPCAddr           string
	GRPCMaxRecvMsgSize int
	HTTPAddr           string
	// HTTPAPIEnabled mounts the OpenAI-compatible API on HTTPAddr; off leaves health only.
	HTTPAPIEnabled bool
	// HTTPAPITimeout bounds one HTTP API call, which waits on an upstream model.
	HTTPAPITimeout  time.Duration
	LogLevel        string
	ShutdownTimeout time.Duration
	DBMaxConns      int32
	DBMinConns      int32

	// RoutingReload is how often replicas poll config_state for changes.
	RoutingReload time.Duration

	// AutoMigrate applies the embedded migrations at startup. On by default;
	// a half-migrated schema is a state the queries were not written for.
	AutoMigrate bool
	// MigrateTimeout bounds the whole run, including the wait for whichever
	// replica holds the advisory lock during a rolling deploy.
	MigrateTimeout time.Duration

	// AutoSeed populates initial models, rules, and dev keys when unconfigured.
	AutoSeed         bool
	SeedClientKey    string
	SeedClientName   string
	SeedOpenAIKey    string
	SeedGeminiKey    string
	SeedAnthropicKey string

	RedisAddr      string
	RedisPassword  string
	RedisDB        int
	RedisKeyPrefix string

	// BootstrapAdminKey solves minting the first api key; unset after use.
	BootstrapAdminKey string

	UsageCollectorHost string
	// UsageCollectorTLS reaches the usage sink over TLS, as a managed
	// platform (Cloud Run) serves it; plaintext otherwise.
	UsageCollectorTLS bool
	UsageProjectID    string

	BatchMaxItems     int
	BatchPollInterval time.Duration
	ResultsRetention  time.Duration

	// Local Node Pool configuration
	NodeHeartbeatInterval time.Duration
	NodeHeartbeatTTL      time.Duration
	NodeRetentionTTL      time.Duration
	NodeAllowLocalhost    bool

	BudgetGraceMargin float64

	// SpendLimitsFailMode is "open" (Redis down lets calls through unmetered)
	// or "closed" (calls under a spend limit are refused until it is back).
	SpendLimitsFailMode string
	// SpendDefaultOutputReserve is the output tokens priced into a spend
	// reservation when the call sets no max tokens.
	SpendDefaultOutputReserve int

	// Upstream vendor Circuit Breaker settings
	CircuitBreakerEnabled       bool
	CircuitBreakerFailThreshold int
	CircuitBreakerCooldown      time.Duration
	// UpstreamRequestTimeout bounds each attempt against an upstream vendor.
	// Stalls beyond this duration trip circuit breakers and advance fallback chains.
	UpstreamRequestTimeout time.Duration

	MetricsAddr              string
	OTELServiceName          string
	OTELExporterOTLPEndpoint string

	CacheEnabled  bool
	CacheTTL      time.Duration
	CacheMaxBytes int

	// SemanticCache serves near-duplicate prompts from Redis by latent
	// similarity; it shares CACHE_TTL and CACHE_MAX_BYTES with the exact cache.
	SemanticCacheEnabled    bool
	SemanticCacheThreshold  float64
	SemanticCacheMaxEntries int

	GuardrailsEnabled               bool
	GuardrailsPIIAction             string
	GuardrailsPromptInjectionAction string
	// GuardrailsLatent* screen prompts against malicious centroids in
	// representation space; the categories file extends the built-in set.
	GuardrailsLatentEnabled        bool
	GuardrailsLatentThreshold      float64
	GuardrailsLatentAction         string
	GuardrailsLatentCategoriesFile string

	// LatentEncoderURL is an OpenAI-compatible /v1/embeddings endpoint (e.g. a
	// JEPA node); empty or unreachable falls back to the local hash encoder.
	LatentEncoderURL       string
	LatentEncoderModel     string
	LatentEncoderAPIKey    string
	LatentEncoderTimeout   time.Duration
	LatentLocalEncoderDims int
	LatentEncoderCacheSize int

	SentryDSN         string
	SentryEnvironment string
	SentryRelease     string
	SentryTracing     bool
}

// Load loads configuration from environment variables
func Load() *Config {
	return &Config{
		DatabaseURL:                     getEnv("DATABASE_URL", ""),
		EncryptionKeys:                  getEnv("LLMPROXY_ENCRYPTION_KEYS", ""),
		GRPCAddr:                        getEnv("GRPC_ADDR", ":9090"),
		GRPCMaxRecvMsgSize:              getIntEnv("GRPC_MAX_RECV_MSG_SIZE", 16*1024*1024),
		HTTPAddr:                        getEnv("HTTP_ADDR", ":8080"),
		HTTPAPIEnabled:                  getBoolEnv("HTTP_API_ENABLED", true),
		HTTPAPITimeout:                  getDurationEnv("HTTP_API_TIMEOUT", 5*time.Minute),
		LogLevel:                        getEnv("LOG_LEVEL", "info"),
		ShutdownTimeout:                 getDurationEnv("SHUTDOWN_TIMEOUT", 30*time.Second),
		DBMaxConns:                      int32(getIntEnv("DB_MAX_CONNS", 25)),
		DBMinConns:                      int32(getIntEnv("DB_MIN_CONNS", 5)),
		RoutingReload:                   time.Duration(getIntEnv("ROUTING_RELOAD_SECONDS", 60)) * time.Second,
		AutoMigrate:                     getBoolEnv("AUTO_MIGRATE", true),
		MigrateTimeout:                  getDurationEnv("MIGRATE_TIMEOUT", 3*time.Minute),
		AutoSeed:                        getBoolEnv("AUTO_SEED", false),
		SeedClientKey:                   getEnv("SEED_CLIENT_KEY", ""),
		SeedClientName:                  getEnv("SEED_CLIENT_NAME", "dev-default"),
		SeedOpenAIKey:                   getEnv("SEED_OPENAI_KEY", getEnv("OPENAI_API_KEY", "")),
		SeedGeminiKey:                   getEnv("SEED_GEMINI_KEY", getEnv("GEMINI_API_KEY", "")),
		SeedAnthropicKey:                getEnv("SEED_ANTHROPIC_KEY", getEnv("ANTHROPIC_API_KEY", "")),
		RedisAddr:                       getEnv("REDIS_ADDR", ""),
		RedisPassword:                   getEnv("REDIS_PASSWORD", ""),
		RedisDB:                         getIntEnv("REDIS_DB", 0),
		RedisKeyPrefix:                  getEnv("REDIS_KEY_PREFIX", ""),
		BootstrapAdminKey:               getEnv("BOOTSTRAP_ADMIN_KEY", ""),
		UsageCollectorHost:              getEnv("USAGE_COLLECTOR_HOST", ""),
		UsageCollectorTLS:               getBoolEnv("USAGE_COLLECTOR_TLS", false),
		UsageProjectID:                  getEnv("USAGE_PROJECT_ID", "llm-proxy"),
		BatchMaxItems:                   getIntEnv("BATCH_MAX_ITEMS", 10000),
		BatchPollInterval:               time.Duration(getIntEnv("BATCH_POLL_SECONDS", 60)) * time.Second,
		ResultsRetention:                getRetentionEnv("RESULTS_RETENTION_DAYS", 7*24*time.Hour),
		NodeHeartbeatInterval:           time.Duration(getIntEnv("NODE_HEARTBEAT_INTERVAL_SECONDS", 5)) * time.Second,
		NodeHeartbeatTTL:                time.Duration(getIntEnv("NODE_HEARTBEAT_TTL_SECONDS", 15)) * time.Second,
		NodeRetentionTTL:                getRetentionEnv("NODE_RETENTION_TTL_DAYS", 24*time.Hour),
		NodeAllowLocalhost:              getBoolEnv("NODE_ALLOW_LOCALHOST_ENDPOINTS", false),
		BudgetGraceMargin:               getFloatEnv("BUDGET_GRACE_MARGIN", 0.05),
		SpendLimitsFailMode:             getEnv("SPEND_LIMITS_FAIL_MODE", "open"),
		SpendDefaultOutputReserve:       getIntEnv("SPEND_DEFAULT_OUTPUT_RESERVE_TOKENS", 4096),
		CircuitBreakerEnabled:           getBoolEnv("CIRCUIT_BREAKER_ENABLED", true),
		CircuitBreakerFailThreshold:     getIntEnv("CIRCUIT_BREAKER_FAIL_THRESHOLD", 5),
		CircuitBreakerCooldown:          getDurationEnv("CIRCUIT_BREAKER_COOLDOWN", 30*time.Second),
		UpstreamRequestTimeout:          getDurationEnv("UPSTREAM_REQUEST_TIMEOUT", 0),
		MetricsAddr:                     getEnv("METRICS_ADDR", ":9091"),
		OTELServiceName:                 getEnv("OTEL_SERVICE_NAME", "llm-proxy"),
		OTELExporterOTLPEndpoint:        getEnv("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		CacheEnabled:                    getBoolEnv("CACHE_ENABLED", false),
		CacheTTL:                        getDurationEnv("CACHE_TTL", 1*time.Hour),
		CacheMaxBytes:                   getIntEnv("CACHE_MAX_BYTES", 1048576),
		GuardrailsEnabled:               getBoolEnv("GUARDRAILS_ENABLED", false),
		GuardrailsPIIAction:             getEnv("GUARDRAILS_PII_ACTION", "block"),
		GuardrailsPromptInjectionAction: getEnv("GUARDRAILS_PROMPT_INJECTION_ACTION", "block"),
		GuardrailsLatentEnabled:         getBoolEnv("GUARDRAILS_LATENT_ENABLED", false),
		GuardrailsLatentThreshold:       getFloatEnv("GUARDRAILS_LATENT_THRESHOLD", 0.70),
		GuardrailsLatentAction:          getEnv("GUARDRAILS_LATENT_ACTION", "block"),
		GuardrailsLatentCategoriesFile:  getEnv("GUARDRAILS_LATENT_CATEGORIES_FILE", ""),
		SemanticCacheEnabled:            getBoolEnv("SEMANTIC_CACHE_ENABLED", false),
		SemanticCacheThreshold:          getFloatEnv("SEMANTIC_CACHE_SIMILARITY_THRESHOLD", 0.95),
		SemanticCacheMaxEntries:         getIntEnv("SEMANTIC_CACHE_MAX_ENTRIES", 256),
		LatentEncoderURL:                getEnv("LATENT_ENCODER_URL", ""),
		LatentEncoderModel:              getEnv("LATENT_ENCODER_MODEL", "jepa-text"),
		LatentEncoderAPIKey:             getEnv("LATENT_ENCODER_API_KEY", ""),
		LatentEncoderTimeout:            getDurationEnv("LATENT_ENCODER_TIMEOUT", 50*time.Millisecond),
		LatentLocalEncoderDims:          getIntEnv("LATENT_LOCAL_ENCODER_DIMS", 512),
		LatentEncoderCacheSize:          getIntEnv("LATENT_ENCODER_CACHE_SIZE", 4096),
		SentryDSN:                       getEnv("SENTRY_DSN", ""),
		SentryEnvironment:               getEnv("SENTRY_ENVIRONMENT", "development"),
		SentryRelease:                   getEnv("SENTRY_RELEASE", ""),
		SentryTracing:                   getEnv("SENTRY_ENABLE_TRACING", "false") == "true",
	}
}

// RedisConfigured reports whether throttling has a backing store; without it
// all requests are allowed (fail-open by design).
func (c *Config) RedisConfigured() bool { return c.RedisAddr != "" }

// SpendFailClosed reports whether spend limits refuse calls while their
// counters are unreachable.
func (c *Config) SpendFailClosed() bool { return c.SpendLimitsFailMode == "closed" }

// LatentEncoderNeeded reports whether any feature consumes latent embeddings.
func (c *Config) LatentEncoderNeeded() bool {
	return c.SemanticCacheEnabled || (c.GuardrailsEnabled && c.GuardrailsLatentEnabled)
}

// UsageConfigured reports whether usage events are forwarded anywhere.
func (c *Config) UsageConfigured() bool { return c.UsageCollectorHost != "" }

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getBoolEnv(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if b, err := strconv.ParseBool(value); err == nil {
			return b
		}
	}
	return defaultValue
}

func getIntEnv(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if i, err := strconv.Atoi(value); err == nil {
			return i
		}
	}
	return defaultValue
}

func getFloatEnv(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			return f
		}
	}
	return defaultValue
}

func getDurationEnv(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if d, err := time.ParseDuration(value); err == nil {
			return d
		}
	}
	return defaultValue
}

func getRetentionEnv(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if d, err := time.ParseDuration(value); err == nil {
			return d
		}
		// Try parsing as integer days (e.g. "7")
		if days, err := strconv.Atoi(value); err == nil && days > 0 {
			return time.Duration(days) * 24 * time.Hour
		}
	}
	return defaultValue
}
