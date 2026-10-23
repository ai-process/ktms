package adapters

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ai-process/ktms/internal/llm"
	usagev1 "github.com/ai-process/ktms/internal/usagereport/gen/usage/v1"
)

type stubAdapter struct{ resp *llm.Response }

func (s *stubAdapter) GenerateText(ctx context.Context, chat *llm.ChatContext) (*llm.Response, error) {
	return s.resp, nil
}

type capturingTracker struct {
	usage                           llm.TokensUsage
	userID, actionID, vendor, model string
	in, out                         int
	cost                            int64
	meta                            map[string]string
	calls                           int
}

func (c *capturingTracker) RecordUsage(userID, actionID, vendor, model string, usage llm.TokensUsage, durationMs, costNanoUSD int64, status usagev1.Status, meta map[string]string) error {
	c.userID, c.actionID, c.vendor, c.model = userID, actionID, vendor, model
	c.in, c.out = usage.Input, usage.Output
	c.usage = usage
	c.cost = costNanoUSD
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

	if _, err := adapter.GenerateText(context.Background(), chat); err != nil {
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

// Every event carries the cost the pricer derives from its own usage and start time.
func TestTrackingAdapterStampsCost(t *testing.T) {
	resp := &llm.Response{Usage: llm.TokensUsage{Input: 100, Output: 10}}
	tracker := &capturingTracker{}
	var pricedAt time.Time
	adapter := NewTrackingAdapter(&stubAdapter{resp: resp}, tracker, "openai", "gpt-test").
		WithPricer(func(u llm.TokensUsage, startedAt time.Time) int64 {
			pricedAt = startedAt
			return int64(u.Input*7 + u.Output*11)
		})

	before := time.Now()
	if _, err := adapter.GenerateText(context.Background(), llm.NewChatContext()); err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if tracker.cost != 100*7+10*11 {
		t.Fatalf("cost = %d, want 810", tracker.cost)
	}
	if pricedAt.Before(before) || pricedAt.After(time.Now()) {
		t.Fatalf("priced at %s, want the call's start time", pricedAt)
	}

	unpriced := NewTrackingAdapter(&stubAdapter{resp: resp}, tracker, "openai", "gpt-test")
	if _, err := unpriced.GenerateText(context.Background(), llm.NewChatContext()); err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if tracker.cost != 0 {
		t.Fatalf("adapter without a pricer must report 0, got %d", tracker.cost)
	}
}

// Without a caller the tracker still records, with nil meta.
func TestTrackingAdapterNilMeta(t *testing.T) {
	tracker := &capturingTracker{}
	adapter := NewTrackingAdapter(&stubAdapter{resp: &llm.Response{}}, tracker, "google", "gemini-test")

	chat := llm.NewChatContext()
	chat.SetInternalUserID("someone")
	if _, err := adapter.GenerateText(context.Background(), chat); err != nil {
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

func TestTrackingAdapterStreamDisconnectBeforeFirstChunk(t *testing.T) {
	tracker := &capturingTracker{}
	streamAdapter := &stubStreamAdapter{
		stream: func(ctx context.Context, _ *llm.ChatContext, _ func(llm.StreamChunk) error) error {
			return ctx.Err()
		},
	}
	adapter := NewTrackingAdapter(streamAdapter, tracker, "openai", "gpt-4")
	chat := llm.NewChatContext()
	chat.SetInternalUserID("u1")
	chat.AddUserMessage("hello world")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = adapter.GenerateTextStream(ctx, chat, func(llm.StreamChunk) error { return nil })
	if tracker.calls != 1 {
		t.Fatalf("expected 1 tracker call, got %d", tracker.calls)
	}
	if tracker.in <= 0 {
		t.Errorf("expected estimated input tokens recorded on pre-chunk disconnect, got in=%d", tracker.in)
	}
}

func TestTrackingAdapterShadowZeroCost(t *testing.T) {
	resp := &llm.Response{Usage: llm.TokensUsage{Input: 100, Output: 10}}
	tracker := &capturingTracker{}
	adapter := NewTrackingAdapter(&stubAdapter{resp: resp}, tracker, "openai", "gpt-test").
		WithPricer(func(u llm.TokensUsage, startedAt time.Time) int64 {
			return int64(u.Input*7 + u.Output*11)
		})

	// Client sending forged is_shadow or shadow metadata without internal shadow context must pay full cost.
	chat := llm.NewChatContext()
	chat.SetCaller(llm.Caller{
		UserID: "user-1",
		Meta:   map[string]string{"is_shadow": "true", "shadow": "true"},
	})
	if _, err := adapter.GenerateText(context.Background(), chat); err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	expectedFullCost := int64(100*7 + 10*11)
	if tracker.cost != expectedFullCost {
		t.Fatalf("forged shadow request cost = %d, want full cost %d", tracker.cost, expectedFullCost)
	}

	// Internal shadow traffic marked on context gets 0 cost.
	chat2 := llm.NewChatContext()
	chat2.SetCaller(llm.Caller{UserID: "user-2"})
	shadowCtx := llm.WithShadowTraffic(context.Background())
	if _, err := adapter.GenerateText(shadowCtx, chat2); err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if tracker.cost != 0 {
		t.Fatalf("shadow request cost via context = %d, want 0", tracker.cost)
	}
}

func TestRequestStatusSeparatesCanceledCalls(t *testing.T) {
	for err, want := range map[error]string{
		context.Canceled: "canceled",
		errors.Join(errors.New("x"), context.Canceled): "canceled",
		context.DeadlineExceeded:                       "error",
		errors.New("vendor 500"):                       "error",
	} {
		if got := requestStatus(err); got != want {
			t.Errorf("requestStatus(%v) = %q, want %q", err, got, want)
		}
	}
}
