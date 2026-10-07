package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/llm-proxy/internal/apikeys"
	"github.com/ai-process/llm-proxy/internal/config"
	"github.com/ai-process/llm-proxy/internal/crypto"
	"github.com/ai-process/llm-proxy/internal/grpcapi"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/logger"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
	"github.com/ai-process/llm-proxy/internal/router"
	"github.com/ai-process/llm-proxy/internal/server"
	"github.com/ai-process/llm-proxy/internal/throttle"
	"github.com/ai-process/llm-proxy/internal/usagereport"
	"github.com/ai-process/llm-proxy/migrations"
)

func main() {
	_ = godotenv.Load() // .env is absent in prod on purpose

	logger.Init()
	cfg := config.Load()

	if cfg.DatabaseURL == "" {
		log.Fatal().Msg("DATABASE_URL is required")
	}
	keyring, err := crypto.ParseKeyring(cfg.EncryptionKeys)
	if err != nil {
		log.Fatal().Err(err).Msg("LLMPROXY_ENCRYPTION_KEYS is required and must parse")
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid DATABASE_URL")
	}
	poolCfg.MaxConns = cfg.DBMaxConns
	poolCfg.MinConns = cfg.DBMinConns
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

	db := proxydb.New(pool)
	verifier := apikeys.NewVerifier(db, cfg.BootstrapAdminKey)
	if cfg.BootstrapAdminKey != "" {
		log.Warn().Msg("bootstrap admin key is set; unset it once real keys are minted")
	}

	var limiter router.Throttle
	if cfg.RedisConfigured() {
		redisClient := redis.NewClient(&redis.Options{
			Addr:     cfg.RedisAddr,
			Password: cfg.RedisPassword,
			DB:       cfg.RedisDB,
		})
		defer func() { _ = redisClient.Close() }()
		limiter = throttle.New(redisClient, cfg.RedisKeyPrefix)
		log.Info().Str("addr", cfg.RedisAddr).Msg("redis throttling enabled")
	} else {
		log.Warn().Msg("REDIS_ADDR unset: rpm and token budgets are NOT enforced")
	}

	// Typed-nil guard: a nil *UsageCollectorClient must not become a non-nil
	// llm.UsageTracker interface, or every adapter would wrap a dead tracker.
	var tracker llm.UsageTracker
	usageClient, err := usagereport.NewUsageCollectorClient(cfg.UsageCollectorHost, cfg.UsageProjectID)
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
	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()
	if err := loader.Load(rootCtx); err != nil {
		log.Fatal().Err(err).Msg("initial routing config load failed")
	}
	loader.Start(rootCtx, cfg.RoutingReload)

	adminSrv := grpcapi.NewAdminServer(db, keyring, loader.Refresh)
	proxySrv := grpcapi.NewProxyServer(loader, limiter)

	grpcSrv := server.NewGRPCServer(proxySrv, adminSrv, verifier, cfg.GRPCAddr)
	var openaiAPI *server.OpenAIAPI
	if cfg.HTTPAPIEnabled {
		openaiAPI = server.NewOpenAIAPI(proxySrv, verifier)
	}
	httpSrv := server.NewHTTPServer(pool, cfg.HTTPAddr, openaiAPI, cfg.HTTPAPITimeout)

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
