package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/batch"
	"github.com/ai-process/ktms/internal/cache"
	"github.com/ai-process/ktms/internal/circuitbreaker"
	"github.com/ai-process/ktms/internal/config"
	"github.com/ai-process/ktms/internal/cooldown"
	"github.com/ai-process/ktms/internal/crypto"
	"github.com/ai-process/ktms/internal/grpcapi"
	"github.com/ai-process/ktms/internal/guardrails"
	"github.com/ai-process/ktms/internal/latent"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/logger"
	"github.com/ai-process/ktms/internal/nodes"
	"github.com/ai-process/ktms/internal/observability"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/redisguard"
	"github.com/ai-process/ktms/internal/rediskeys"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/router"
	"github.com/ai-process/ktms/internal/seed"
	"github.com/ai-process/ktms/internal/server"
	"github.com/ai-process/ktms/internal/throttle"
	"github.com/ai-process/ktms/internal/usagereport"
	"github.com/ai-process/ktms/migrations"
)

// initialLoadTimeout keeps a replica that cannot reach Postgres at boot from
// hanging: it exits and is restarted, instead of sitting there unready.
const initialLoadTimeout = 30 * time.Second

func main() {
	_ = godotenv.Load() // .env is absent in prod on purpose

	cfg := config.Load()
	logger.Init(logger.Options{
		Level: cfg.LogLevel, SentryDSN: cfg.SentryDSN, SentryEnvironment: cfg.SentryEnvironment,
		SentryRelease: cfg.SentryRelease, SentryTracing: cfg.SentryTracing,
		SentryDebug: cfg.SentryDebug, SentryBreadcrumbs: cfg.SentryBreadcrumbs,
	})
	for _, d := range cfg.Notices {
		log.Warn().Msg(d)
	}
	if cfg.SpendLimitsFailMode != "open" && cfg.SpendLimitsFailMode != "closed" {
		log.Fatal().Str("value", cfg.SpendLimitsFailMode).Msg(`SPEND_LIMITS_FAIL_MODE must be "open" or "closed"`)
	}

	shutdownTracing, err := observability.InitTracing(context.Background(), cfg.OTELServiceName, cfg.OTELExporterOTLPEndpoint)
	if err != nil {
		log.Warn().Err(err).Msg("failed to initialize OpenTelemetry tracing")
	} else {
		defer func() {
			shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutCancel()
			if err := shutdownTracing(shutCtx); err != nil {
				log.Warn().Err(err).Msg("failed to shutdown OpenTelemetry tracing")
			}
		}()
	}

	metricsSrv := observability.NewServer(cfg.MetricsAddr)
	if metricsSrv != nil {
		metricsSrv.Start()
		defer func() {
			shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutCancel()
			_ = metricsSrv.Shutdown(shutCtx)
		}()
	}

	if cfg.DatabaseURL == "" {
		log.Fatal().Msg("DATABASE_URL is required")
	}
	keyring, err := crypto.ParseKeyring(cfg.EncryptionKeys)
	if err != nil {
		log.Fatal().Err(err).Msg("KTMS_ENCRYPTION_KEYS is required and must parse")
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid DATABASE_URL")
	}
	poolCfg.MaxConns = cfg.DBMaxConns
	poolCfg.MinConns = cfg.DBMinConns
	if poolCfg.ConnConfig.ConnectTimeout == 0 && cfg.DBTimeout > 0 {
		poolCfg.ConnConfig.ConnectTimeout = cfg.DBTimeout // a DSN connect_timeout still wins
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create database pool")
	}
	defer pool.Close()

	// Before serving: a half-migrated schema is a state the queries below were
	// not written for, so a failure here keeps this replica out of the load
	// balancer and leaves the previous one serving.
	if cfg.AutoMigrate {
		migrateCtx, cancel := context.WithTimeout(context.Background(), cfg.MigrateTimeout)
		err := proxydb.Migrate(migrateCtx, pool, migrations.FS())
		cancel()
		if err != nil {
			log.Fatal().Err(err).Msg("applying migrations failed")
		}
	} else {
		log.Warn().Msg("AUTO_MIGRATE is off; the schema must be applied by hand")
	}

	if cfg.AutoSeed {
		seedCtx, seedCancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := seed.Run(seedCtx, pool, keyring, seed.Config{
			Enabled:      cfg.AutoSeed,
			ClientKey:    cfg.SeedClientKey,
			ClientName:   cfg.SeedClientName,
			OpenAIKey:    cfg.SeedOpenAIKey,
			GeminiKey:    cfg.SeedGeminiKey,
			AnthropicKey: cfg.SeedAnthropicKey,
		})
		seedCancel()
		if err != nil {
			log.Fatal().Err(err).Msg("applying auto-seed failed")
		}
	}

	db := proxydb.New(pool)
	verifier := apikeys.NewVerifier(db, cfg.BootstrapAdminKey).
		WithQueryTimeout(cfg.DBTimeout).
		WithStaleIfError(cfg.AuthStaleIfError)
	if cfg.BootstrapAdminKey != "" {
		log.Warn().Msg("bootstrap admin key is set; unset it once real keys are minted")
	}

	// Typed-nil guard: a nil *UsageCollectorClient must not become a non-nil
	// llm.UsageTracker interface, or every adapter would wrap a dead tracker.
	var tracker llm.UsageTracker
	usageClient, err := usagereport.NewUsageCollectorClient(cfg.UsageCollectorHost, cfg.UsageProjectID, cfg.UsageCollectorTLS, cfg.UsageCollectorToken)
	if err != nil {
		log.Warn().Err(err).Msg("usage collector unavailable; usage will not be reported")
	}
	if usageClient != nil {
		// Attribute spend to the calling service (its api-key name).
		usageClient.SetProjectResolver(grpcapi.UsageProject)
		defer func() { _ = usageClient.Close() }()
		tracker = usageClient
	}

	loader := registry.NewLoader(db, keyring, tracker)

	var (
		nodeMgr *nodes.Manager
		nodeReg *grpcapi.NodeRegistryServer
	)

	var (
		limiter     router.Throttle
		redisClient *redis.Client
	)
	if cfg.RedisConfigured() {
		redisClient = redis.NewClient(cfg.RedisOptions())
		redisClient.AddHook(redisguard.New())
		defer func() { _ = redisClient.Close() }()
		rLimiter := throttle.New(redisClient, cfg.RedisKeyPrefix)
		rLimiter.SetGraceMargin(cfg.BudgetGraceMargin)
		rLimiter.SetUserRateLimitResolver(func(userID, modelID string) *proxydb.UserRateLimit {
			return loader.Snapshot().UserRateLimit(userID, modelID)
		})
		rLimiter.SetSpendLimitResolver(func(subjectType, subjectID, period string) int64 {
			return loader.Snapshot().SpendLimit(subjectType, subjectID, period)
		})
		rLimiter.SetSpendFailClosed(cfg.SpendFailClosed())
		rLimiter.SetDefaultOutputReserve(int64(cfg.SpendDefaultOutputReserve))
		limiter = rLimiter
		log.Info().Str("addr", cfg.RedisAddr).Msg("redis throttling enabled")

		rkeys := rediskeys.New(cfg.RedisKeyPrefix)
		nStore := nodes.NewStore(redisClient, rkeys)
		nSlots := nodes.NewLeaseManager(redisClient, nStore)
		nCircuit := nodes.NewCircuitBreaker(redisClient, nStore, 5, 30*time.Second)
		nTelemetry := nodes.NewTelemetryTracker(redisClient, nStore, 0.2, 24*time.Hour)
		nodeMgr = nodes.NewManager(nStore, nSlots, nCircuit, nTelemetry, nodes.Config{
			HeartbeatInterval: cfg.NodeHeartbeatInterval,
			HeartbeatTTL:      cfg.NodeHeartbeatTTL,
			RetentionTTL:      cfg.NodeRetentionTTL,
			AllowLocalhost:    cfg.NodeAllowLocalhost,
		})
		nodeMgr.Start(context.Background())
		defer nodeMgr.Stop()

		nSelector := nodes.NewSelector(nodeMgr)
		poolResolver := nodes.NewPoolResolver(nSelector, keyring, 60*time.Second)
		poolResolver.SetUserRateLimitResolver(func(userID, modelID string) *proxydb.UserRateLimit {
			return loader.Snapshot().UserRateLimit(userID, modelID)
		})
		loader.SetNodePool(poolResolver)

		nodeReg = grpcapi.NewNodeRegistryServer(nodeMgr, keyring)
	} else {
		log.Warn().Msg("REDIS_ADDR unset: rpm, token budgets, key rates and spend limits are NOT enforced, local node pool is disabled")
	}

	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()
	loadCtx, loadCancel := context.WithTimeout(rootCtx, initialLoadTimeout)
	err = loader.Load(loadCtx)
	loadCancel()
	if err != nil {
		log.Fatal().Err(err).Msg("initial routing config load failed")
	}
	loader.Start(rootCtx, cfg.RoutingReload)

	adminSrv := grpcapi.NewAdminServer(db, keyring, loader.Refresh)
	if rl, ok := limiter.(*throttle.Redis); ok {
		adminSrv.SetSpendReader(rl.SpendSoFar)
		adminSrv.SetSpendLimitResolver(func(subjectType, subjectID, period string) int64 {
			return loader.Snapshot().SpendLimit(subjectType, subjectID, period)
		})
	}
	if nodeMgr != nil {
		adminSrv.SetNodeManager(nodeMgr)
	}
	cb := circuitbreaker.NewManager(circuitbreaker.Config{
		Enabled:       cfg.CircuitBreakerEnabled,
		FailThreshold: cfg.CircuitBreakerFailThreshold,
		Cooldown:      cfg.CircuitBreakerCooldown,
	})
	proxySrv := grpcapi.NewProxyServer(loader, limiter).
		WithCircuitBreaker(cb).
		WithCooldown(cooldown.New(cfg.VendorCooldownMax)).
		WithUpstreamTimeout(cfg.UpstreamRequestTimeout).
		WithBatch(db, cfg.BatchMaxItems)
	if rl, ok := limiter.(*throttle.Redis); ok {
		proxySrv.WithKeyLimiter(rl)
	}
	if cfg.CacheEnabled && redisClient != nil {
		c := cache.New(redisClient, keyring, cfg.CacheTTL, cfg.CacheMaxBytes, cfg.RedisKeyPrefix)
		proxySrv.WithCache(c)
		log.Info().Dur("ttl", cfg.CacheTTL).Int("max_bytes", cfg.CacheMaxBytes).Msg("response caching enabled")
	}
	var encoder latent.Encoder
	if cfg.LatentEncoderNeeded() {
		encoder = newLatentEncoder(cfg)
	}
	if cfg.SemanticCacheEnabled {
		if redisClient == nil {
			log.Warn().Msg("SEMANTIC_CACHE_ENABLED needs REDIS_ADDR: semantic cache disabled")
		} else {
			sc := cache.NewSemantic(redisClient, keyring, encoder, cache.SemanticOptions{
				Threshold:  cfg.SemanticCacheThreshold,
				MaxEntries: cfg.SemanticCacheMaxEntries,
				TTL:        cfg.CacheTTL,
				MaxBytes:   cfg.CacheMaxBytes,
				Prefix:     cfg.RedisKeyPrefix,
			})
			proxySrv.WithSemanticCache(sc)
			log.Info().Float64("threshold", cfg.SemanticCacheThreshold).Int("max_entries", cfg.SemanticCacheMaxEntries).Msg("semantic caching enabled")
		}
	}
	if cfg.GuardrailsEnabled {
		g := guardrails.New(cfg.GuardrailsEnabled, cfg.GuardrailsPIIAction, cfg.GuardrailsPromptInjectionAction)
		if cfg.GuardrailsLatentEnabled {
			g.WithLatent(newLatentDetector(rootCtx, cfg, encoder), cfg.GuardrailsLatentAction)
		}
		proxySrv.WithGuardrails(g)
		log.Info().Str("pii_action", cfg.GuardrailsPIIAction).Str("prompt_injection_action", cfg.GuardrailsPromptInjectionAction).Bool("latent", cfg.GuardrailsLatentEnabled).Msg("guardrails enabled")
	}

	batchPoller := batch.NewPoller(db, loader, limiter, tracker, cfg.BatchPollInterval, cfg.ResultsRetention)
	batchPoller.Start(rootCtx)
	defer batchPoller.Stop()

	var nodeRegServer pb.NodeRegistryServiceServer
	if nodeReg != nil {
		nodeRegServer = nodeReg
	}
	grpcSrv := server.NewGRPCServer(proxySrv, adminSrv, nodeRegServer, verifier, cfg.GRPCAddr, cfg.GRPCMaxRecvMsgSize)
	var openaiAPI *server.OpenAIAPI
	if cfg.HTTPAPIEnabled {
		openaiAPI = server.NewOpenAIAPI(proxySrv, verifier)
	}
	httpSrv := server.NewHTTPServer(pool, cfg.HTTPAddr, openaiAPI, cfg.HTTPAPITimeout).
		WithDegradedReadiness(cfg.AuthStaleIfError)

	grpcErrCh := make(chan error, 1)
	httpErrCh := make(chan error, 1)
	go func() { grpcErrCh <- grpcSrv.Start() }()
	go func() { httpErrCh <- httpSrv.Start() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		log.Info().Str("signal", sig.String()).Msg("shutting down")
	case err := <-grpcErrCh:
		log.Error().Err(err).Msg("gRPC server failed")
	case err := <-httpErrCh:
		log.Error().Err(err).Msg("HTTP server failed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	grpcSrv.Stop()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Error().Err(err).Msg("HTTP shutdown error")
	}
}

// newLatentEncoder memoizes the configured encoder; a remote one falls back to
// the local hash encoder so an encoder outage degrades instead of failing.
func newLatentEncoder(cfg *config.Config) latent.Encoder {
	local := latent.NewHashEncoder(cfg.LatentLocalEncoderDims)
	var enc latent.Encoder = local
	if cfg.LatentEncoderURL != "" {
		remote := latent.NewHTTPEncoder(cfg.LatentEncoderURL, cfg.LatentEncoderModel, cfg.LatentEncoderAPIKey, cfg.LatentEncoderTimeout, &http.Client{})
		enc = latent.NewFallback(remote, local)
		log.Info().Str("url", cfg.LatentEncoderURL).Str("model", cfg.LatentEncoderModel).Dur("timeout", cfg.LatentEncoderTimeout).Msg("latent encoder configured")
	} else {
		log.Info().Str("model", local.Model()).Msg("latent encoder: local hash encoder")
	}
	return latent.NewMemo(enc, cfg.LatentEncoderCacheSize)
}

func newLatentDetector(ctx context.Context, cfg *config.Config, enc latent.Encoder) *guardrails.LatentDetector {
	cats := guardrails.DefaultLatentCategories()
	if cfg.GuardrailsLatentCategoriesFile != "" {
		extra, err := guardrails.LoadLatentCategories(cfg.GuardrailsLatentCategoriesFile)
		if err != nil {
			log.Fatal().Err(err).Msg("latent guardrail categories")
		}
		cats = append(cats, extra...)
	}
	d, err := guardrails.NewLatentDetector(enc, cfg.GuardrailsLatentThreshold, cats)
	if err != nil {
		log.Fatal().Err(err).Msg("latent guardrail")
	}
	warmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := d.Warmup(warmCtx); err != nil {
		log.Warn().Err(err).Msg("latent guardrail warmup failed; centroids compile on first request")
	}
	return d
}
