package logger

import (
	"io"
	"os"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	sentryzerolog "github.com/getsentry/sentry-go/zerolog"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Init initializes the global logger with proper configuration
func Init() {
	zerolog.TimeFieldFormat = time.RFC3339

	logLevel := os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		logLevel = "info"
	}

	var level zerolog.Level
	switch strings.ToLower(logLevel) {
	case "debug":
		level = zerolog.DebugLevel
	case "info":
		level = zerolog.InfoLevel
	case "warn":
		level = zerolog.WarnLevel
	case "error":
		level = zerolog.ErrorLevel
	default:
		level = zerolog.InfoLevel
	}

	zerolog.SetGlobalLevel(level)

	var writers []io.Writer

	consoleWriter := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: time.RFC3339,
	}
	writers = append(writers, consoleWriter)

	// Initialize Sentry if DSN is provided
	var sentryWriter *sentryzerolog.Writer
	if sentryDSN := os.Getenv("SENTRY_DSN"); sentryDSN != "" {
		err := sentry.Init(sentry.ClientOptions{
			Dsn:              sentryDSN,
			Debug:            os.Getenv("SENTRY_DEBUG") == "true",
			EnableTracing:    os.Getenv("SENTRY_ENABLE_TRACING") == "true",
			TracesSampleRate: 1.0,
			Environment:      envOr("SENTRY_ENVIRONMENT", "development"),
			Release:          os.Getenv("SENTRY_RELEASE"),
		})
		if err != nil {
			log.Fatal().Err(err).Msg("sentry.Init failed")
		}

		sentryWriter, err = sentryzerolog.New(sentryzerolog.Config{
			ClientOptions: sentry.ClientOptions{
				Dsn: sentryDSN,
			},
			Options: sentryzerolog.Options{
				Levels:          []zerolog.Level{zerolog.ErrorLevel, zerolog.FatalLevel, zerolog.PanicLevel},
				WithBreadcrumbs: os.Getenv("SENTRY_WITH_BREADCRUMBS") == "true",
				FlushTimeout:    3 * time.Second,
			},
		})
		if err != nil {
			log.Fatal().Err(err).Msg("failed to create sentry writer")
		}

		log.Info().Msg("Sentry integration enabled")
	}

	if sentryWriter != nil {
		writers = append(writers, sentryWriter)
	}

	log.Logger = log.Output(zerolog.MultiLevelWriter(writers...))

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	log.Logger = log.Logger.With().Str("host", hostname).Str("service", "ktms").Logger()

	zerolog.DefaultContextLogger = &log.Logger
}

// envOr applies the documented default; Init runs before config.Load.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
