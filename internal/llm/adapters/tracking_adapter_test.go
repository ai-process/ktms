package adapters

import (
	"context"
	"errors"
	"testing"

	"github.com/ai-process/llm-proxy/internal/llm"
	usagev1 "github.com/ai-process/llm-proxy/internal/usagereport/gen/usage/v1"
)

type stubAdapter struct{ resp *llm.Response }

func (s *stubAdapter) GenerateText(chat *llm.ChatContext) (*llm.Response, error) {
	return s.resp, nil
}

type capturingTracker struct {
	userID, actionID, vendor, model string
	in, out                         int
	meta                            map[string]string
	calls                           int
}

func (c *capturingTracker) RecordUsage(userID, actionID, vendor, model string, inputTokens, outputTokens int, durationMs int64, status usagev1.Status, meta map[string]string) error {
	c.userID, c.actionID, c.vendor, c.model = userID, actionID, vendor, model
	c.in, c.out = inputTokens, outputTokens
	c.meta = meta
	c.calls++
	return nil
}

// The whole point of usage meta is per-job token accounting: whatever the
// caller sets on the chat must reach the tracker verbatim.
func TestTrackingAdapterPassesCallerMeta(t *testing.T) {
	resp := &llm.Response{Usage: llm.TokensUsage{Input: 123, Output: 45}}
	tracker := &capturingTracker{}
	adapter := NewTrackingAdapter(&stubAdapter{resp: resp}, tracker, "google", "gemini-test")

	chat := llm.NewChatContext()
	chat.SetActionID("app.test")
	chat.SetCaller(llm.Caller{
		UserID: "batch-worker",
		Meta:   map[string]string{"job_id": "job-1", "gentask_id": "task-2"},
	})

	if _, err := adapter.GenerateText(chat); err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if tracker.calls != 1 {
		t.Fatalf("expected 1 tracker call, got %d", tracker.calls)
	}
	if tracker.userID != "batch-worker" || tracker.actionID != "app.test" {
		t.Fatalf("attribution lost: %q %q", tracker.userID, tracker.actionID)
	}
	if tracker.in != 123 || tracker.out != 45 {
		t.Fatalf("tokens lost: %d/%d", tracker.in, tracker.out)
	}
	if tracker.meta["job_id"] != "job-1" || tracker.meta["gentask_id"] != "task-2" {
		t.Fatalf("meta lost: %v", tracker.meta)
	}
}

// Without a caller the tracker still records, with nil meta.
func TestTrackingAdapterNilMeta(t *testing.T) {
	tracker := &capturingTracker{}
	adapter := NewTrackingAdapter(&stubAdapter{resp: &llm.Response{}}, tracker, "google", "gemini-test")

	chat := llm.NewChatContext()
	chat.SetInternalUserID("someone")
	if _, err := adapter.GenerateText(chat); err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if tracker.calls != 1 || tracker.meta != nil {
		t.Fatalf("expected 1 call with nil meta, got calls=%d meta=%v", tracker.calls, tracker.meta)
	}
}

type stubStreamAdapter struct {
	stubAdapter
	stream func(ctx context.Context, chat *llm.ChatContext, onChunk func(llm.StreamChunk) error) error
}

func (s *stubStreamAdapter) GenerateTextStream(ctx context.Context, chat *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
	return s.stream(ctx, chat, onChunk)
}

func TestTrackingAdapterSupportsStreaming(t *testing.T) {
	tracker := &capturingTracker{}
	nonStreaming := NewTrackingAdapter(&stubAdapter{}, tracker, "vendor", "model-nostream")
	if nonStreaming.SupportsStreaming() {
		t.Fatal("expected SupportsStreaming == false for non-streaming adapter")
	}

	err := nonStreaming.GenerateTextStream(context.Background(), llm.NewChatContext(), func(llm.StreamChunk) error { return nil })
	if !errors.Is(err, llm.ErrStreamingNotSupported) {
		t.Fatalf("expected ErrStreamingNotSupported, got %v", err)
	}

	streaming := NewTrackingAdapter(&stubStreamAdapter{}, tracker, "vendor", "model-stream")
	if !streaming.SupportsStreaming() {
		t.Fatal("expected SupportsStreaming == true for stream adapter")
	}
}

func TestTrackingAdapterStreamAbortedWithoutUsage(t *testing.T) {
	tracker := &capturingTracker{}
	streamAdapter := &stubStreamAdapter{
		stream: func(_ context.Context, _ *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
			if err := onChunk(llm.StreamChunk{Delta: "some streamed words"}); err != nil {
				return err
			}
			return errors.New("disconnected mid-stream")
		},
	}
	adapter := NewTrackingAdapter(streamAdapter, tracker, "openai", "gpt-4")
	chat := llm.NewChatContext()
	chat.SetInternalUserID("u1")
	chat.AddUserMessage("hello world")

	_ = adapter.GenerateTextStream(context.Background(), chat, func(llm.StreamChunk) error { return nil })
	if tracker.calls != 1 {
		t.Fatalf("expected 1 tracker call, got %d", tracker.calls)
	}
	if tracker.in <= 0 || tracker.out <= 0 {
		t.Errorf("expected estimated usage recorded on aborted stream, got in=%d out=%d", tracker.in, tracker.out)
	}
}
