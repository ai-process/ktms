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
	EncryptionKeys string
	GRPCAddr       string
	HTTPAddr       string
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

	RedisAddr      string
	RedisPassword  string
	RedisDB        int
	RedisKeyPrefix string

	// BootstrapAdminKey solves minting the first api key; unset after use.
	BootstrapAdminKey string

	UsageCollectorHost string
	UsageProjectID     string

	BatchMaxItems     int
	BatchPollInterval time.Duration
	ResultsRetention  time.Duration

	SentryDSN         string
	SentryEnvironment string
	SentryRelease     string
	SentryTracing     bool
}

// Load loads configuration from environment variables
func Load() *Config {
	return &Config{
		DatabaseURL:        getEnv("DATABASE_URL", ""),
		EncryptionKeys:     getEnv("LLMPROXY_ENCRYPTION_KEYS", ""),
		GRPCAddr:           getEnv("GRPC_ADDR", ":9090"),
		HTTPAddr:           getEnv("HTTP_ADDR", ":8080"),
		HTTPAPIEnabled:     getBoolEnv("HTTP_API_ENABLED", true),
		HTTPAPITimeout:     getDurationEnv("HTTP_API_TIMEOUT", 5*time.Minute),
		LogLevel:           getEnv("LOG_LEVEL", "info"),
		ShutdownTimeout:    getDurationEnv("SHUTDOWN_TIMEOUT", 30*time.Second),
		DBMaxConns:         int32(getIntEnv("DB_MAX_CONNS", 25)),
		DBMinConns:         int32(getIntEnv("DB_MIN_CONNS", 5)),
		RoutingReload:      time.Duration(getIntEnv("ROUTING_RELOAD_SECONDS", 60)) * time.Second,
		AutoMigrate:        getBoolEnv("AUTO_MIGRATE", true),
		MigrateTimeout:     getDurationEnv("MIGRATE_TIMEOUT", 3*time.Minute),
		RedisAddr:          getEnv("REDIS_ADDR", ""),
		RedisPassword:      getEnv("REDIS_PASSWORD", ""),
		RedisDB:            getIntEnv("REDIS_DB", 0),
		RedisKeyPrefix:     getEnv("REDIS_KEY_PREFIX", ""),
		BootstrapAdminKey:  getEnv("BOOTSTRAP_ADMIN_KEY", ""),
		UsageCollectorHost: getEnv("USAGE_COLLECTOR_HOST", ""),
		UsageProjectID:     getEnv("USAGE_PROJECT_ID", "llm-proxy"),
		BatchMaxItems:      getIntEnv("BATCH_MAX_ITEMS", 10000),
		BatchPollInterval:  time.Duration(getIntEnv("BATCH_POLL_SECONDS", 60)) * time.Second,
		ResultsRetention:   getRetentionEnv("RESULTS_RETENTION_DAYS", 7*24*time.Hour),
		SentryDSN:          getEnv("SENTRY_DSN", ""),
		SentryEnvironment:  getEnv("SENTRY_ENVIRONMENT", "development"),
		SentryRelease:      getEnv("SENTRY_RELEASE", ""),
		SentryTracing:      getEnv("SENTRY_ENABLE_TRACING", "false") == "true",
	}
}

// RedisConfigured reports whether throttling has a backing store; without it
// all requests are allowed (fail-open by design).
func (c *Config) RedisConfigured() bool { return c.RedisAddr != "" }

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
