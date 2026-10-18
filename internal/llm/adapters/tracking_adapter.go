package adapters

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/observability"
	usagev1 "github.com/ai-process/llm-proxy/internal/usagereport/gen/usage/v1"
)

// TrackingAdapter wraps a ClientAdapter and automatically tracks usage
type TrackingAdapter struct {
	adapter llm.ClientAdapter
	tracker llm.UsageTracker
	vendor  string
	model   string
	pricer  Pricer
}

// Pricer prices one call's usage in nano-USD for a call that started at the
// given time (rates can depend on the hour).
type Pricer func(usage llm.TokensUsage, startedAt time.Time) int64

// NewTrackingAdapter creates a new tracking adapter that wraps an existing adapter
func NewTrackingAdapter(adapter llm.ClientAdapter, tracker llm.UsageTracker, vendor, model string) *TrackingAdapter {
	return &TrackingAdapter{
		adapter: adapter,
		tracker: tracker,
		vendor:  vendor,
		model:   model,
	}
}

// WithPricer stamps every usage event with its cost; without one events carry 0.
func (t *TrackingAdapter) WithPricer(p Pricer) *TrackingAdapter {
	t.pricer = p
	return t
}

func (t *TrackingAdapter) cost(ctx context.Context, in, out int, startedAt time.Time) int64 {
	if llm.IsShadowTraffic(ctx) {
		return 0
	}
	if t.pricer == nil {
		return 0
	}
	return t.pricer(llm.TokensUsage{Input: in, Output: out}, startedAt)
}

// GenerateText generates text and tracks usage
func (t *TrackingAdapter) GenerateText(ctx context.Context, chat *llm.ChatContext) (*llm.Response, error) {
	startTime := time.Now()
	effort := "default"
	var userID string
	actionID := "llm.generate_text"
	var meta map[string]string

	if chat != nil {
		userID = chat.GetInternalUserID()
		if actionIDFromContext := chat.GetActionID(); actionIDFromContext != "" {
			actionID = actionIDFromContext
		}
		meta = chat.GetMeta()
		if e := meta["effort"]; e != "" {
			effort = e
		}
	}

	attrs := []attribute.KeyValue{
		observability.AttrGenAIProviderName.String(t.vendor),
		observability.AttrGenAIRequestModel.String(t.model),
		observability.AttrLLMProxyEffort.String(effort),
	}
	if meta != nil {
		if r := meta["rule"]; r != "" {
			attrs = append(attrs, observability.AttrLLMProxyRule.String(r))
		}
		if k := meta["api_key"]; k != "" {
			attrs = append(attrs, observability.AttrLLMProxyClientKey.String(k))
		}
	}

	ctx, span := observability.Tracer().Start(ctx, "chat "+t.model, trace.WithAttributes(attrs...))
	defer span.End()

	observability.ActiveRequests.WithLabelValues(t.vendor, t.model).Inc()
	defer observability.ActiveRequests.WithLabelValues(t.vendor, t.model).Dec()

	resp, err := t.adapter.GenerateText(ctx, chat)
	duration := time.Since(startTime)
	durationMs := duration.Milliseconds()

	statusLabel := "success"
	if err != nil {
		statusLabel = "error"
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	observability.RequestsTotal.WithLabelValues(t.vendor, t.model, effort, statusLabel).Inc()
	observability.RequestDuration.WithLabelValues(t.vendor, t.model, effort).Observe(duration.Seconds())

	if resp != nil {
		observability.TokensTotal.WithLabelValues(t.vendor, t.model, "input").Add(float64(resp.Usage.Input))
		observability.TokensTotal.WithLabelValues(t.vendor, t.model, "output").Add(float64(resp.Usage.Output))
		span.SetAttributes(
			observability.AttrGenAIResponseModel.String(t.model),
			observability.AttrGenAIUsageInput.Int(resp.Usage.Input),
			observability.AttrGenAIUsageOutput.Int(resp.Usage.Output),
		)
	}

	// Always proceed with the request, but track usage if tracker is available
	if t.tracker != nil {
		var status usagev1.Status
		if err != nil {
			status = usagev1.Status_STATUS_ERROR
		} else {
			status = usagev1.Status_STATUS_SUCCESS
		}

		if resp != nil {
			if trackErr := t.tracker.RecordUsage(
				userID,
				actionID,
				t.vendor,
				t.model,
				resp.Usage.Input,
				resp.Usage.Output,
				durationMs,
				t.cost(ctx, resp.Usage.Input, resp.Usage.Output, startTime),
				status,
				meta,
			); trackErr != nil {
				log.Error().
					Stack().
					Err(trackErr).
					Str("userID", userID).
					Str("actionID", actionID).
					Str("vendor", t.vendor).
					Str("model", t.model).
					Msg("Failed to record usage")
			}
		} else if err != nil {
			if trackErr := t.tracker.RecordUsage(
				userID,
				actionID,
				t.vendor,
				t.model,
				0,
				0,
				durationMs,
				0,
				status,
				meta,
			); trackErr != nil {
				log.Error().
					Stack().
					Err(trackErr).
					Str("userID", userID).
					Str("actionID", actionID).
					Str("vendor", t.vendor).
					Str("model", t.model).
					Msg("Failed to record usage")
			}
		}
	}

	return resp, err
}

// SupportsStreaming reports whether the underlying adapter supports streaming.
func (t *TrackingAdapter) SupportsStreaming() bool {
	streamer, ok := t.adapter.(llm.StreamAdapter)
	if !ok {
		return false
	}
	if sc, ok := streamer.(interface{ SupportsStreaming() bool }); ok {
		return sc.SupportsStreaming()
	}
	return true
}

// GenerateTextStream forwards streaming text generation and tracks usage upon completion.
func (t *TrackingAdapter) GenerateTextStream(ctx context.Context, chat *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
	streamer, ok := t.adapter.(llm.StreamAdapter)
	if !ok {
		return fmt.Errorf("%w: model %s", llm.ErrStreamingNotSupported, t.model)
	}

	startTime := time.Now()
	effort := "default"
	var userID string
	actionID := "llm.generate_text"
	var meta map[string]string
	if chat != nil {
		userID = chat.GetInternalUserID()
		if actionIDFromContext := chat.GetActionID(); actionIDFromContext != "" {
			actionID = actionIDFromContext
		}
		meta = chat.GetMeta()
		if e := meta["effort"]; e != "" {
			effort = e
		}
	}

	attrs := []attribute.KeyValue{
		observability.AttrGenAIProviderName.String(t.vendor),
		observability.AttrGenAIRequestModel.String(t.model),
		observability.AttrLLMProxyEffort.String(effort),
	}
	if meta != nil {
		if r := meta["rule"]; r != "" {
			attrs = append(attrs, observability.AttrLLMProxyRule.String(r))
		}
		if k := meta["api_key"]; k != "" {
			attrs = append(attrs, observability.AttrLLMProxyClientKey.String(k))
		}
	}

	ctx, span := observability.Tracer().Start(ctx, "chat_stream "+t.model, trace.WithAttributes(attrs...))
	defer span.End()

	observability.ActiveRequests.WithLabelValues(t.vendor, t.model).Inc()
	defer observability.ActiveRequests.WithLabelValues(t.vendor, t.model).Dec()

	var (
		lastUsage        llm.TokensUsage
		outputCharsCount int
		chunksEmitted    int
	)
	wrappedChunk := func(chunk llm.StreamChunk) error {
		chunksEmitted++
		outputCharsCount += len([]rune(chunk.Delta))
		for _, tc := range chunk.ToolCallChunks {
			outputCharsCount += len([]rune(tc.ArgumentsDelta)) + len([]rune(tc.Name))
		}
		if chunk.Usage != nil {
			lastUsage = *chunk.Usage
		}
		return onChunk(chunk)
	}

	err := streamer.GenerateTextStream(ctx, chat, wrappedChunk)
	duration := time.Since(startTime)
	durationMs := duration.Milliseconds()

	// If the stream ended without vendor token accounting (cut short, mid-stream disconnect,
	// or aborted before first chunk while thinking), fall back to estimating consumed tokens.
	disconnected := ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	if lastUsage.Input == 0 && (chunksEmitted > 0 || disconnected) {
		if chat != nil {
			lastUsage.Input = chat.EstimateInputTokens()
		}
	}
	if lastUsage.Output == 0 && chunksEmitted > 0 {
		lastUsage.Output = llm.EstimateOutputTokens(outputCharsCount)
	}

	statusLabel := "success"
	if err != nil {
		statusLabel = "error"
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	observability.RequestsTotal.WithLabelValues(t.vendor, t.model, effort, statusLabel).Inc()
	observability.RequestDuration.WithLabelValues(t.vendor, t.model, effort).Observe(duration.Seconds())

	if lastUsage.Input > 0 || lastUsage.Output > 0 {
		observability.TokensTotal.WithLabelValues(t.vendor, t.model, "input").Add(float64(lastUsage.Input))
		observability.TokensTotal.WithLabelValues(t.vendor, t.model, "output").Add(float64(lastUsage.Output))
		span.SetAttributes(
			observability.AttrGenAIResponseModel.String(t.model),
			observability.AttrGenAIUsageInput.Int(lastUsage.Input),
			observability.AttrGenAIUsageOutput.Int(lastUsage.Output),
		)
	}

	if t.tracker != nil {
		var status usagev1.Status
		if err != nil {
			status = usagev1.Status_STATUS_ERROR
		} else {
			status = usagev1.Status_STATUS_SUCCESS
		}
		if trackErr := t.tracker.RecordUsage(
			userID,
			actionID,
			t.vendor,
			t.model,
			lastUsage.Input,
			lastUsage.Output,
			durationMs,
			t.cost(ctx, lastUsage.Input, lastUsage.Output, startTime),
			status,
			meta,
		); trackErr != nil {
			log.Error().
				Stack().
				Err(trackErr).
				Str("userID", userID).
				Str("actionID", actionID).
				Str("vendor", t.vendor).
				Str("model", t.model).
				Msg("Failed to record usage for stream")
		}
	}

	return err
}

// SynthesizeSpeech synthesizes speech and tracks usage
func (t *TrackingAdapter) SynthesizeSpeech(ctx context.Context, text, language, userID string, meta map[string]string) (*llm.SpeechResult, error) {
	synth, ok := t.adapter.(llm.SpeechSynthesizer)
	if !ok {
		return nil, fmt.Errorf("%s does not synthesize speech", t.model)
	}

	startTime := time.Now()
	effort := "default"
	if meta != nil && meta["effort"] != "" {
		effort = meta["effort"]
	}

	attrs := []attribute.KeyValue{
		observability.AttrGenAIProviderName.String(t.vendor),
		observability.AttrGenAIRequestModel.String(t.model),
	}
	if meta != nil {
		if r := meta["rule"]; r != "" {
			attrs = append(attrs, observability.AttrLLMProxyRule.String(r))
		}
		if k := meta["api_key"]; k != "" {
			attrs = append(attrs, observability.AttrLLMProxyClientKey.String(k))
		}
	}

	ctx, span := observability.Tracer().Start(ctx, "synthesize_speech "+t.model, trace.WithAttributes(attrs...))
	defer span.End()

	observability.ActiveRequests.WithLabelValues(t.vendor, t.model).Inc()
	defer observability.ActiveRequests.WithLabelValues(t.vendor, t.model).Dec()

	result, err := synth.SynthesizeSpeech(ctx, text, language, userID, meta)
	duration := time.Since(startTime)
	durationMs := duration.Milliseconds()

	statusLabel := "success"
	if err != nil {
		statusLabel = "error"
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	observability.RequestsTotal.WithLabelValues(t.vendor, t.model, effort, statusLabel).Inc()
	observability.RequestDuration.WithLabelValues(t.vendor, t.model, effort).Observe(duration.Seconds())

	if t.tracker != nil {
		var status usagev1.Status
		if err != nil {
			status = usagev1.Status_STATUS_ERROR
		} else {
			status = usagev1.Status_STATUS_SUCCESS
		}

		// Record the TTS model actually used (falling back to the wrapped chat
		// model on error) and the input length in chars — OpenAI TTS reports no
		// tokens, so chars are the only spend signal.
		model := t.model
		var inTokens, outTokens int
		if result != nil {
			if result.Model != "" {
				model = result.Model
			}
			inTokens = result.Usage.Input
			outTokens = result.Usage.Output
			observability.TokensTotal.WithLabelValues(t.vendor, model, "input").Add(float64(inTokens))
			observability.TokensTotal.WithLabelValues(t.vendor, model, "output").Add(float64(outTokens))
			span.SetAttributes(
				observability.AttrGenAIResponseModel.String(model),
				observability.AttrGenAIUsageInput.Int(inTokens),
				observability.AttrGenAIUsageOutput.Int(outTokens),
			)
		}
		trackMeta := map[string]string{}
		for k, v := range meta {
			trackMeta[k] = v
		}
		trackMeta["chars"] = strconv.Itoa(len(text))
		// Capture the failure reason so TTS errors are diagnosable without
		// log-diving (RecordUsage has no error field).
		if err != nil {
			msg := err.Error()
			if len(msg) > 300 {
				msg = msg[:300]
			}
			trackMeta["error"] = msg
		}

		if trackErr := t.tracker.RecordUsage(
			userID,
			"llm.synthesize_speech",
			t.vendor,
			model,
			inTokens,
			outTokens,
			durationMs,
			t.cost(ctx, inTokens, outTokens, startTime),
			status,
			trackMeta,
		); trackErr != nil {
			log.Error().
				Stack().
				Err(trackErr).
				Str("userID", userID).
				Str("actionID", "llm.synthesize_speech").
				Str("vendor", t.vendor).
				Str("model", model).
				Msg("Failed to record usage")
		}
	}

	return result, err
}

// GenerateImage forwards to the wrapped adapter's optional image capability and
// tracks usage. Adapters without one surface ErrImageGenerationUnsupported.
func (t *TrackingAdapter) GenerateImage(ctx context.Context, prompt, userID string, meta map[string]string) (*llm.ImageResult, error) {
	gen, ok := t.adapter.(llm.ImageGenerator)
	if !ok {
		return nil, llm.ErrImageGenerationUnsupported
	}

	startTime := time.Now()
	effort := "default"
	if meta != nil && meta["effort"] != "" {
		effort = meta["effort"]
	}

	attrs := []attribute.KeyValue{
		observability.AttrGenAIProviderName.String(t.vendor),
		observability.AttrGenAIRequestModel.String(t.model),
	}
	if meta != nil {
		if r := meta["rule"]; r != "" {
			attrs = append(attrs, observability.AttrLLMProxyRule.String(r))
		}
		if k := meta["api_key"]; k != "" {
			attrs = append(attrs, observability.AttrLLMProxyClientKey.String(k))
		}
	}

	ctx, span := observability.Tracer().Start(ctx, "generate_image "+t.model, trace.WithAttributes(attrs...))
	defer span.End()

	observability.ActiveRequests.WithLabelValues(t.vendor, t.model).Inc()
	defer observability.ActiveRequests.WithLabelValues(t.vendor, t.model).Dec()

	result, err := gen.GenerateImage(ctx, prompt, userID, meta)
	duration := time.Since(startTime)
	durationMs := duration.Milliseconds()

	statusLabel := "success"
	if err != nil {
		statusLabel = "error"
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	observability.RequestsTotal.WithLabelValues(t.vendor, t.model, effort, statusLabel).Inc()
	observability.RequestDuration.WithLabelValues(t.vendor, t.model, effort).Observe(duration.Seconds())

	if t.tracker != nil {
		var status usagev1.Status
		if err != nil {
			status = usagev1.Status_STATUS_ERROR
		} else {
			status = usagev1.Status_STATUS_SUCCESS
		}

		// Record the image model actually used, not the wrapping chat model.
		model := t.model
		var inTokens, outTokens int
		if result != nil {
			if result.Model != "" {
				model = result.Model
			}
			inTokens = result.Usage.Input
			outTokens = result.Usage.Output
			observability.TokensTotal.WithLabelValues(t.vendor, model, "input").Add(float64(inTokens))
			observability.TokensTotal.WithLabelValues(t.vendor, model, "output").Add(float64(outTokens))
			span.SetAttributes(
				observability.AttrGenAIResponseModel.String(model),
				observability.AttrGenAIUsageInput.Int(inTokens),
				observability.AttrGenAIUsageOutput.Int(outTokens),
			)
		}
		trackMeta := map[string]string{}
		for k, v := range meta {
			trackMeta[k] = v
		}
		if err != nil {
			msg := err.Error()
			if len(msg) > 300 {
				msg = msg[:300]
			}
			trackMeta["error"] = msg
		}

		if trackErr := t.tracker.RecordUsage(
			userID,
			"llm.generate_image",
			t.vendor,
			model,
			inTokens,
			outTokens,
			durationMs,
			t.cost(ctx, inTokens, outTokens, startTime),
			status,
			trackMeta,
		); trackErr != nil {
			log.Error().
				Stack().
				Err(trackErr).
				Str("userID", userID).
				Str("actionID", "llm.generate_image").
				Str("vendor", t.vendor).
				Str("model", model).
				Msg("Failed to record usage")
		}
	}

	return result, err
}

// Judge forwards to the wrapped adapter's optional judgment capability and
// tracks usage. Adapters without one surface ErrJudgmentUnsupported.
func (t *TrackingAdapter) Judge(ctx context.Context, req *llm.JudgeRequest, userID string, meta map[string]string) (*llm.JudgeResult, error) {
	judge, ok := t.adapter.(llm.Judge)
	if !ok {
		return nil, llm.ErrJudgmentUnsupported
	}

	startTime := time.Now()
	effort := "default"
	if meta != nil && meta["effort"] != "" {
		effort = meta["effort"]
	}

	attrs := []attribute.KeyValue{
		observability.AttrGenAIProviderName.String(t.vendor),
		observability.AttrGenAIRequestModel.String(t.model),
	}
	if meta != nil {
		if r := meta["rule"]; r != "" {
			attrs = append(attrs, observability.AttrLLMProxyRule.String(r))
		}
		if k := meta["api_key"]; k != "" {
			attrs = append(attrs, observability.AttrLLMProxyClientKey.String(k))
		}
	}

	ctx, span := observability.Tracer().Start(ctx, "judge "+t.model, trace.WithAttributes(attrs...))
	defer span.End()

	observability.ActiveRequests.WithLabelValues(t.vendor, t.model).Inc()
	defer observability.ActiveRequests.WithLabelValues(t.vendor, t.model).Dec()

	result, err := judge.Judge(ctx, req, userID, meta)
	duration := time.Since(startTime)
	durationMs := duration.Milliseconds()

	statusLabel := "success"
	if err != nil {
		statusLabel = "error"
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	observability.RequestsTotal.WithLabelValues(t.vendor, t.model, effort, statusLabel).Inc()
	observability.RequestDuration.WithLabelValues(t.vendor, t.model, effort).Observe(duration.Seconds())

	if t.tracker != nil {
		status := usagev1.Status_STATUS_SUCCESS
		if err != nil {
			status = usagev1.Status_STATUS_ERROR
		}

		actionID := "llmproxy.judge"
		if req != nil && req.ActionID != "" {
			actionID = req.ActionID
		}
		model := t.model
		var inTokens, outTokens int
		if result != nil {
			if result.Model != "" {
				model = result.Model
			}
			inTokens = result.Usage.Input
			outTokens = result.Usage.Output
			observability.TokensTotal.WithLabelValues(t.vendor, model, "input").Add(float64(inTokens))
			observability.TokensTotal.WithLabelValues(t.vendor, model, "output").Add(float64(outTokens))
			span.SetAttributes(
				observability.AttrGenAIResponseModel.String(model),
				observability.AttrGenAIUsageInput.Int(inTokens),
				observability.AttrGenAIUsageOutput.Int(outTokens),
			)
		}
		trackMeta := map[string]string{}
		for k, v := range meta {
			trackMeta[k] = v
		}
		// The question count is the only shape signal worth keeping: judge
		// cost is one state plus n instructions, and n explains the spread.
		if req != nil {
			trackMeta["questions"] = strconv.Itoa(len(req.Questions))
		}
		if err != nil {
			msg := err.Error()
			if len(msg) > 300 {
				msg = msg[:300]
			}
			trackMeta["error"] = msg
		}

		if trackErr := t.tracker.RecordUsage(
			userID,
			actionID,
			t.vendor,
			model,
			inTokens,
			outTokens,
			durationMs,
			t.cost(ctx, inTokens, outTokens, startTime),
			status,
			trackMeta,
		); trackErr != nil {
			log.Error().
				Stack().
				Err(trackErr).
				Str("userID", userID).
				Str("actionID", actionID).
				Str("vendor", t.vendor).
				Str("model", model).
				Msg("Failed to record usage")
		}
	}

	return result, err
}

// EvaluateRollouts forwards to the wrapped adapter's optional rollout judgment capability and
// tracks usage. Adapters without one surface ErrJudgmentUnsupported.
func (t *TrackingAdapter) EvaluateRollouts(ctx context.Context, req *llm.EvaluateRolloutsRequest, userID string, meta map[string]string) (*llm.EvaluateRolloutsResult, error) {
	rolloutJudge, ok := t.adapter.(llm.RolloutJudge)
	if !ok {
		return nil, llm.ErrJudgmentUnsupported
	}

	startTime := time.Now()
	effort := "default"
	if meta != nil && meta["effort"] != "" {
		effort = meta["effort"]
	}

	attrs := []attribute.KeyValue{
		observability.AttrGenAIProviderName.String(t.vendor),
		observability.AttrGenAIRequestModel.String(t.model),
	}
	if meta != nil {
		if r := meta["rule"]; r != "" {
			attrs = append(attrs, observability.AttrLLMProxyRule.String(r))
		}
		if k := meta["api_key"]; k != "" {
			attrs = append(attrs, observability.AttrLLMProxyClientKey.String(k))
		}
	}

	ctx, span := observability.Tracer().Start(ctx, "rollouts "+t.model, trace.WithAttributes(attrs...))
	defer span.End()

	observability.ActiveRequests.WithLabelValues(t.vendor, t.model).Inc()
	defer observability.ActiveRequests.WithLabelValues(t.vendor, t.model).Dec()

	result, err := rolloutJudge.EvaluateRollouts(ctx, req, userID, meta)
	duration := time.Since(startTime)
	durationMs := duration.Milliseconds()

	statusLabel := "success"
	if err != nil {
		statusLabel = "error"
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	observability.RequestsTotal.WithLabelValues(t.vendor, t.model, effort, statusLabel).Inc()
	observability.RequestDuration.WithLabelValues(t.vendor, t.model, effort).Observe(duration.Seconds())

	if t.tracker != nil {
		status := usagev1.Status_STATUS_SUCCESS
		if err != nil {
			status = usagev1.Status_STATUS_ERROR
		}

		actionID := "llmproxy.rollouts"
		if req != nil && req.ActionID != "" {
			actionID = req.ActionID
		}
		model := t.model
		var inTokens, outTokens int
		if result != nil {
			if result.Model != "" {
				model = result.Model
			}
			inTokens = result.Usage.Input
			outTokens = result.Usage.Output
			observability.TokensTotal.WithLabelValues(t.vendor, model, "input").Add(float64(inTokens))
			observability.TokensTotal.WithLabelValues(t.vendor, model, "output").Add(float64(outTokens))
			span.SetAttributes(
				observability.AttrGenAIResponseModel.String(model),
				observability.AttrGenAIUsageInput.Int(inTokens),
				observability.AttrGenAIUsageOutput.Int(outTokens),
			)
		}
		trackMeta := map[string]string{}
		for k, v := range meta {
			trackMeta[k] = v
		}
		if req != nil {
			trackMeta["candidates"] = strconv.Itoa(len(req.Candidates))
			trackMeta["criteria"] = strconv.Itoa(len(req.Criteria))
		}
		if err != nil {
			msg := err.Error()
			if len(msg) > 300 {
				msg = msg[:300]
			}
			trackMeta["error"] = msg
		}

		if trackErr := t.tracker.RecordUsage(
			userID,
			actionID,
			t.vendor,
			model,
			inTokens,
			outTokens,
			durationMs,
			t.cost(ctx, inTokens, outTokens, startTime),
			status,
			trackMeta,
		); trackErr != nil {
			log.Error().
				Stack().
				Err(trackErr).
				Str("userID", userID).
				Str("actionID", actionID).
				Str("vendor", t.vendor).
				Str("model", model).
				Msg("Failed to record usage")
		}
	}

	return result, err
}

// Unwrap returns the wrapped adapter.
func (t *TrackingAdapter) Unwrap() llm.ClientAdapter {
	return t.adapter
}

// SubmitBatch delegates to the wrapped adapter if it implements BatchAdapter.
func (t *TrackingAdapter) SubmitBatch(ctx context.Context, batchID string, items []*llm.BatchSubmitItem) (string, error) {
	b, ok := t.adapter.(llm.BatchAdapter)
	if !ok {
		return "", fmt.Errorf("%s does not support batch processing", t.model)
	}
	return b.SubmitBatch(ctx, batchID, items)
}

// GetBatch delegates to the wrapped adapter if it implements BatchAdapter.
func (t *TrackingAdapter) GetBatch(ctx context.Context, vendorJobID string) (*llm.BatchJobStatus, error) {
	b, ok := t.adapter.(llm.BatchAdapter)
	if !ok {
		return nil, fmt.Errorf("%s does not support batch processing", t.model)
	}
	return b.GetBatch(ctx, vendorJobID)
}

// CancelBatch delegates to the wrapped adapter if it implements BatchAdapter.
func (t *TrackingAdapter) CancelBatch(ctx context.Context, vendorJobID string) error {
	b, ok := t.adapter.(llm.BatchAdapter)
	if !ok {
		return fmt.Errorf("%s does not support batch processing", t.model)
	}
	return b.CancelBatch(ctx, vendorJobID)
}

// FetchBatchResults delegates to the wrapped adapter if it implements BatchAdapter.
func (t *TrackingAdapter) FetchBatchResults(ctx context.Context, vendorJobID string, items []*llm.BatchSubmitItem) ([]*llm.BatchItemResult, error) {
	b, ok := t.adapter.(llm.BatchAdapter)
	if !ok {
		return nil, fmt.Errorf("%s does not support batch processing", t.model)
	}
	return b.FetchBatchResults(ctx, vendorJobID, items)
}

// CreateEmbeddings forwards to the wrapped adapter's optional embedding capability and
// tracks usage. Adapters without one surface ErrEmbeddingUnsupported.
func (t *TrackingAdapter) CreateEmbeddings(ctx context.Context, req *llm.EmbeddingRequest, userID string, meta map[string]string) (*llm.EmbeddingResult, error) {
	emb, ok := t.adapter.(llm.EmbeddingAdapter)
	if !ok {
		return nil, llm.ErrEmbeddingUnsupported
	}

	startTime := time.Now()
	effort := "default"
	if meta != nil && meta["effort"] != "" {
		effort = meta["effort"]
	}

	attrs := []attribute.KeyValue{
		observability.AttrGenAIProviderName.String(t.vendor),
		observability.AttrGenAIRequestModel.String(t.model),
	}
	if meta != nil {
		if r := meta["rule"]; r != "" {
			attrs = append(attrs, observability.AttrLLMProxyRule.String(r))
		}
		if k := meta["api_key"]; k != "" {
			attrs = append(attrs, observability.AttrLLMProxyClientKey.String(k))
		}
	}

	ctx, span := observability.Tracer().Start(ctx, "embeddings "+t.model, trace.WithAttributes(attrs...))
	defer span.End()

	observability.ActiveRequests.WithLabelValues(t.vendor, t.model).Inc()
	defer observability.ActiveRequests.WithLabelValues(t.vendor, t.model).Dec()

	result, err := emb.CreateEmbeddings(ctx, req, userID, meta)
	duration := time.Since(startTime)
	durationMs := duration.Milliseconds()

	statusLabel := "success"
	if err != nil {
		statusLabel = "error"
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	observability.RequestsTotal.WithLabelValues(t.vendor, t.model, effort, statusLabel).Inc()
	observability.RequestDuration.WithLabelValues(t.vendor, t.model, effort).Observe(duration.Seconds())

	if t.tracker != nil {
		status := usagev1.Status_STATUS_SUCCESS
		if err != nil {
			status = usagev1.Status_STATUS_ERROR
		}

		actionID := "llmproxy.embeddings"
		if req != nil && req.ActionID != "" {
			actionID = req.ActionID
		}
		model := t.model
		var inTokens int
		if result != nil {
			if result.Model != "" {
				model = result.Model
			}
			inTokens = result.Usage.Input
			observability.TokensTotal.WithLabelValues(t.vendor, model, "input").Add(float64(inTokens))
			span.SetAttributes(
				observability.AttrGenAIResponseModel.String(model),
				observability.AttrGenAIUsageInput.Int(inTokens),
			)
		}
		trackMeta := map[string]string{}
		for k, v := range meta {
			trackMeta[k] = v
		}
		trackMeta["modality"] = "embedding"
		if req != nil {
			trackMeta["inputs"] = strconv.Itoa(len(req.Inputs))
			if req.Dimensions > 0 {
				trackMeta["dimensions"] = strconv.Itoa(req.Dimensions)
			}
		}
		if err != nil {
			msg := err.Error()
			if len(msg) > 300 {
				msg = msg[:300]
			}
			trackMeta["error"] = msg
		}

		if trackErr := t.tracker.RecordUsage(
			userID,
			actionID,
			t.vendor,
			model,
			inTokens,
			0,
			durationMs,
			t.cost(ctx, inTokens, 0, startTime),
			status,
			trackMeta,
		); trackErr != nil {
			log.Error().
				Stack().
				Err(trackErr).
				Str("userID", userID).
				Str("actionID", actionID).
				Str("vendor", t.vendor).
				Str("model", model).
				Msg("Failed to record usage")
		}
	}

	return result, err
}
