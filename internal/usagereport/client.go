package usagereport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
	closeMu   sync.RWMutex // held for reading by RecordUsage; Close takes it to shut the door
	closed    bool
	hasToken  bool // only to word the log when the collector refuses us
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
// projectID is the default project events are attributed to; a non-empty
// token is sent as a bearer token on every call.
func NewUsageCollectorClient(host, projectID string, useTLS bool, token string) (*UsageCollectorClient, error) {
	if host == "" {
		return nil, nil
	}

	// Plaintext on a private network; TLS for a sink behind a managed front.
	creds := insecure.NewCredentials()
	if useTLS {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}
	if token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(bearerToken{"authorization": "Bearer " + token}))
		if !useTLS {
			log.Info().Msg("USAGE_COLLECTOR_TOKEN is sent without TLS; keep the usage sink on a private network or set USAGE_COLLECTOR_TLS")
		}
	}
	conn, err := grpc.NewClient(host, opts...)
	if err != nil {
		return nil, err
	}

	c := newClient(usagev1.NewUsageServiceClient(conn), projectID, queueSize)
	c.conn = conn
	c.hasToken = token != ""
	return c, nil
}

// bearerToken may travel in plaintext: USAGE_COLLECTOR_TLS=false is for a
// private network, where the collector's auth guards against stray callers.
type bearerToken map[string]string

func (t bearerToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return t, nil
}

func (bearerToken) RequireTransportSecurity() bool { return false }

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
	authLogged := false // a refusal is logged once until a send succeeds again
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
		var err error
		batch, err = c.deliver(c.ctx, batch)
		if err != nil && c.ctx.Err() != nil {
			c.drain(batch) // Close cut the send short; that is not a collector failure
			return
		}
		if err != nil {
			// Keep the batch and retry later; new events keep queueing meanwhile.
			observability.DependencyErrorsTotal.WithLabelValues("usage_collector", "record_usage").Inc()
			if msg := c.refusal(err); msg != "" {
				if !authLogged {
					log.Error().Err(err).Int("events", len(batch)).Msg(msg + ", holding events and retrying")
					authLogged = true
				}
			} else if backoff == flushEvery {
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
		authLogged = false
		backoff = flushEvery
	}
}

// refusal explains an auth error from the collector; "" for any other error.
func (c *UsageCollectorClient) refusal(err error) string {
	switch status.Code(err) {
	case codes.Unauthenticated:
		if !c.hasToken {
			return "usage collector requires a token: set USAGE_COLLECTOR_TOKEN"
		}
		return "usage collector rejected USAGE_COLLECTOR_TOKEN"
	case codes.PermissionDenied:
		return "USAGE_COLLECTOR_TOKEN lacks the collector's write scope"
	}
	return ""
}

func (c *UsageCollectorClient) send(ctx context.Context, batch []*usagev1.UsageEvent) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	_, err := c.client.RecordUsageEvents(ctx, &usagev1.RecordUsageEventsRequest{Events: batch})
	return err
}

// deliver sends a batch and returns what is still to be sent. The collector
// refuses a whole batch for one invalid event and names its index; that event
// is dropped (counted once) and the rest resent, so it cannot block delivery.
func (c *UsageCollectorClient) deliver(ctx context.Context, batch []*usagev1.UsageEvent) ([]*usagev1.UsageEvent, error) {
	for len(batch) > 0 {
		err := c.send(ctx, batch)
		if err == nil {
			return batch[:0], nil
		}
		if status.Code(err) != codes.InvalidArgument {
			return batch, err
		}
		i, ok := rejectedIndex(err, len(batch))
		if !ok {
			// No index to go on: settle it one event at a time.
			return c.deliverEach(ctx, batch)
		}
		c.reject(batch[i], err)
		batch = append(batch[:i], batch[i+1:]...)
	}
	return batch, nil
}

func (c *UsageCollectorClient) deliverEach(ctx context.Context, batch []*usagev1.UsageEvent) ([]*usagev1.UsageEvent, error) {
	for i, ev := range batch {
		err := c.send(ctx, []*usagev1.UsageEvent{ev})
		if status.Code(err) == codes.InvalidArgument {
			c.reject(ev, err)
			continue
		}
		if err != nil {
			return batch[i:], err
		}
	}
	return batch[:0], nil
}

func (c *UsageCollectorClient) reject(ev *usagev1.UsageEvent, err error) {
	observability.UsageEventsDroppedTotal.WithLabelValues("rejected").Inc()
	log.Error().Err(err).Str("event_id", ev.GetEventId()).Str("model", ev.GetModel()).Msg("collector rejected usage event, dropped")
}

// rejectedIndex reads "invalid event at index N" from light-tokenmeter.
func rejectedIndex(err error, n int) (int, bool) {
	msg := status.Convert(err).Message()
	const marker = "at index "
	j := strings.Index(msg, marker)
	if j < 0 {
		return 0, false
	}
	digits := msg[j+len(marker):]
	if k := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); k >= 0 {
		digits = digits[:k]
	}
	i, err := strconv.Atoi(digits)
	if err != nil || i < 0 || i >= n {
		return 0, false
	}
	return i, true
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
		var err error
		if batch, err = c.deliver(ctx, batch); err != nil {
			lost := len(batch) + len(c.queue)
			observability.UsageEventsDroppedTotal.WithLabelValues("shutdown").Add(float64(lost))
			log.Error().Err(err).Int("events", lost).Msg("usage events not delivered before shutdown")
			return
		}
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
		c.closeMu.Lock()
		c.closed = true // no enqueue can land after the drain below has looked
		c.closeMu.Unlock()
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
	c.closeMu.RLock()
	defer c.closeMu.RUnlock()
	if c.closed {
		observability.UsageEventsDroppedTotal.WithLabelValues("closed").Inc()
		return nil
	}
	select {
	case c.queue <- event:
	default:
		observability.UsageEventsDroppedTotal.WithLabelValues("queue_full").Inc()
		log.Warn().Str("actionID", actionID).Str("model", model).Msg("usage event queue full, event dropped")
	}

	return nil
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
