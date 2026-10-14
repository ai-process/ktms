package nodes

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/rs/zerolog/log"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"

	"github.com/ai-process/llm-proxy/internal/crypto"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/llm/adapters"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
)

type UserRateLimitResolver func(userID, modelID string) *proxydb.UserRateLimit

type PoolResolver struct {
	selector          *Selector
	keyring           *crypto.Keyring
	leaseTTL          time.Duration
	userLimitResolver UserRateLimitResolver
}

func NewPoolResolver(selector *Selector, keyring *crypto.Keyring, leaseTTL time.Duration) *PoolResolver {
	if leaseTTL <= 0 {
		leaseTTL = 60 * time.Second
	}
	return &PoolResolver{
		selector: selector,
		keyring:  keyring,
		leaseTTL: leaseTTL,
	}
}

func (pr *PoolResolver) SetUserRateLimitResolver(resolver UserRateLimitResolver) {
	pr.userLimitResolver = resolver
}

func (pr *PoolResolver) PoolAdapter(m *proxydb.Model) (llm.ClientAdapter, error) {
	return NewPoolAdapter(pr.selector, pr.keyring, m, pr.leaseTTL, pr.userLimitResolver), nil
}

type PoolAdapter struct {
	selector          *Selector
	keyring           *crypto.Keyring
	model             *proxydb.Model
	leaseTTL          time.Duration
	userLimitResolver UserRateLimitResolver
}

func NewPoolAdapter(selector *Selector, keyring *crypto.Keyring, model *proxydb.Model, leaseTTL time.Duration, userLimitResolver UserRateLimitResolver) *PoolAdapter {
	if leaseTTL <= 0 {
		leaseTTL = 60 * time.Second
	}
	return &PoolAdapter{
		selector:          selector,
		keyring:           keyring,
		model:             model,
		leaseTTL:          leaseTTL,
		userLimitResolver: userLimitResolver,
	}
}

func (pa *PoolAdapter) SupportsStreaming() bool {
	return true
}

func (pa *PoolAdapter) GenerateText(chat *llm.ChatContext) (*llm.Response, error) {
	ctx := context.Background()
	if chat != nil {
		ctx = chat.Context()
	}
	cand, reqID, err := pa.acquireSlotWithWait(ctx, chat)
	if err != nil {
		return nil, err
	}
	defer func() {
		if relErr := pa.selector.mgr.Slots().ReleaseLease(context.Background(), cand.Snapshot.Meta.NodeID, reqID); relErr != nil {
			log.Warn().Err(relErr).Str("node_id", cand.Snapshot.Meta.NodeID).Str("req_id", reqID).Msg("failed to release node slot lease")
		}
	}()

	client, underlyingModel, err := pa.buildNodeClient(cand)
	if err != nil {
		return nil, err
	}

	adapter := pa.newNodeAdapter(client, underlyingModel, cand)
	start := time.Now()
	resp, err := adapter.GenerateText(chat)
	duration := time.Since(start)

	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(ctx.Err(), context.Canceled) {
			if recErr := pa.selector.mgr.Circuit().RecordFailure(context.Background(), cand.Snapshot.Meta.NodeID); recErr != nil {
				log.Warn().Err(recErr).Str("node_id", cand.Snapshot.Meta.NodeID).Msg("failed to record circuit failure")
			}
		}
		return nil, err
	}

	if recErr := pa.selector.mgr.Circuit().RecordSuccess(context.Background(), cand.Snapshot.Meta.NodeID); recErr != nil {
		log.Warn().Err(recErr).Str("node_id", cand.Snapshot.Meta.NodeID).Msg("failed to record circuit success")
	}

	// Update telemetry
	if resp != nil && resp.Usage.Output > 0 && duration > 0 {
		tps := float64(resp.Usage.Output) / duration.Seconds()
		if recErr := pa.selector.mgr.Telemetry().RecordMetrics(context.Background(), cand.Snapshot.Meta.NodeID, pa.model.ID, tps, -1); recErr != nil {
			log.Warn().Err(recErr).Str("node_id", cand.Snapshot.Meta.NodeID).Msg("failed to record node telemetry")
		}
	}

	// Attach node metadata
	if chat != nil {
		meta := chat.GetMeta()
		if meta == nil {
			meta = make(map[string]string)
		}
		meta["node_id"] = cand.Snapshot.Meta.NodeID
		chat.SetMeta(meta)
	}

	return resp, nil
}

func (pa *PoolAdapter) GenerateTextStream(ctx context.Context, chat *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
	cand, reqID, err := pa.acquireSlotWithWait(ctx, chat)
	if err != nil {
		return err
	}
	defer func() {
		if relErr := pa.selector.mgr.Slots().ReleaseLease(context.Background(), cand.Snapshot.Meta.NodeID, reqID); relErr != nil {
			log.Warn().Err(relErr).Str("node_id", cand.Snapshot.Meta.NodeID).Str("req_id", reqID).Msg("failed to release node slot lease")
		}
	}()

	// Background renewal ticker for streaming
	renewCtx, cancelRenew := context.WithCancel(ctx)
	defer cancelRenew()
	go func() {
		ticker := time.NewTicker(pa.leaseTTL / 2)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				renewed, renewErr := pa.selector.mgr.Slots().RenewLease(context.Background(), cand.Snapshot.Meta.NodeID, reqID, pa.leaseTTL)
				if renewErr != nil {
					log.Warn().Err(renewErr).Str("node_id", cand.Snapshot.Meta.NodeID).Str("req_id", reqID).Msg("failed to renew node slot lease")
				} else if !renewed {
					log.Warn().Str("node_id", cand.Snapshot.Meta.NodeID).Str("req_id", reqID).Msg("node slot lease expired or lost before renewal")
				}
			}
		}
	}()

	client, underlyingModel, err := pa.buildNodeClient(cand)
	if err != nil {
		return err
	}

	adapter := pa.newNodeAdapter(client, underlyingModel, cand)
	start := time.Now()
	var firstTokenTime time.Time
	var totalOutputTokens int

	err = adapter.GenerateTextStream(ctx, chat, func(chunk llm.StreamChunk) error {
		if firstTokenTime.IsZero() && chunk.Delta != "" {
			firstTokenTime = time.Now()
		}
		if chunk.Usage != nil {
			totalOutputTokens = chunk.Usage.Output
		}
		return onChunk(chunk)
	})

	duration := time.Since(start)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(ctx.Err(), context.Canceled) {
			if recErr := pa.selector.mgr.Circuit().RecordFailure(context.Background(), cand.Snapshot.Meta.NodeID); recErr != nil {
				log.Warn().Err(recErr).Str("node_id", cand.Snapshot.Meta.NodeID).Msg("failed to record circuit failure")
			}
		}
		return err
	}

	if recErr := pa.selector.mgr.Circuit().RecordSuccess(context.Background(), cand.Snapshot.Meta.NodeID); recErr != nil {
		log.Warn().Err(recErr).Str("node_id", cand.Snapshot.Meta.NodeID).Msg("failed to record circuit success")
	}

	// Calculate and record telemetry (TTFT & TPS)
	var ttftNorm float64 = -1
	if !firstTokenTime.IsZero() {
		ttftMs := float64(firstTokenTime.Sub(start).Milliseconds())
		inputTokens := 1
		if chat != nil {
			inputTokens = chat.EstimateInputTokens()
		}
		if inputTokens <= 0 {
			inputTokens = 1
		}
		ttftNorm = (ttftMs / float64(inputTokens)) * 1000.0
	}

	var tps float64 = -1
	if totalOutputTokens > 0 && duration > 0 {
		tps = float64(totalOutputTokens) / duration.Seconds()
	}

	if tps > 0 || ttftNorm > 0 {
		if recErr := pa.selector.mgr.Telemetry().RecordMetrics(context.Background(), cand.Snapshot.Meta.NodeID, pa.model.ID, tps, ttftNorm); recErr != nil {
			log.Warn().Err(recErr).Str("node_id", cand.Snapshot.Meta.NodeID).Msg("failed to record node telemetry")
		}
	}

	return nil
}

// newNodeAdapter applies the same model config as registry.buildAdapter, keyed by the node's engine.
func (pa *PoolAdapter) newNodeAdapter(client *openai.Client, model string, cand *Candidate) *adapters.OpenAIAdapter {
	a := adapters.NewOpenAIAdapter(client, model, llm.NewThrottleControl(0, 60))
	registry.ConfigureOpenAI(a, pa.model, cand.Snapshot.Meta.Engine)
	return a
}

func (pa *PoolAdapter) buildNodeClient(cand *Candidate) (*openai.Client, string, error) {
	endpoint := strings.TrimRight(cand.Snapshot.Meta.Endpoint, "/")
	if !strings.HasSuffix(endpoint, "/v1") {
		endpoint += "/v1"
	}

	var plainAuthToken string
	if cand.Snapshot.Meta.AuthToken != "" && pa.keyring != nil {
		dec, err := pa.keyring.DecodeVersioned(cand.Snapshot.Meta.AuthToken)
		if err != nil {
			log.Warn().Err(err).Str("node_id", cand.Snapshot.Meta.NodeID).Msg("failed to decrypt node auth token")
		} else {
			plainAuthToken = dec
		}
	}
	if plainAuthToken == "" {
		plainAuthToken = "dummy-node-key"
	}

	opts := []option.RequestOption{
		option.WithBaseURL(endpoint),
		option.WithAPIKey(plainAuthToken),
		option.WithMaxRetries(0), // No SDK internal retries
	}

	cl := openai.NewClient(opts...)
	underlyingModel := cand.ModelSpec.RemoteModelName
	if underlyingModel == "" {
		underlyingModel = cand.ModelSpec.ModelId
	}

	return &cl, underlyingModel, nil
}

func (pa *PoolAdapter) acquireSlotWithWait(ctx context.Context, chat *llm.ChatContext) (*Candidate, string, error) {
	// 1. Quick attempt without waiting
	cand, err := pa.selector.SelectNode(ctx, pa.model.ID)
	if err == nil {
		reqID := uuid.NewString()
		acquired, leaseErr := pa.selector.mgr.Slots().AcquireLease(ctx, cand.Snapshot.Meta.NodeID, reqID, cand.Snapshot.Meta.MaxConcurrent, pa.leaseTTL)
		if leaseErr != nil {
			return nil, "", fmt.Errorf("local pool acquire lease: %w", leaseErr)
		}
		if acquired {
			return cand, reqID, nil
		}
	} else if !errors.Is(err, ErrNodePoolFull) {
		return nil, "", err
	}

	// Determine user ID
	var userID string
	if chat != nil {
		userID = chat.GetInternalUserID()
		if userID == "" {
			if meta := chat.GetMeta(); meta != nil {
				userID = meta["user_id"]
			}
		}
	}

	// Determine queue wait timeout
	timeout := time.Duration(pa.model.QueueTimeoutMs) * time.Millisecond
	if pa.userLimitResolver != nil && userID != "" {
		if ul := pa.userLimitResolver(userID, pa.model.ID); ul != nil && ul.QueueTimeoutMs > 0 {
			timeout = time.Duration(ul.QueueTimeoutMs) * time.Millisecond
		}
	}
	if timeout <= 0 {
		return nil, "", ErrNodeSlotExhausted
	}

	// Prioritize interactive vs service traffic:
	// If interactive (non-svc), cap timeout to 250ms to protect TTFT.
	if userID != "" && !strings.HasPrefix(userID, "svc:") {
		if timeout > 250*time.Millisecond {
			timeout = 250 * time.Millisecond
		}
	}

	// Predictive admission check via Little's Law: Wait_est = (QueuePos * Duration_ema) / TotalSlots
	nodes, listErr := pa.selector.mgr.ListNodes(ctx)
	if listErr == nil && len(nodes) > 0 {
		var totalSlots int32
		var totalInflight int32
		var queueDepth int32
		var totalDuration float64
		var perfCount int

		for _, n := range nodes {
			if n.State == pb.NodeState_NODE_STATE_OFFLINE || n.State == pb.NodeState_NODE_STATE_DRAINING || n.State == pb.NodeState_NODE_STATE_CIRCUIT_BROKEN {
				continue
			}
			hasModel := false
			for _, m := range n.Meta.Models {
				if m.ModelId == pa.model.ID {
					hasModel = true
					break
				}
			}
			if !hasModel {
				continue
			}

			totalSlots += n.Meta.MaxConcurrent
			totalInflight += n.Inflight
			if n.Heartbeat != nil {
				queueDepth += n.Heartbeat.QueueDepth
			}
			if perf, ok := n.Perf[pa.model.ID]; ok && perf.TPSEMA > 0 {
				tokens := 50.0
				if chat != nil && chat.GetMaxOutputTokens() > 0 {
					tokens = float64(chat.GetMaxOutputTokens())
					if tokens > 500 {
						tokens = 500
					}
				}
				dur := (tokens / perf.TPSEMA) * 1000.0
				if dur < 10 {
					dur = 10
				}
				if dur > 10000 {
					dur = 10000
				}
				totalDuration += dur
				perfCount++
			}
		}

		if totalSlots > 0 {
			durationEMA := 500.0 // Default 500ms duration estimate if telemetry not populated
			if perfCount > 0 {
				durationEMA = totalDuration / float64(perfCount)
			}
			queuePos := totalInflight - totalSlots + 1 + queueDepth
			if queuePos < 1 {
				queuePos = 1
			}
			waitEstMs := (float64(queuePos) * durationEMA) / float64(totalSlots)
			if waitEstMs > float64(timeout.Milliseconds()) {
				log.Debug().
					Float64("wait_est_ms", waitEstMs).
					Int64("timeout_ms", timeout.Milliseconds()).
					Str("model", pa.model.ID).
					Msg("predictive queue admission: estimated wait exceeds timeout, skipping to next model")
				return nil, "", ErrNodeSlotExhausted
			}
		}
	}

	// Jittered backoff polling loop
	deadline := time.Now().Add(timeout)
	backoff := 25 * time.Millisecond

	for {
		if time.Now().After(deadline) {
			return nil, "", ErrNodeSlotExhausted
		}

		// Jitter +/- 10%
		jitter := float64(backoff) * (0.9 + 0.2*rand.Float64())
		sleepDur := time.Duration(jitter)
		if time.Now().Add(sleepDur).After(deadline) {
			sleepDur = time.Until(deadline)
		}

		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-time.After(sleepDur):
		}

		cand, err := pa.selector.SelectNode(ctx, pa.model.ID)
		if err == nil {
			reqID := uuid.NewString()
			acquired, leaseErr := pa.selector.mgr.Slots().AcquireLease(ctx, cand.Snapshot.Meta.NodeID, reqID, cand.Snapshot.Meta.MaxConcurrent, pa.leaseTTL)
			if leaseErr != nil {
				return nil, "", fmt.Errorf("local pool acquire lease: %w", leaseErr)
			}
			if acquired {
				return cand, reqID, nil
			}
		} else if !errors.Is(err, ErrNodePoolFull) {
			return nil, "", err
		}

		backoff *= 2
		if backoff > 100*time.Millisecond {
			backoff = 100 * time.Millisecond
		}
	}
}
