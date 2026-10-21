package usagereport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gofrs/uuid"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ai-process/ktms/internal/observability"
	usagev1 "github.com/ai-process/ktms/internal/usagereport/gen/usage/v1"
)

// UsageCollectorClient is a client for sending LLM usage events to the usage collector service
// It implements llm.UsageTracker interface
type UsageCollectorClient struct {
	client    usagev1.UsageServiceClient
	conn      *grpc.ClientConn
	queue     chan *usagev1.UsageEvent
	stop      chan struct{}
	done      chan struct{}
	ctx       context.Context // cancelled on Close, cutting an in-flight send short
	cancel    context.CancelFunc
	stopOnce  sync.Once
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

	c := newClient(usagev1.NewUsageServiceClient(conn), projectID, queueSize)
	c.conn = conn
	return c, nil
}

// Sending is decoupled from requests: events queue here and one sender ships
// them in batches, retrying until the collector takes them. Event IDs make
// retries safe; the collector drops duplicates.
const (
	queueSize     = 10000
	maxBatch      = 500 // the collector's limit for RecordUsageEvents
	flushEvery    = time.Second
	sendTimeout   = 5 * time.Second
	retryMax      = 30 * time.Second
	closeDeadline = 5 * time.Second
)

func newClient(client usagev1.UsageServiceClient, projectID string, size int) *UsageCollectorClient {
	ctx, cancel := context.WithCancel(context.Background())
	c := &UsageCollectorClient{
		client:    client,
		queue:     make(chan *usagev1.UsageEvent, size),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
		projectID: projectID,
	}
	go c.sendLoop()
	return c
}

func (c *UsageCollectorClient) sendLoop() {
	defer close(c.done)
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()
	batch := make([]*usagev1.UsageEvent, 0, maxBatch)
	backoff := flushEvery
	for {
		in := c.queue
		if len(batch) >= maxBatch {
			in = nil // a held batch is full: retry it before taking more
		}
		select {
		case ev := <-in:
			batch = append(batch, ev)
			if len(batch) < maxBatch {
				continue
			}
		case <-ticker.C:
		case <-c.stop:
			c.drain(batch)
			return
		}
		if len(batch) == 0 {
			continue
		}
		err := c.send(c.ctx, batch)
		if status.Code(err) == codes.InvalidArgument {
			// The collector refuses the whole batch for one bad event; retrying it
			// as is would block delivery for good, so find and drop the bad ones.
			err = c.sendEach(c.ctx, batch)
		}
		if err != nil {
			// Keep the batch and retry later; new events keep queueing meanwhile.
			observability.DependencyErrorsTotal.WithLabelValues("usage_collector", "record_usage").Inc()
			if backoff == flushEvery {
				log.Warn().Err(err).Int("events", len(batch)).Msg("usage collector unreachable, holding events and retrying")
			}
			if !c.sleep(backoff) {
				c.drain(batch)
				return
			}
			backoff = min(2*backoff, retryMax)
			continue
		}
		if backoff != flushEvery {
			log.Info().Int("events", len(batch)).Msg("usage collector reachable again, held events delivered")
		}
		backoff = flushEvery
		batch = batch[:0]
	}
}

func (c *UsageCollectorClient) send(ctx context.Context, batch []*usagev1.UsageEvent) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	_, err := c.client.RecordUsageEvents(ctx, &usagev1.RecordUsageEventsRequest{Events: batch})
	return err
}

// sendEach delivers a refused batch one event at a time, dropping (and
// counting) the events the collector rejects. Any other error stops it, so the
// caller retries the whole batch; duplicates of delivered ones are ignored.
func (c *UsageCollectorClient) sendEach(ctx context.Context, batch []*usagev1.UsageEvent) error {
	for _, ev := range batch {
		err := c.send(ctx, []*usagev1.UsageEvent{ev})
		if status.Code(err) == codes.InvalidArgument {
			observability.UsageEventsDroppedTotal.WithLabelValues("rejected").Inc()
			log.Error().Err(err).Str("event_id", ev.GetEventId()).Str("model", ev.GetModel()).Msg("collector rejected usage event, dropped")
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *UsageCollectorClient) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-c.stop:
		return false
	}
}

// drain makes one last attempt at shutdown for what is held and queued, in
// batches, all within closeDeadline, and counts whatever it could not deliver.
func (c *UsageCollectorClient) drain(batch []*usagev1.UsageEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), closeDeadline)
	defer cancel()
	for {
		for len(batch) < maxBatch {
			select {
			case ev := <-c.queue:
				batch = append(batch, ev)
				continue
			default:
			}
			break
		}
		if len(batch) == 0 {
			return
		}
		if c.send(ctx, batch) != nil {
			lost := len(batch) + len(c.queue)
			observability.UsageEventsDroppedTotal.WithLabelValues("shutdown").Add(float64(lost))
			log.Error().Int("events", lost).Msg("usage events not delivered before shutdown")
			return
		}
		batch = batch[:0]
	}
}

// SetProjectID sets the project ID (App UUID) for usage tracking
func (c *UsageCollectorClient) SetProjectID(projectID string) {
	if c == nil {
		return
	}
	c.projectID = projectID
}

// Close flushes held and queued events (up to a few seconds), then closes the connection.
func (c *UsageCollectorClient) Close() error {
	if c == nil {
		return nil
	}
	c.stopOnce.Do(func() {
		close(c.stop)
		c.cancel() // an in-flight send must not hold shutdown for its full timeout
	})
	<-c.done
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// RecordUsage queues a usage event for the collector. It returns an error only
// when validation fails; delivery is asynchronous and retried.
func (c *UsageCollectorClient) RecordUsage(
	userID string,
	actionID string,
	vendor string,
	model string,
	inputTokens int,
	outputTokens int,
	durationMs int64,
	costNanoUSD int64,
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
	if err := validateLimits(userID, projectID, actionID, vendor, model, costNanoUSD, status); err != nil {
		return err
	}

	event := &usagev1.UsageEvent{
		EventId:      uuid.Must(uuid.NewV7()).String(),
		OccurredAt:   timestamppb.Now(),
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
		CostNanousd:  costNanoUSD,
	}
	// Never block a request on accounting: a full queue means the collector has
	// been down long enough to fill it, and the loss is counted, not hidden.
	select {
	case <-c.stop:
		observability.UsageEventsDroppedTotal.WithLabelValues("closed").Inc()
		return nil
	default:
	}
	select {
	case c.queue <- event:
	default:
		observability.UsageEventsDroppedTotal.WithLabelValues("queue_full").Inc()
		log.Warn().Str("actionID", actionID).Str("model", model).Msg("usage event queue full, event dropped")
	}

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

// The collector's limits (light-tokenmeter domain.Event.Validate). Checking
// here keeps a bad event from being refused, with its batch, after the fact.
const (
	maxStringLength = 256
	maxVendorLength = 64
	maxModelLength  = 128
)

func validateLimits(userID, projectID, actionID, vendor, model string, cost int64, st usagev1.Status) error {
	switch {
	case len(userID) > maxStringLength, len(projectID) > maxStringLength, len(actionID) > maxStringLength:
		return fmt.Errorf("usage event ids longer than %d bytes (action %q)", maxStringLength, actionID)
	case vendor == "" || len(vendor) > maxVendorLength:
		return fmt.Errorf("usage event vendor must be 1-%d bytes, got %q", maxVendorLength, vendor)
	case model == "" || len(model) > maxModelLength:
		return fmt.Errorf("usage event model must be 1-%d bytes, got %d", maxModelLength, len(model))
	case cost < 0:
		return fmt.Errorf("usage event cost must not be negative, got %d", cost)
	case st == usagev1.Status_STATUS_UNSPECIFIED:
		return fmt.Errorf("usage event status must be set (action %q)", actionID)
	}
	return nil
}
