package adapters

import (
	"context"
	"fmt"
	"github.com/ai-process/llm-proxy/internal/llm"
	usagev1 "github.com/ai-process/llm-proxy/internal/usagereport/gen/usage/v1"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"
)

// TrackingAdapter wraps a ClientAdapter and automatically tracks usage
type TrackingAdapter struct {
	adapter llm.ClientAdapter
	tracker llm.UsageTracker
	vendor  string
	model   string
}

// NewTrackingAdapter creates a new tracking adapter that wraps an existing adapter
func NewTrackingAdapter(adapter llm.ClientAdapter, tracker llm.UsageTracker, vendor, model string) *TrackingAdapter {
	return &TrackingAdapter{
		adapter: adapter,
		tracker: tracker,
		vendor:  vendor,
		model:   model,
	}
}

// GenerateText generates text and tracks usage
func (t *TrackingAdapter) GenerateText(chat *llm.ChatContext) (*llm.Response, error) {
	startTime := time.Now()

	// Extract userID from context
	var userID string
	if chat != nil {
		userID = chat.GetInternalUserID()
	}

	// Extract actionID from context if available
	actionID := "llm.generate_text" // Default action ID
	var meta map[string]string
	if chat != nil {
		if actionIDFromContext := chat.GetActionID(); actionIDFromContext != "" {
			actionID = actionIDFromContext
		}
		meta = chat.GetMeta()
	}

	resp, err := t.adapter.GenerateText(chat)
	durationMs := time.Since(startTime).Milliseconds()

	// Always proceed with the request, but track usage if tracker is available
	if t.tracker != nil {
		var status usagev1.Status
		if err != nil {
			status = usagev1.Status_STATUS_ERROR
		} else {
			status = usagev1.Status_STATUS_SUCCESS
		}

		// Track usage if we have a response (even on error, we might have partial usage data)
		if resp != nil {
			if trackErr := t.tracker.RecordUsage(
				userID,
				actionID,
				t.vendor,
				t.model,
				resp.Usage.Input,
				resp.Usage.Output,
				durationMs,
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
			// Track error even without response
			if trackErr := t.tracker.RecordUsage(
				userID,
				actionID,
				t.vendor,
				t.model,
				0,
				0,
				durationMs,
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

// GenerateTextStream forwards streaming text generation and tracks usage upon completion.
func (t *TrackingAdapter) GenerateTextStream(ctx context.Context, chat *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
	streamer, ok := t.adapter.(llm.StreamAdapter)
	if !ok {
		return fmt.Errorf("adapter for %s does not support streaming", t.model)
	}

	startTime := time.Now()
	var userID string
	if chat != nil {
		userID = chat.GetInternalUserID()
	}

	actionID := "llm.generate_text"
	var meta map[string]string
	if chat != nil {
		if actionIDFromContext := chat.GetActionID(); actionIDFromContext != "" {
			actionID = actionIDFromContext
		}
		meta = chat.GetMeta()
	}

	var lastUsage llm.TokensUsage
	wrappedChunk := func(chunk llm.StreamChunk) error {
		if chunk.Usage != nil {
			lastUsage = *chunk.Usage
		}
		return onChunk(chunk)
	}

	err := streamer.GenerateTextStream(ctx, chat, wrappedChunk)
	durationMs := time.Since(startTime).Milliseconds()

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
func (t *TrackingAdapter) SynthesizeSpeech(text, language, userID string, meta map[string]string) (*llm.SpeechResult, error) {
	startTime := time.Now()

	synth, ok := t.adapter.(llm.SpeechSynthesizer)
	if !ok {
		return nil, fmt.Errorf("%s does not synthesize speech", t.model)
	}
	result, err := synth.SynthesizeSpeech(text, language, userID, meta)
	durationMs := time.Since(startTime).Milliseconds()

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
func (t *TrackingAdapter) GenerateImage(prompt, userID string, meta map[string]string) (*llm.ImageResult, error) {
	gen, ok := t.adapter.(llm.ImageGenerator)
	if !ok {
		return nil, llm.ErrImageGenerationUnsupported
	}

	startTime := time.Now()
	result, err := gen.GenerateImage(prompt, userID, meta)
	durationMs := time.Since(startTime).Milliseconds()

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
	result, err := judge.Judge(ctx, req, userID, meta)
	durationMs := time.Since(startTime).Milliseconds()

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
