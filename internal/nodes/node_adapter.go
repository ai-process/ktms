package nodes

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/rs/zerolog/log"

	pb "github.com/ai-process/ktms/gen/ktms/v1"

	"github.com/ai-process/ktms/internal/circuitbreaker"
	"github.com/ai-process/ktms/internal/crypto"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/llm/adapters"
	"github.com/ai-process/ktms/internal/observability"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
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

func (pa *PoolAdapter) GenerateText(ctx context.Context, chat *llm.ChatContext) (*llm.Response, error) {
	if isJudgeOnly(pa.model.Capabilities) {
		return nil, adapters.ErrTextGenerationUnsupported
	}
	if ctx == nil {
		ctx = context.Background()
		if chat != nil && chat.Context() != nil {
			ctx = chat.Context()
		}
	}
	cand, reqID, err := pa.acquireSlotWithWait(ctx, chat)
	if err != nil {
		return nil, err
	}
	defer pa.holdLease(cand, reqID)()

	client, underlyingModel, _, err := pa.buildNodeClient(cand)
	if err != nil {
		return nil, err
	}

	adapter := pa.newNodeAdapter(client, underlyingModel, cand)
	start := time.Now()
	resp, err := adapter.GenerateText(ctx, chat)
	duration := time.Since(start)

	if err != nil {
		if (!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(ctx.Err(), context.Canceled)) || errors.Is(context.Cause(ctx), circuitbreaker.ErrUpstreamTimeout) {
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
	if isJudgeOnly(pa.model.Capabilities) {
		return adapters.ErrTextGenerationUnsupported
	}
	cand, reqID, err := pa.acquireSlotWithWait(ctx, chat)
	if err != nil {
		return err
	}
	defer pa.holdLease(cand, reqID)()

	client, underlyingModel, _, err := pa.buildNodeClient(cand)
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
		if (!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(ctx.Err(), context.Canceled)) || errors.Is(context.Cause(ctx), circuitbreaker.ErrUpstreamTimeout) {
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

// poolUnavailable passes capacity outcomes through and marks anything else
// (a Redis failure) as unknown pool state, so cloud fallbacks still run.
func poolUnavailable(op string, err error) error {
	var capErr *nodeCapacityError
	if err == nil || errors.As(err, &capErr) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	observability.DependencyErrorsTotal.WithLabelValues("redis", "node_"+op).Inc()
	return &poolStateError{op: op, err: err}
}

// externalLoad is the engine queue KTMS did not cause: requests the heartbeat
// saw waiting beyond KTMS's own leases on the node. Other clients' running
// requests share the engine's batch with ours and are not counted. The
// heartbeat may be one interval old, which errs towards caution after bursts.
func externalLoad(n *NodeSnapshot) int64 {
	if n.Heartbeat == nil {
		return 0
	}
	if ext := int64(n.Heartbeat.QueueDepth) - int64(n.Inflight); ext > 0 {
		return ext
	}
	return 0
}

// holdLease renews the slot for as long as the call runs: a unary generation
// outlasts the TTL as easily as a stream, and a lapsed lease lets the pool
// overbook the node. The returned func stops renewing and releases the slot.
func (pa *PoolAdapter) holdLease(cand *Candidate, reqID string) func() {
	nodeID := cand.Snapshot.Meta.NodeID
	every := pa.leaseTTL / 2
	timeout := min(2*time.Second, every) // a renewal must land before the lease runs out
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				rctx, cancel := context.WithTimeout(context.Background(), timeout)
				renewed, err := pa.selector.mgr.Slots().RenewLease(rctx, nodeID, reqID, pa.leaseTTL)
				cancel()
				if err != nil {
					log.Warn().Err(err).Str("node_id", nodeID).Str("req_id", reqID).Msg("failed to renew node slot lease")
				} else if !renewed {
					// Gone for good: the renew script never recreates a lease, so stop trying.
					log.Warn().Str("node_id", nodeID).Str("req_id", reqID).Msg("node slot lease lapsed before the call finished")
					return
				}
			}
		}
	}()
	return func() {
		close(stop)
		// No wait for an in-flight renewal: it cannot recreate a released lease.
		if err := pa.selector.mgr.Slots().ReleaseLease(context.Background(), nodeID, reqID); err != nil {
			log.Warn().Err(err).Str("node_id", nodeID).Str("req_id", reqID).Msg("failed to release node slot lease")
		}
	}
}

// newNodeAdapter applies the same model config as registry.buildAdapter, keyed by the node's engine.
func (pa *PoolAdapter) newNodeAdapter(client *openai.Client, model string, cand *Candidate) *adapters.OpenAIAdapter {
	a := adapters.NewOpenAIAdapter(client, model, llm.NewThrottleControl(0, 60))
	registry.ConfigureOpenAI(a, pa.model, cand.Snapshot.Meta.Engine)
	return a
}

func (pa *PoolAdapter) buildNodeClient(cand *Candidate) (*openai.Client, string, string, error) {
	endpoint := strings.TrimRight(cand.Snapshot.Meta.Endpoint, "/")
	if !strings.HasSuffix(endpoint, "/v1") {
		endpoint += "/v1"
	}

	var plainAuthToken string
	if cand.Snapshot.Meta.AuthToken != "" {
		if pa.keyring != nil {
			dec, err := pa.keyring.DecodeVersioned(cand.Snapshot.Meta.AuthToken)
			if err != nil {
				log.Warn().Err(err).Str("node_id", cand.Snapshot.Meta.NodeID).Msg("failed to decrypt node auth token")
			} else {
				plainAuthToken = dec
			}
		} else {
			plainAuthToken = cand.Snapshot.Meta.AuthToken
		}
	}
	if plainAuthToken == "" {
		plainAuthToken = "dummy-node-key"
	}

	opts := []option.RequestOption{
		option.WithBaseURL(endpoint),
		option.WithAPIKey(plainAuthToken),
		option.WithMaxRetries(0), // No SDK internal retries
		option.WithHTTPClient(llm.VendorHTTPClient()),
	}

	cl := openai.NewClient(opts...)
	underlyingModel := cand.ModelSpec.RemoteModelName
	if underlyingModel == "" {
		underlyingModel = cand.ModelSpec.ModelId
	}

	return &cl, underlyingModel, plainAuthToken, nil
}

func (pa *PoolAdapter) acquireSlotWithWait(ctx context.Context, chat *llm.ChatContext) (*Candidate, string, error) {
	var userID string
	tokens := 50.0
	if chat != nil {
		userID = chat.GetInternalUserID()
		if userID == "" {
			if meta := chat.GetMeta(); meta != nil {
				userID = meta["user_id"]
			}
		}
		if chat.GetMaxOutputTokens() > 0 {
			tokens = float64(chat.GetMaxOutputTokens())
			if tokens > 500 {
				tokens = 500
			}
		}
	}
	return pa.acquireSlot(ctx, userID, tokens, "")
}

func (pa *PoolAdapter) acquireSlot(ctx context.Context, userID string, estTokens float64, capability string) (*Candidate, string, error) {
	// 1. Quick attempt without waiting
	cand, err := pa.selector.SelectNodeForCapability(ctx, pa.model.ID, capability)
	if err == nil {
		reqID := uuid.NewString()
		acquired, leaseErr := pa.selector.mgr.Slots().AcquireLease(ctx, cand.Snapshot.Meta.NodeID, reqID, cand.Snapshot.Meta.MaxConcurrent, pa.leaseTTL)
		if leaseErr != nil {
			return nil, "", poolUnavailable("acquire_lease", leaseErr)
		}
		if acquired {
			return cand, reqID, nil
		}
	} else if !errors.Is(err, ErrNodePoolFull) {
		return nil, "", poolUnavailable("select_node", err)
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
		var queueDepth int64 // int64: reported gauges can be large enough to wrap int32 sums
		var totalDuration float64
		var perfCount int

		for _, n := range nodes {
			if n.State == pb.NodeState_NODE_STATE_OFFLINE || n.State == pb.NodeState_NODE_STATE_DRAINING || n.State == pb.NodeState_NODE_STATE_CIRCUIT_BROKEN {
				continue
			}
			hasModel := false
			for _, m := range n.Meta.Models {
				if m.ModelId == pa.model.ID {
					if capability != "" {
						if hasCapability(m.Capabilities, capability) {
							hasModel = true
							break
						}
					} else if !isJudgeOnly(m.Capabilities) {
						hasModel = true
						break
					}
				}
			}
			if !hasModel {
				continue
			}

			totalSlots += n.Meta.MaxConcurrent
			totalInflight += n.Inflight
			queueDepth += externalLoad(n)
			if perf, ok := n.Perf[pa.model.ID]; ok && perf.TPSEMA > 0 {
				dur := (estTokens / perf.TPSEMA) * 1000.0
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
			queuePos := int64(totalInflight) - int64(totalSlots) + 1 + queueDepth
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
			if errors.Is(context.Cause(ctx), circuitbreaker.ErrUpstreamTimeout) {
				return nil, "", ErrNodeSlotExhausted
			}
			return nil, "", ctx.Err()
		case <-time.After(sleepDur):
		}

		cand, err := pa.selector.SelectNodeForCapability(ctx, pa.model.ID, capability)
		if err == nil {
			reqID := uuid.NewString()
			acquired, leaseErr := pa.selector.mgr.Slots().AcquireLease(ctx, cand.Snapshot.Meta.NodeID, reqID, cand.Snapshot.Meta.MaxConcurrent, pa.leaseTTL)
			if leaseErr != nil {
				return nil, "", poolUnavailable("acquire_lease", leaseErr)
			}
			if acquired {
				return cand, reqID, nil
			}
		} else if !errors.Is(err, ErrNodePoolFull) {
			return nil, "", poolUnavailable("select_node", err)
		}

		backoff *= 2
		if backoff > 100*time.Millisecond {
			backoff = 100 * time.Millisecond
		}
	}
}

// Judge evaluates a decision request against a local node with judge capability.
func (pa *PoolAdapter) Judge(ctx context.Context, req *llm.JudgeRequest, userID string, meta map[string]string) (*llm.JudgeResult, error) {
	if !hasCapability(pa.model.Capabilities, registry.CapabilityJudge) {
		return nil, llm.ErrJudgmentUnsupported
	}

	if ctx == nil {
		ctx = context.Background()
	}

	estTokens := float64(judgeInputEstimate(req))
	cand, reqID, err := pa.acquireSlot(ctx, userID, estTokens, registry.CapabilityJudge)
	if err != nil {
		return nil, err
	}
	defer pa.holdLease(cand, reqID)()

	client, underlyingModel, plainAuthToken, err := pa.buildNodeClient(cand)
	if err != nil {
		return nil, err
	}

	adapter := pa.newNodeAdapter(client, underlyingModel, cand)
	adapter.SetDecisionsConfig(cand.Snapshot.Meta.Endpoint, plainAuthToken)

	start := time.Now()
	res, err := adapter.Judge(ctx, req, userID, meta)
	duration := time.Since(start)

	if err != nil {
		isClientErr := ctx.Err() != nil && !errors.Is(context.Cause(ctx), circuitbreaker.ErrUpstreamTimeout)
		var decisionErr *adapters.OpenAIDecisionError
		if errors.As(err, &decisionErr) {
			switch decisionErr.Status {
			case 400, 413, 422:
				isClientErr = true
			case 404:
				// Node engine does not expose /v1/decisions for this model.
				// Yield to the next model in the chain without tripping the node's circuit breaker.
				return nil, fmt.Errorf("%w: %v", llm.ErrJudgmentUnsupported, err)
			}
		}
		if !isClientErr {
			if recErr := pa.selector.mgr.Circuit().RecordFailure(context.Background(), cand.Snapshot.Meta.NodeID); recErr != nil {
				log.Warn().Err(recErr).Str("node_id", cand.Snapshot.Meta.NodeID).Msg("failed to record circuit failure")
			}
		}
		return nil, err
	}

	if recErr := pa.selector.mgr.Circuit().RecordSuccess(context.Background(), cand.Snapshot.Meta.NodeID); recErr != nil {
		log.Warn().Err(recErr).Str("node_id", cand.Snapshot.Meta.NodeID).Msg("failed to record circuit success")
	}

	if res != nil && res.Usage.Input > 0 && duration > 0 {
		inputTokens := float64(res.Usage.Input)
		ttftNorm := (float64(duration.Milliseconds()) / inputTokens) * 1000.0
		if recErr := pa.selector.mgr.Telemetry().RecordMetrics(context.Background(), cand.Snapshot.Meta.NodeID, pa.model.ID, -1, ttftNorm); recErr != nil {
			log.Warn().Err(recErr).Str("node_id", cand.Snapshot.Meta.NodeID).Msg("failed to record node telemetry")
		}
	}

	if meta != nil {
		meta["node_id"] = cand.Snapshot.Meta.NodeID
	}

	return res, nil
}

func judgeInputEstimate(req *llm.JudgeRequest) int64 {
	if req == nil {
		return 50
	}
	runes := utf8.RuneCountInString(req.StateJSON)
	for _, q := range req.Questions {
		runes += utf8.RuneCountInString(q.Instructions)
		for _, opt := range q.Options {
			runes += utf8.RuneCountInString(opt)
		}
		for _, lvl := range q.Levels {
			runes += utf8.RuneCountInString(lvl)
		}
		runes += utf8.RuneCountInString(q.TrueCriteria) + utf8.RuneCountInString(q.FalseCriteria)
	}
	est := int64(runes)/4 + 1
	if est < 10 {
		return 10
	}
	return est
}

// EvaluateRollouts scores multiple rollout candidate actions against a local node.
func (pa *PoolAdapter) EvaluateRollouts(ctx context.Context, req *llm.EvaluateRolloutsRequest, userID string, meta map[string]string) (*llm.EvaluateRolloutsResult, error) {
	if !hasCapability(pa.model.Capabilities, registry.CapabilityJudge) {
		return nil, llm.ErrJudgmentUnsupported
	}

	if ctx == nil {
		ctx = context.Background()
	}

	estTokens := float64(rolloutInputEstimate(req))
	cand, reqID, err := pa.acquireSlot(ctx, userID, estTokens, registry.CapabilityJudge)
	if err != nil {
		return nil, err
	}
	defer pa.holdLease(cand, reqID)()

	client, underlyingModel, plainAuthToken, err := pa.buildNodeClient(cand)
	if err != nil {
		return nil, err
	}

	adapter := pa.newNodeAdapter(client, underlyingModel, cand)
	adapter.SetDecisionsConfig(cand.Snapshot.Meta.Endpoint, plainAuthToken)

	start := time.Now()
	res, err := adapter.EvaluateRollouts(ctx, req, userID, meta)
	duration := time.Since(start)

	if err != nil {
		isClientErr := ctx.Err() != nil && !errors.Is(context.Cause(ctx), circuitbreaker.ErrUpstreamTimeout)
		var decisionErr *adapters.OpenAIDecisionError
		if errors.As(err, &decisionErr) {
			switch decisionErr.Status {
			case 400, 413, 422:
				isClientErr = true
			case 404:
				// Node engine does not expose /v1/decisions/rollouts for this model.
				// Yield to the next model in the chain without tripping the node's circuit breaker.
				return nil, fmt.Errorf("%w: %v", llm.ErrJudgmentUnsupported, err)
			}
		}
		if !isClientErr {
			if recErr := pa.selector.mgr.Circuit().RecordFailure(context.Background(), cand.Snapshot.Meta.NodeID); recErr != nil {
				log.Warn().Err(recErr).Str("node_id", cand.Snapshot.Meta.NodeID).Msg("failed to record circuit failure")
			}
		}
		return nil, err
	}

	if recErr := pa.selector.mgr.Circuit().RecordSuccess(context.Background(), cand.Snapshot.Meta.NodeID); recErr != nil {
		log.Warn().Err(recErr).Str("node_id", cand.Snapshot.Meta.NodeID).Msg("failed to record circuit success")
	}

	if res != nil && res.Usage.Input > 0 && duration > 0 {
		inputTokens := float64(res.Usage.Input)
		ttftNorm := (float64(duration.Milliseconds()) / inputTokens) * 1000.0
		if recErr := pa.selector.mgr.Telemetry().RecordMetrics(context.Background(), cand.Snapshot.Meta.NodeID, pa.model.ID, -1, ttftNorm); recErr != nil {
			log.Warn().Err(recErr).Str("node_id", cand.Snapshot.Meta.NodeID).Msg("failed to record node telemetry")
		}
	}

	if meta != nil {
		meta["node_id"] = cand.Snapshot.Meta.NodeID
	}

	return res, nil
}

func rolloutInputEstimate(req *llm.EvaluateRolloutsRequest) int64 {
	if req == nil {
		return 50
	}
	baseRunes := utf8.RuneCountInString(req.StateJSON)
	critRunes := 0
	for _, q := range req.Criteria {
		critRunes += utf8.RuneCountInString(q.Instructions)
		for _, opt := range q.Options {
			critRunes += utf8.RuneCountInString(opt)
		}
		for _, lvl := range q.Levels {
			critRunes += utf8.RuneCountInString(lvl)
		}
		critRunes += utf8.RuneCountInString(q.TrueCriteria) + utf8.RuneCountInString(q.FalseCriteria)
	}
	var total int64
	for _, c := range req.Candidates {
		candRunes := baseRunes + utf8.RuneCountInString(c.ActionJSON) + utf8.RuneCountInString(c.ActionText) + critRunes
		total += int64(candRunes)/4 + 1
	}
	if total < 10 {
		return 10
	}
	return total
}
