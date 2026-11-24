package nodes

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/ai-process/ktms/internal/circuitbreaker"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/llm/adapters"
	"github.com/ai-process/ktms/internal/registry"
)

const usageKindTranslate = "translate"

// Translate sends the batch to an MT engine when a node serves this model
// natively (engine mt, spec capability translate), and otherwise translates by
// prompt through GenerateText, as a chat model on a node does.
func (pa *PoolAdapter) Translate(ctx context.Context, req *llm.TranslateRequest, userID string, meta map[string]string) (*llm.TranslateResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	native, err := pa.servesNatively(ctx)
	if err != nil {
		return nil, poolUnavailable("list_nodes", err)
	}
	if !native {
		return llm.TranslateByPrompt(ctx, pa, req, userID, meta)
	}

	est := float64(llm.EstimateOutputTokens(llm.SourceCharacters(req.Texts)))
	cand, reqID, err := pa.acquireSlot(ctx, userID, est, registry.CapabilityTranslate)
	if err != nil {
		return nil, err
	}
	call := &nodeCall{kind: usageKindTranslate}
	defer pa.holdLease(ctx, cand, reqID, call)()

	_, model, token, err := pa.buildNodeClient(cand)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	res, err := adapters.TranslateHTTP(ctx, llm.VendorHTTPClient(), cand.Snapshot.Meta.Endpoint, token, model, req)
	duration := time.Since(start)
	if res != nil {
		call.finish(res.Usage, 0, err)
	} else {
		call.finish(llm.TokensUsage{}, 0, err)
	}
	nodeID := cand.Snapshot.Meta.NodeID
	if err != nil {
		// A pair the engine lacks or a reply that does not line up is the
		// request's fault, not the node's: no strike against its breaker.
		var mismatch *llm.TranslationMismatchError
		clientErr := errors.Is(err, llm.ErrUnsupportedLanguagePair) || errors.Is(err, llm.ErrTranslationUnsupported) ||
			errors.As(err, &mismatch) || (ctx.Err() != nil && !errors.Is(context.Cause(ctx), circuitbreaker.ErrUpstreamTimeout))
		var mtErr *adapters.MTError
		if errors.As(err, &mtErr) && mtErr.Status >= 400 && mtErr.Status < 500 && mtErr.Status != 429 {
			clientErr = true
		}
		if !clientErr {
			if recErr := pa.selector.mgr.Circuit().RecordFailure(context.Background(), nodeID); recErr != nil {
				log.Warn().Err(recErr).Str("node_id", nodeID).Msg("failed to record circuit failure")
			}
		}
		return nil, err
	}
	if recErr := pa.selector.mgr.Circuit().RecordSuccess(context.Background(), nodeID); recErr != nil {
		log.Warn().Err(recErr).Str("node_id", nodeID).Msg("failed to record circuit success")
	}
	if res.Usage.Output > 0 && duration > 0 {
		tps := float64(res.Usage.Output) / duration.Seconds()
		if recErr := pa.selector.mgr.Telemetry().RecordMetrics(context.Background(), nodeID, pa.model.ID, tps, -1); recErr != nil {
			log.Warn().Err(recErr).Str("node_id", nodeID).Msg("failed to record node telemetry")
		}
	}
	// Usage is reported under the registry id, whatever the engine calls it.
	res.Model = pa.model.ID
	if meta != nil {
		meta["node_id"] = nodeID
	}
	return res, nil
}

// servesNatively reports whether some node may take this model as an MT
// engine now. Only then does a lease make sense; a chat model's prompt path
// leases per chunk through GenerateText.
func (pa *PoolAdapter) servesNatively(ctx context.Context) (bool, error) {
	return Serves(ctx, pa.selector.mgr, pa.model.ID, registry.CapabilityTranslate)
}
