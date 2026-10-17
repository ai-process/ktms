package llm

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func TestEstimateInputTokens_Multimodal(t *testing.T) {
	chat := NewChatContext()

	// Text only: "hello world" = 11 chars -> 11/4 + 1 = 3 tokens
	chat.AddUserMessage("hello world")
	if tokens := chat.EstimateInputTokens(); tokens != 3 {
		t.Fatalf("expected 3 tokens for text only, got %d", tokens)
	}

	// Add user message with 1MB image payload
	largeData := bytes.Repeat([]byte{0x42}, 1024*1024)

	chat2 := NewChatContext()
	chat2.AddUserMessageWithParts("", []*ContentPart{
		{
			Type:   ContentPartTypeImage,
			Data:   largeData,
			Detail: ImageDetailLow,
		},
	})
	// Image detail low should add 85 tokens, not 1MB/4 tokens
	if tokens := chat2.EstimateInputTokens(); tokens != 86 { // 85 + 1
		t.Fatalf("expected 86 tokens for low-detail image, got %d", tokens)
	}

	chat3 := NewChatContext()
	chat3.AddUserMessageWithParts("", []*ContentPart{
		{
			Type:   ContentPartTypeImage,
			Data:   largeData,
			Detail: ImageDetailHigh,
		},
		{
			Type: ContentPartTypeAudio,
			Data: largeData,
		},
		{
			Type: ContentPartTypeDocument,
			Data: largeData,
		},
	})
	// 1000 (high detail image) + 200 (audio) + 500 (doc) + 1 = 1701 tokens
	if tokens := chat3.EstimateInputTokens(); tokens != 1701 {
		t.Fatalf("expected 1701 tokens for multimodal parts, got %d", tokens)
	}
}

func TestNoPIIInLogs(t *testing.T) {
	var buf bytes.Buffer
	origLogger := log.Logger
	log.Logger = zerolog.New(&buf)
	defer func() { log.Logger = origLogger }()

	chat := NewChatContext()
	sensitiveText := "confidential user payload credit-card-12345 user@example.com"
	chat.AddUserMessage(sensitiveText)
	chat.AddAssistantMessage(sensitiveText)
	chat.AddToolMessage("call-1", sensitiveText)

	if strings.Contains(buf.String(), sensitiveText) {
		t.Fatalf("detected sensitive payload in logger output: %s", buf.String())
	}
}

func TestChatContext_Clone(t *testing.T) {
	orig := NewChatContext()
	orig.SetMeta(map[string]string{"foo": "bar"})
	orig.AddUserMessage("hello")
	orig.SetActionID("act-1")

	clone := orig.Clone()
	if clone == nil {
		t.Fatal("expected non-nil clone")
	}

	cloneMeta := clone.GetMeta()
	cloneMeta["is_shadow"] = "true"
	clone.SetMeta(cloneMeta)
	clone.AddAssistantMessage("world")

	if orig.GetMeta()["is_shadow"] != "" {
		t.Errorf("mutation of clone meta leaked to orig: %v", orig.GetMeta())
	}
	if len(orig.GetMessages()) != 1 {
		t.Errorf("mutation of clone messages affected orig: len = %d", len(orig.GetMessages()))
	}
	if len(clone.GetMessages()) != 2 {
		t.Errorf("clone messages len = %d, want 2", len(clone.GetMessages()))
	}
}
