package usagereport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/gofrs/uuid"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"

	usagev1 "github.com/ai-process/llm-proxy/internal/usagereport/gen/usage/v1"
)

// UsageCollectorClient is a client for sending LLM usage events to the usage collector service
// It implements llm.UsageTracker interface
type UsageCollectorClient struct {
	client    usagev1.UsageServiceClient
	conn      *grpc.ClientConn
	projectID string
	// projectFor resolves the project from event meta (the proxy attributes
	// spend to the calling service, not to itself). Empty result falls back
	// to projectID.
	projectFor func(meta map[string]string) string
}

// SetProjectResolver installs a per-event project resolver.
func (c *UsageCollectorClient) SetProjectResolver(fn func(meta map[string]string) string) {
	if c == nil {
		return
	}
	c.projectFor = fn
}

// NewUsageCollectorClient creates a new usage collector client
// If host is empty, returns nil (usage collection disabled)
// projectID is the default project events are attributed to
func NewUsageCollectorClient(host, projectID string, useTLS bool) (*UsageCollectorClient, error) {
	if host == "" {
		return nil, nil
	}

	// Plaintext on a private network; TLS for a sink behind a managed front.
	creds := insecure.NewCredentials()
	if useTLS {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	conn, err := grpc.NewClient(host, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, err
	}

	client := usagev1.NewUsageServiceClient(conn)

	return &UsageCollectorClient{
		client:    client,
		conn:      conn,
		projectID: projectID,
	}, nil
}

// SetProjectID sets the project ID (App UUID) for usage tracking
func (c *UsageCollectorClient) SetProjectID(projectID string) {
	if c == nil {
		return
	}
	c.projectID = projectID
}

// Close closes the gRPC connection
func (c *UsageCollectorClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// RecordUsage sends a usage event asynchronously to the collector
// Returns an error if validation fails. The actual gRPC call happens asynchronously
// and errors from the gRPC call are logged but not returned.
func (c *UsageCollectorClient) RecordUsage(
	userID string,
	actionID string,
	vendor string,
	model string,
	inputTokens int,
	outputTokens int,
	durationMs int64,
	status usagev1.Status,
	meta map[string]string,
) error {
	if c == nil {
		return errors.New("usage collector client is nil")
	}

	// Validate required parameters
	if userID == "" {
		return fmt.Errorf("userID is required but not set (actionID: %s, vendor: %s, model: %s)", actionID, vendor, model)
	}

	if actionID == "" {
		return fmt.Errorf("actionID is required but not set (userID: %s, vendor: %s, model: %s)", userID, vendor, model)
	}

	projectID := c.projectID
	if c.projectFor != nil {
		if p := c.projectFor(meta); p != "" {
			projectID = p
		}
	}
	if projectID == "" {
		return fmt.Errorf("projectID is required but not set (userID: %s, actionID: %s, vendor: %s, model: %s)", userID, actionID, vendor, model)
	}

	// Send asynchronously in a goroutine
	go func() {
		eventID := uuid.Must(uuid.NewV7()).String()
		now := timestamppb.Now()

		event := &usagev1.UsageEvent{
			EventId:      eventID,
			OccurredAt:   now,
			UserId:       userID,
			ProjectId:    projectID,
			ActionId:     actionID,
			Vendor:       vendor,
			Model:        model,
			Status:       status,
			DurationMs:   durationMs,
			TokensInput:  int64(inputTokens),
			TokensOutput: int64(outputTokens),
			TokensTotal:  int64(inputTokens + outputTokens),
			Meta:         meta,
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, err := c.client.RecordUsageEvent(ctx, &usagev1.RecordUsageEventRequest{
			Event: event,
		})

		if err != nil {
			log.Error().Stack().Err(err).Str("userID", userID).
				Str("actionID", actionID).Str("vendor", vendor).Str("model", model).
				Msg("Failed to send usage event to collector")
		}
	}()

	return nil
}

// GetUsageByMeta returns usage aggregated over events carrying the given meta
// key/value (e.g. one batch job via "job_id"), split by groupByMetaKey
// (e.g. "gentask_id") plus vendor and model. Scoped to this client's project.
func (c *UsageCollectorClient) GetUsageByMeta(ctx context.Context, metaKey, metaValue, groupByMetaKey string) ([]*usagev1.UsageByMetaRow, error) {
	if c == nil {
		return nil, errors.New("usage collector is not configured")
	}
	resp, err := c.client.GetUsageByMeta(ctx, &usagev1.GetUsageByMetaRequest{
		MetaKey:        metaKey,
		MetaValue:      metaValue,
		GroupByMetaKey: groupByMetaKey,
		ProjectId:      c.projectID,
	})
	if err != nil {
		return nil, err
	}
	return resp.Rows, nil
}

// NewUsageCollectorClientFromEnv creates a usage collector client by reading configuration from environment variables
// If USAGE_COLLECTOR_HOST is not set, returns nil (usage collection disabled)
// projectID is the default project events are attributed to
func NewUsageCollectorClientFromEnv(projectID string) (*UsageCollectorClient, error) {
	host := os.Getenv("USAGE_COLLECTOR_HOST")
	if host == "" {
		return nil, nil
	}
	return NewUsageCollectorClient(host, projectID, os.Getenv("USAGE_COLLECTOR_TLS") == "true")
}
