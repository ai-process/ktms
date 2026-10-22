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

// Options come from config.Load, the one place settings and their defaults
// are read.
type Options struct {
	Level             string
	SentryDSN         string
	SentryEnvironment string
	SentryRelease     string
	SentryTracing     bool
	SentryDebug       bool
	SentryBreadcrumbs bool
}

// Init initializes the global logger with proper configuration
func Init(o Options) {
	zerolog.TimeFieldFormat = time.RFC3339

	logLevel := o.Level
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
	if sentryDSN := o.SentryDSN; sentryDSN != "" {
		err := sentry.Init(sentry.ClientOptions{
			Dsn:              sentryDSN,
			Debug:            o.SentryDebug,
			EnableTracing:    o.SentryTracing,
			TracesSampleRate: 1.0,
			Environment:      o.SentryEnvironment,
			Release:          o.SentryRelease,
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
				WithBreadcrumbs: o.SentryBreadcrumbs,
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
