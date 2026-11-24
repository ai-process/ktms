package adapters

import (
	"context"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/observability"
	usagev1 "github.com/ai-process/ktms/internal/usagereport/gen/usage/v1"
)

// translateActionID attributes Translate calls that name no action.
const translateActionID = "translate"

// Translate uses the wrapped adapter's native engine when it has one and
// otherwise translates by prompt over its GenerateText, untracked, so each
// attempt is one usage event carrying tokens, characters and their price.
func (t *TrackingAdapter) Translate(ctx context.Context, req *llm.TranslateRequest, userID string, meta map[string]string) (*llm.TranslateResult, error) {
	startTime := time.Now()
	attrs := []attribute.KeyValue{
		observability.AttrGenAIProviderName.String(t.vendor),
		observability.AttrGenAIRequestModel.String(t.model),
	}
	if r := meta["rule"]; r != "" {
		attrs = append(attrs, observability.AttrKTMSRule.String(r))
	}
	if k := meta["api_key"]; k != "" {
		attrs = append(attrs, observability.AttrKTMSClientKey.String(k))
	}
	ctx, span := observability.Tracer().Start(ctx, "translate "+t.model, trace.WithAttributes(attrs...))
	defer span.End()
	observability.ActiveRequests.WithLabelValues(t.vendor, t.model).Inc()
	defer observability.ActiveRequests.WithLabelValues(t.vendor, t.model).Dec()

	var (
		result *llm.TranslateResult
		err    error
	)
	if native, ok := t.adapter.(llm.Translator); ok {
		result, err = native.Translate(ctx, req, userID, meta)
	} else {
		result, err = llm.TranslateByPrompt(ctx, t.adapter, req, userID, meta)
	}
	duration := time.Since(startTime)

	effort := "default"
	if e := meta["effort"]; e != "" {
		effort = e
	}
	statusLabel := "success"
	if err != nil {
		statusLabel = requestStatus(err)
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	observability.RequestsTotal.WithLabelValues(t.vendor, t.model, effort, statusLabel).Inc()
	observability.RequestDuration.WithLabelValues(t.vendor, t.model, effort).Observe(duration.Seconds())

	model := t.model
	var usage llm.TokensUsage
	if result != nil {
		if result.Model != "" {
			model = result.Model
		}
		usage = result.Usage
		observability.TokensTotal.WithLabelValues(t.vendor, model, "input").Add(float64(usage.Input))
		observability.TokensTotal.WithLabelValues(t.vendor, model, "output").Add(float64(usage.Output))
		span.SetAttributes(
			observability.AttrGenAIResponseModel.String(model),
			observability.AttrGenAIUsageInput.Int(usage.Input),
			observability.AttrGenAIUsageOutput.Int(usage.Output),
		)
	}
	if t.tracker == nil {
		return result, err
	}

	status := usagev1.Status_STATUS_SUCCESS
	if err != nil {
		status = usagev1.Status_STATUS_ERROR
	}
	actionID := translateActionID
	if req != nil && req.ActionID != "" {
		actionID = req.ActionID
	}
	trackMeta := make(map[string]string, len(meta)+5)
	for k, v := range meta {
		trackMeta[k] = v
	}
	// tokenmeter has no character field: the count rides in meta, its price in cost.
	trackMeta["characters"] = strconv.Itoa(usage.Characters)
	if req != nil {
		trackMeta["texts"] = strconv.Itoa(len(req.Texts))
		trackMeta["target_language"] = req.TargetLanguage
		if req.SourceLanguage != "" {
			trackMeta["source_language"] = req.SourceLanguage
		}
	}
	if err != nil {
		// As in the gateway's budget: a failed attempt bills tokens, never characters.
		usage.Characters = 0
		msg := err.Error()
		if len(msg) > 300 {
			msg = msg[:300]
		}
		trackMeta["error"] = msg
	}
	if trackErr := t.tracker.RecordUsage(userID, actionID, t.vendor, model, usage, duration.Milliseconds(),
		t.cost(ctx, usage, startTime), status, trackMeta); trackErr != nil {
		log.Error().Stack().Err(trackErr).Str("userID", userID).Str("actionID", actionID).
			Str("vendor", t.vendor).Str("model", model).Msg("Failed to record usage")
	}
	return result, err
}
