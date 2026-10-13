package nodes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/llm-proxy/internal/crypto"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/llm/adapters"
	"github.com/ai-process/llm-proxy/internal/proxydb"
)

type PoolResolver struct {
	selector *Selector
	keyring  *crypto.Keyring
	leaseTTL time.Duration
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

func (pr *PoolResolver) PoolAdapter(m *proxydb.Model) (llm.ClientAdapter, error) {
	return NewPoolAdapter(pr.selector, pr.keyring, m, pr.leaseTTL), nil
}

type PoolAdapter struct {
	selector *Selector
	keyring  *crypto.Keyring
	model    *proxydb.Model
	leaseTTL time.Duration
}

func NewPoolAdapter(selector *Selector, keyring *crypto.Keyring, model *proxydb.Model, leaseTTL time.Duration) *PoolAdapter {
	if leaseTTL <= 0 {
		leaseTTL = 60 * time.Second
	}
	return &PoolAdapter{
		selector: selector,
		keyring:  keyring,
		model:    model,
		leaseTTL: leaseTTL,
	}
}

func (pa *PoolAdapter) SupportsStreaming() bool {
	return true
}

func (pa *PoolAdapter) GenerateText(chat *llm.ChatContext) (*llm.Response, error) {
	ctx := context.Background()
	cand, err := pa.selector.SelectNode(ctx, pa.model.ID)
	if err != nil {
		return nil, fmt.Errorf("local pool select node: %w", err)
	}

	reqID := uuid.NewString()
	acquired, err := pa.selector.mgr.Slots().AcquireLease(ctx, cand.Snapshot.Meta.NodeID, reqID, cand.Snapshot.Meta.MaxConcurrent, pa.leaseTTL)
	if err != nil {
		return nil, fmt.Errorf("local pool acquire lease: %w", err)
	}
	if !acquired {
		return nil, ErrNodeSlotExhausted
	}
	defer func() {
		_ = pa.selector.mgr.Slots().ReleaseLease(context.Background(), cand.Snapshot.Meta.NodeID, reqID)
	}()

	client, underlyingModel, err := pa.buildNodeClient(cand)
	if err != nil {
		return nil, err
	}

	adapter := adapters.NewOpenAIAdapter(client, underlyingModel, llm.NewThrottleControl(0, 60))
	start := time.Now()
	resp, err := adapter.GenerateText(chat)
	duration := time.Since(start)

	if err != nil {
		_ = pa.selector.mgr.Circuit().RecordFailure(ctx, cand.Snapshot.Meta.NodeID)
		return nil, err
	}

	_ = pa.selector.mgr.Circuit().RecordSuccess(ctx, cand.Snapshot.Meta.NodeID)

	// Update telemetry
	if resp != nil && resp.Usage.Output > 0 && duration > 0 {
		tps := float64(resp.Usage.Output) / duration.Seconds()
		_ = pa.selector.mgr.Telemetry().RecordMetrics(ctx, cand.Snapshot.Meta.NodeID, pa.model.ID, tps, -1)
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
	cand, err := pa.selector.SelectNode(ctx, pa.model.ID)
	if err != nil {
		return fmt.Errorf("local pool select node: %w", err)
	}

	reqID := uuid.NewString()
	acquired, err := pa.selector.mgr.Slots().AcquireLease(ctx, cand.Snapshot.Meta.NodeID, reqID, cand.Snapshot.Meta.MaxConcurrent, pa.leaseTTL)
	if err != nil {
		return fmt.Errorf("local pool acquire lease: %w", err)
	}
	if !acquired {
		return ErrNodeSlotExhausted
	}
	defer func() {
		_ = pa.selector.mgr.Slots().ReleaseLease(context.Background(), cand.Snapshot.Meta.NodeID, reqID)
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
				_, _ = pa.selector.mgr.Slots().RenewLease(context.Background(), cand.Snapshot.Meta.NodeID, reqID, pa.leaseTTL)
			}
		}
	}()

	client, underlyingModel, err := pa.buildNodeClient(cand)
	if err != nil {
		return err
	}

	adapter := adapters.NewOpenAIAdapter(client, underlyingModel, llm.NewThrottleControl(0, 60))
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
		_ = pa.selector.mgr.Circuit().RecordFailure(context.Background(), cand.Snapshot.Meta.NodeID)
		return err
	}

	_ = pa.selector.mgr.Circuit().RecordSuccess(context.Background(), cand.Snapshot.Meta.NodeID)

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
		_ = pa.selector.mgr.Telemetry().RecordMetrics(context.Background(), cand.Snapshot.Meta.NodeID, pa.model.ID, tps, ttftNorm)
	}

	return nil
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
