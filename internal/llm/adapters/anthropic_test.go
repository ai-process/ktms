package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/ai-process/ktms/internal/llm"
)

func newTestAnthropicAdapter(serverURL string, model string) *AnthropicAdapter {
	noThrottle := llm.NewThrottleControl(0, 60)
	client := anthropic.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(serverURL),
	)
	return NewAnthropicAdapter(&client, model, noThrottle)
}

func TestAnthropicModelMaxOutputTokens(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"claude-3-5-sonnet-20241022", 8192},
		{"claude-3-5-haiku-20241022", 8192},
		{"claude-3-7-sonnet-20250219", 8192},
		{"claude-3-opus-20240229", 4096},
		{"claude-3-sonnet-20240229", 4096},
		{"claude-3-haiku-20240307", 4096},
		{"unknown-claude", 8192},
	}

	for _, tc := range cases {
		if got := modelMaxOutputTokens(tc.model); got != tc.want {
			t.Errorf("modelMaxOutputTokens(%q) = %d, want %d", tc.model, got, tc.want)
		}
	}
}

func TestAnthropicContextToMessages(t *testing.T) {
	adapter := newTestAnthropicAdapter("http://localhost", "claude-3-5-sonnet")

	chat := llm.NewChatContext()
	chat.AddSystemMessage("system prompt")
	chat.AddUserMessage("hello")
	chat.AddAssistantMessage("how can I help?")
	chat.AddAssistantMessageWithToolCalls("", []*llm.ToolCall{
		{
			ID:   "call_1",
			Type: "function",
			Function: llm.FunctionCall{
				Name:      "get_weather",
				Arguments: `{"city":"Berlin"}`,
			},
		},
		{
			ID:   "call_2",
			Type: "function",
			Function: llm.FunctionCall{
				Name:      "get_forecast",
				Arguments: `{"days":1}`,
			},
		},
	})
	chat.AddToolMessage("call_1", `{"temp":18}`)
	chat.AddToolMessage("call_2", `{"forecast":"rain"}`)
	chat.AddUserMessage("and what about tomorrow?")

	msgs, _ := adapter.contextToMessages(chat.GetMessages(), "")

	// Expect alternating turns:
	// 1. user: "hello"
	// 2. assistant: "how can I help?" + 2 tool_use blocks
	// 3. user: tool_result 1 + tool_result 2 + "and what about tomorrow?"
	if len(msgs) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(msgs))
	}

	if msgs[0].Role != anthropic.MessageParamRoleUser {
		t.Errorf("turn 0 role = %v, want user", msgs[0].Role)
	}
	if len(msgs[0].Content) != 1 || msgs[0].Content[0].OfText == nil || msgs[0].Content[0].OfText.Text != "hello" {
		t.Errorf("turn 0 content unexpected: %+v", msgs[0].Content)
	}

	if msgs[1].Role != anthropic.MessageParamRoleAssistant {
		t.Errorf("turn 1 role = %v, want assistant", msgs[1].Role)
	}
	if len(msgs[1].Content) != 3 {
		t.Fatalf("turn 1 expected 3 content blocks (text + 2 tool_use), got %d", len(msgs[1].Content))
	}

	if msgs[2].Role != anthropic.MessageParamRoleUser {
		t.Errorf("turn 2 role = %v, want user", msgs[2].Role)
	}
	// 2 tool results + 1 user text block
	if len(msgs[2].Content) != 3 {
		t.Fatalf("turn 2 expected 3 content blocks, got %d", len(msgs[2].Content))
	}
	if msgs[2].Content[0].OfToolResult == nil || msgs[2].Content[0].OfToolResult.ToolUseID != "call_1" {
		t.Errorf("turn 2 block 0 tool_result = %+v", msgs[2].Content[0])
	}
	if msgs[2].Content[1].OfToolResult == nil || msgs[2].Content[1].OfToolResult.ToolUseID != "call_2" {
		t.Errorf("turn 2 block 1 tool_result = %+v", msgs[2].Content[1])
	}
	if msgs[2].Content[2].OfText == nil || msgs[2].Content[2].OfText.Text != "and what about tomorrow?" {
		t.Errorf("turn 2 block 2 text = %+v", msgs[2].Content[2])
	}
}

func TestAnthropicContextToMessages_InstructionOnly(t *testing.T) {
	adapter := newTestAnthropicAdapter("http://localhost", "claude-3-5-sonnet")

	chat := llm.NewChatContext()
	chat.AddSystemMessage("write a poem")

	msgs, systemConsumed := adapter.contextToMessages(chat.GetMessages(), "write a poem")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(msgs))
	}
	if !systemConsumed {
		t.Error("expected system instruction to be marked consumed")
	}
	if msgs[0].Role != anthropic.MessageParamRoleUser {
		t.Errorf("role = %v, want user", msgs[0].Role)
	}
	if msgs[0].Content[0].OfText == nil || msgs[0].Content[0].OfText.Text != "write a poem" {
		t.Errorf("content = %+v, want 'write a poem'", msgs[0].Content[0])
	}
}

func TestAnthropicContextToMessages_PrependsContinueWhenStartingWithAssistant(t *testing.T) {
	adapter := newTestAnthropicAdapter("http://localhost", "claude-3-5-sonnet")

	chat := llm.NewChatContext()
	chat.AddAssistantMessage("initial bot turn")

	msgs, _ := adapter.contextToMessages(chat.GetMessages(), "")
	if len(msgs) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(msgs))
	}
	if msgs[0].Role != anthropic.MessageParamRoleUser {
		t.Errorf("turn 0 role = %v, want user", msgs[0].Role)
	}
	if msgs[0].Content[0].OfText == nil || msgs[0].Content[0].OfText.Text != "Please continue." {
		t.Errorf("turn 0 text = %+v, want 'Please continue.'", msgs[0].Content[0])
	}
	if msgs[1].Role != anthropic.MessageParamRoleAssistant {
		t.Errorf("turn 1 role = %v, want assistant", msgs[1].Role)
	}
}

func TestAnthropicContextToMessages_UnmatchedToolFallsBackToText(t *testing.T) {
	adapter := newTestAnthropicAdapter("http://localhost", "claude-3-5-sonnet")

	chat := llm.NewChatContext()
	chat.AddToolMessage("orphan_call", `{"status":"ok"}`)

	msgs, _ := adapter.contextToMessages(chat.GetMessages(), "")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(msgs))
	}
	if msgs[0].Role != anthropic.MessageParamRoleUser {
		t.Errorf("role = %v, want user", msgs[0].Role)
	}
	// Must fall back to text block to avoid Anthropic 400 rejection
	if msgs[0].Content[0].OfText == nil || msgs[0].Content[0].OfText.Text != `{"status":"ok"}` {
		t.Errorf("expected text block for unmatched tool, got %+v", msgs[0].Content[0])
	}
}

func TestAnthropicContextToMessages_EmptyToolCallIDMatchesByIndex(t *testing.T) {
	adapter := newTestAnthropicAdapter("http://localhost", "claude-3-5-sonnet")

	chat := llm.NewChatContext()
	chat.AddAssistantMessageWithToolCalls("", []*llm.ToolCall{
		{
			ID:   "",
			Type: "function",
			Function: llm.FunctionCall{
				Name:      "get_weather",
				Arguments: `{"city":"Berlin"}`,
			},
		},
	})
	chat.AddToolMessage("", `{"temp":18}`)

	msgs, _ := adapter.contextToMessages(chat.GetMessages(), "")
	// Expect 3 turns: prepended user "Please continue.", assistant tool_use, user tool_result
	if len(msgs) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(msgs))
	}
	if msgs[0].Role != anthropic.MessageParamRoleUser || msgs[0].Content[0].OfText == nil || msgs[0].Content[0].OfText.Text != "Please continue." {
		t.Errorf("turn 0 unexpected: %+v", msgs[0])
	}
	assistantTurn := msgs[1]
	userTurn := msgs[2]
	if len(assistantTurn.Content) == 0 || assistantTurn.Content[0].OfToolUse == nil {
		t.Fatalf("expected tool_use block in assistant turn, got %+v", assistantTurn.Content)
	}
	toolUseID := assistantTurn.Content[0].OfToolUse.ID
	if toolUseID != "call_get_weather_0" {
		t.Errorf("tool_use ID = %q, want 'call_get_weather_0'", toolUseID)
	}
	if len(userTurn.Content) == 0 || userTurn.Content[0].OfToolResult == nil {
		t.Fatalf("expected tool_result block in user turn, got %+v", userTurn.Content)
	}
	if userTurn.Content[0].OfToolResult.ToolUseID != toolUseID {
		t.Errorf("tool_result ID = %q, want %q", userTurn.Content[0].OfToolResult.ToolUseID, toolUseID)
	}
}

func TestAnthropicBuildParams(t *testing.T) {
	adapter := newTestAnthropicAdapter("http://localhost", "claude-3-5-sonnet")

	chat := llm.NewChatContext()
	chat.AddSystemMessage("system instruction")
	chat.AddUserMessage("hi")
	chat.SetMaxOutputTokens(1024)

	schema := &llm.ResponseSchema{
		Type: llm.SchemaPropertyTypeObject,
		Properties: map[string]*llm.SchemaProperty{
			"answer":   {Type: llm.SchemaPropertyTypeString},
			"optional": {Type: llm.SchemaPropertyTypeString},
		},
		Required: []string{"answer"},
	}
	chat.SetResponseSchema(schema)

	toolSchema := &llm.ResponseSchema{
		Type: llm.SchemaPropertyTypeObject,
		Properties: map[string]*llm.SchemaProperty{
			"query": {Type: llm.SchemaPropertyTypeString},
			"limit": {Type: llm.SchemaPropertyTypeInteger},
		},
		Required: []string{"query"},
	}

	nonStrictToolSchema := &llm.ResponseSchema{
		Type: llm.SchemaPropertyTypeObject,
		Properties: map[string]*llm.SchemaProperty{
			"filter": {Type: llm.SchemaPropertyTypeString},
		},
		Required: []string{"filter"},
	}

	chat.SetTools([]*llm.Tool{
		{
			Type: "function",
			Function: llm.FunctionDefinition{
				Name:        "search",
				Description: "search tool",
				Strict:      true,
				Parameters:  toolSchema,
			},
		},
		{
			Type: "function",
			Function: llm.FunctionDefinition{
				Name:        "browse",
				Description: "browse tool",
				Strict:      false,
				Parameters:  nonStrictToolSchema,
			},
		},
	})
	chat.SetToolChoice(&llm.ToolChoice{Mode: "specific", SpecificFunctionName: "search"})

	params := adapter.buildParams(chat, chat.GetMaxOutputTokens())

	if params.MaxTokens != 1024 {
		t.Errorf("max tokens = %d, want 1024", params.MaxTokens)
	}
	if len(params.System) != 1 || params.System[0].Text != "system instruction" {
		t.Errorf("system = %+v", params.System)
	}
	if len(params.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(params.Tools))
	}
	if params.Tools[0].OfTool.Name != "search" {
		t.Errorf("tool name = %s, want search", params.Tools[0].OfTool.Name)
	}

	// Verify schemas were cloned and original ResponseSchema is NOT mutated
	if len(schema.Required) != 1 || schema.Required[0] != "answer" {
		t.Errorf("original ResponseSchema was mutated: %+v", schema.Required)
	}
	if len(toolSchema.Required) != 1 || toolSchema.Required[0] != "query" {
		t.Errorf("original toolSchema was mutated: %+v", toolSchema.Required)
	}

	// Strict tool must have additionalProperties: false
	strictToolInput := params.Tools[0].OfTool.InputSchema
	if strictToolInput.ExtraFields["additionalProperties"] != false {
		t.Errorf("expected additionalProperties: false for strict tool, got %+v", strictToolInput.ExtraFields)
	}

	// Non-strict tool must NOT have additionalProperties: false
	nonStrictToolInput := params.Tools[1].OfTool.InputSchema
	if nonStrictToolInput.ExtraFields != nil && nonStrictToolInput.ExtraFields["additionalProperties"] == false {
		t.Errorf("non-strict tool should not have additionalProperties: false")
	}
}

func TestAnthropicBuildParamsToolChoices(t *testing.T) {
	adapter := newTestAnthropicAdapter("http://localhost", "claude-3-5-sonnet")

	modes := []struct {
		mode string
		fn   func(p anthropic.MessageNewParams) bool
	}{
		{"auto", func(p anthropic.MessageNewParams) bool { return p.ToolChoice.OfAuto != nil }},
		{"required", func(p anthropic.MessageNewParams) bool { return p.ToolChoice.OfAny != nil }},
		{"none", func(p anthropic.MessageNewParams) bool { return p.ToolChoice.OfNone != nil }},
	}

	for _, m := range modes {
		chat := llm.NewChatContext()
		chat.AddUserMessage("hi")
		chat.SetToolChoice(&llm.ToolChoice{Mode: m.mode})
		params := adapter.buildParams(chat, 0)
		if !m.fn(params) {
			t.Errorf("mode %s was not properly mapped: %+v", m.mode, params.ToolChoice)
		}
		if params.MaxTokens != 8192 {
			t.Errorf("expected default max tokens 8192 for Claude 3.5 Sonnet, got %d", params.MaxTokens)
		}
	}
}

func TestAnthropicMapStopReason(t *testing.T) {
	cases := map[anthropic.StopReason]string{
		anthropic.StopReasonEndTurn:                    "stop",
		anthropic.StopReasonStopSequence:               "stop",
		anthropic.StopReasonToolUse:                    "tool_calls",
		anthropic.StopReasonMaxTokens:                  "length",
		anthropic.StopReasonRefusal:                    "content_filter",
		anthropic.StopReasonModelContextWindowExceeded: "model_context_window_exceeded",
	}

	for reason, want := range cases {
		if got := mapAnthropicStopReason(reason); got != want {
			t.Errorf("mapAnthropicStopReason(%v) = %q, want %q", reason, got, want)
		}
	}
}

func TestAnthropicGenerateText_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "msg_test1",
			"type":        "message",
			"role":        "assistant",
			"model":       "claude-3-5-sonnet",
			"content":     []map[string]any{{"type": "text", "text": "Hello world!"}},
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens":  15,
				"output_tokens": 10,
			},
		})
	}))
	defer server.Close()

	adapter := newTestAnthropicAdapter(server.URL, "claude-3-5-sonnet")
	chat := llm.NewChatContext()
	chat.AddUserMessage("hello")

	resp, err := adapter.GenerateText(context.Background(), chat)
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}

	if len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
	}
	choice := resp.Choices[0]
	if choice.Text != "Hello world!" {
		t.Errorf("choice text = %q, want 'Hello world!'", choice.Text)
	}
	if choice.FinishReason != "stop" {
		t.Errorf("finish reason = %q, want 'stop'", choice.FinishReason)
	}
	if resp.Usage.Input != 15 || resp.Usage.Output != 10 {
		t.Errorf("usage = %+v, want 15 in, 10 out", resp.Usage)
	}
}

func TestAnthropicGenerateText_ToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "msg_test2",
			"type":  "message",
			"role":  "assistant",
			"model": "claude-3-5-sonnet",
			"content": []map[string]any{
				{
					"type":  "tool_use",
					"id":    "toolu_abc",
					"name":  "get_weather",
					"input": map[string]any{"city": "Paris"},
				},
			},
			"stop_reason": "tool_use",
			"usage": map[string]any{
				"input_tokens":  20,
				"output_tokens": 8,
			},
		})
	}))
	defer server.Close()

	adapter := newTestAnthropicAdapter(server.URL, "claude-3-5-sonnet")
	chat := llm.NewChatContext()
	chat.AddUserMessage("weather in Paris?")

	resp, err := adapter.GenerateText(context.Background(), chat)
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}

	if len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
	}
	choice := resp.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q, want 'tool_calls'", choice.FinishReason)
	}
	if len(choice.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(choice.ToolCalls))
	}
	tc := choice.ToolCalls[0]
	if tc.ID != "toolu_abc" || tc.Function.Name != "get_weather" {
		t.Errorf("tool call = %+v", tc)
	}
	if tc.Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("arguments = %q, want '{\"city\":\"Paris\"}'", tc.Function.Arguments)
	}
}

func TestAnthropicGenerateText_CeilingRetry(t *testing.T) {
	attempts := 0
	var attempt2MaxTokens int64
	var attempt2System string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		var reqBody struct {
			MaxTokens int64 `json:"max_tokens"`
			System    []struct {
				Text string `json:"text"`
			} `json:"system"`
		}
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		if attempts == 2 {
			attempt2MaxTokens = reqBody.MaxTokens
			if len(reqBody.System) > 0 {
				attempt2System = reqBody.System[0].Text
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if attempts == 1 {
			// First attempt exhausted ceiling
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":          "msg_try1",
				"type":        "message",
				"role":        "assistant",
				"model":       "claude-3-5-sonnet",
				"content":     []map[string]any{{"type": "text", "text": "partially written..."}},
				"stop_reason": "max_tokens",
				"usage": map[string]any{
					"input_tokens":  10,
					"output_tokens": 5,
				},
			})
			return
		}
		// Second attempt uncapped succeeded
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "msg_try2",
			"type":        "message",
			"role":        "assistant",
			"model":       "claude-3-5-sonnet",
			"content":     []map[string]any{{"type": "text", "text": "full completion"}},
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 20,
			},
		})
	}))
	defer server.Close()

	adapter := newTestAnthropicAdapter(server.URL, "claude-3-5-sonnet")
	chat := llm.NewChatContext()
	chat.SetRequestInstruction("per-request instruction")
	chat.AddUserMessage("write essay")
	chat.SetMaxOutputTokens(5)

	resp, err := adapter.GenerateText(context.Background(), chat)
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
	}
	if attempt2MaxTokens != 8192 {
		t.Errorf("attempt 2 max_tokens = %d, want 8192", attempt2MaxTokens)
	}
	if resp.Choices[0].Text != "full completion" {
		t.Errorf("text = %q, want 'full completion'", resp.Choices[0].Text)
	}
	if attempt2System != "per-request instruction" {
		t.Errorf("attempt 2 system = %q, want 'per-request instruction'", attempt2System)
	}
	// Carried tokens from attempt 1 + attempt 2: input 10+10=20, output 5+20=25
	if resp.Usage.Input != 20 || resp.Usage.Output != 25 {
		t.Errorf("accumulated usage = %+v, want 20 in, 25 out", resp.Usage)
	}
}

func TestAnthropicGenerateText_TruncatedError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          "msg_trunc",
			"type":        "message",
			"role":        "assistant",
			"model":       "claude-3-5-sonnet",
			"content":     []map[string]any{{"type": "text", "text": "half"}},
			"stop_reason": "max_tokens",
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 8192,
			},
		})
	}))
	defer server.Close()

	adapter := newTestAnthropicAdapter(server.URL, "claude-3-5-sonnet")
	chat := llm.NewChatContext()
	chat.AddUserMessage("write long")

	_, err := adapter.GenerateText(context.Background(), chat)
	if !errors.Is(err, llm.ErrOutputTruncated) {
		t.Fatalf("expected ErrOutputTruncated, got %v", err)
	}
}

func TestAnthropicGenerateTextStream_Text(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected Flusher")
		}

		events := []string{
			`event: message_start` + "\n" +
				`data: {"type":"message_start","message":{"id":"msg_s1","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[],"usage":{"input_tokens":12,"output_tokens":0}}}` + "\n\n",
			`event: content_block_start` + "\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
			`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}` + "\n\n",
			`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"stream!"}}` + "\n\n",
			`event: message_delta` + "\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":6}}` + "\n\n",
			`event: message_stop` + "\n" +
				`data: {"type":"message_stop"}` + "\n\n",
		}

		for _, e := range events {
			_, _ = fmt.Fprint(w, e)
			flusher.Flush()
		}
	}))
	defer server.Close()

	adapter := newTestAnthropicAdapter(server.URL, "claude-3-5-sonnet")
	chat := llm.NewChatContext()
	chat.AddUserMessage("stream me")

	var (
		accumulatedText string
		finishReason    string
		finalUsage      *llm.TokensUsage
		chunkCount      int
	)

	err := adapter.GenerateTextStream(context.Background(), chat, func(chunk llm.StreamChunk) error {
		chunkCount++
		accumulatedText += chunk.Delta
		if chunk.FinishReason != "" {
			finishReason = chunk.FinishReason
		}
		if chunk.Usage != nil {
			finalUsage = chunk.Usage
		}
		return nil
	})

	if err != nil {
		t.Fatalf("GenerateTextStream: %v", err)
	}

	// message_start should not emit a chunk, so only 3 chunks: "Hello ", "stream!", and message_delta
	if chunkCount != 3 {
		t.Errorf("chunk count = %d, want 3", chunkCount)
	}
	if accumulatedText != "Hello stream!" {
		t.Errorf("stream text = %q, want 'Hello stream!'", accumulatedText)
	}
	if finishReason != "stop" {
		t.Errorf("finish reason = %q, want 'stop'", finishReason)
	}
	if finalUsage == nil || finalUsage.Input != 12 || finalUsage.Output != 6 {
		t.Errorf("final usage = %+v, want 12 in, 6 out", finalUsage)
	}
}

func TestAnthropicGenerateTextStream_ToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected Flusher")
		}

		events := []string{
			`event: message_start` + "\n" +
				`data: {"type":"message_start","message":{"id":"msg_s2","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[],"usage":{"input_tokens":15,"output_tokens":0}}}` + "\n\n",
			// Block 0 is text
			`event: content_block_start` + "\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
			`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I will calculate."}}` + "\n\n",
			`event: content_block_stop` + "\n" +
				`data: {"type":"content_block_stop","index":0}` + "\n\n",
			// Block 1 is tool_use (should be mapped to tool index 0)
			`event: content_block_start` + "\n" +
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_99","name":"calc","input":{}}}` + "\n\n",
			`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"expr\":"}}` + "\n\n",
			`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"1+1\"}"}}` + "\n\n",
			`event: content_block_stop` + "\n" +
				`data: {"type":"content_block_stop","index":1}` + "\n\n",
			`event: message_delta` + "\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":11}}` + "\n\n",
			`event: message_stop` + "\n" +
				`data: {"type":"message_stop"}` + "\n\n",
		}

		for _, e := range events {
			_, _ = fmt.Fprint(w, e)
			flusher.Flush()
		}
	}))
	defer server.Close()

	adapter := newTestAnthropicAdapter(server.URL, "claude-3-5-sonnet")
	chat := llm.NewChatContext()
	chat.AddUserMessage("calc 1+1")

	var (
		textChunks   string
		toolChunks   []*llm.ToolCallChunk
		finishReason string
	)

	err := adapter.GenerateTextStream(context.Background(), chat, func(chunk llm.StreamChunk) error {
		textChunks += chunk.Delta
		toolChunks = append(toolChunks, chunk.ToolCallChunks...)
		if chunk.FinishReason != "" {
			finishReason = chunk.FinishReason
		}
		return nil
	})

	if err != nil {
		t.Fatalf("GenerateTextStream: %v", err)
	}

	if textChunks != "I will calculate." {
		t.Errorf("textChunks = %q, want 'I will calculate.'", textChunks)
	}
	if finishReason != "tool_calls" {
		t.Errorf("finish reason = %q, want 'tool_calls'", finishReason)
	}
	// Block start (with id and name) + 2 partial JSON deltas
	if len(toolChunks) != 3 {
		t.Fatalf("expected 3 tool chunks, got %d", len(toolChunks))
	}
	// Verify tool index is 0 despite being block 1
	if toolChunks[0].Index != 0 {
		t.Errorf("tool chunk 0 index = %d, want 0", toolChunks[0].Index)
	}
	if toolChunks[1].Index != 0 || toolChunks[2].Index != 0 {
		t.Errorf("tool arg chunk indices = %d, %d, want 0, 0", toolChunks[1].Index, toolChunks[2].Index)
	}
	if toolChunks[0].ID != "call_99" || toolChunks[0].Name != "calc" {
		t.Errorf("chunk 0 = %+v", toolChunks[0])
	}
	fullArgs := toolChunks[1].ArgumentsDelta + toolChunks[2].ArgumentsDelta
	if fullArgs != `{"expr":"1+1"}` {
		t.Errorf("accumulated args = %q, want '{\"expr\":\"1+1\"}'", fullArgs)
	}
}

func TestAnthropicGenerateTextStream_EmptyArgsToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected Flusher")
		}

		events := []string{
			`event: message_start` + "\n" +
				`data: {"type":"message_start","message":{"id":"msg_s3","type":"message","role":"assistant","model":"claude-3-5-sonnet","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n",
			`event: content_block_start` + "\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_empty","name":"get_time","input":{}}}` + "\n\n",
			`event: content_block_stop` + "\n" +
				`data: {"type":"content_block_stop","index":0}` + "\n\n",
			`event: message_delta` + "\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}` + "\n\n",
			`event: message_stop` + "\n" +
				`data: {"type":"message_stop"}` + "\n\n",
		}

		for _, e := range events {
			_, _ = fmt.Fprint(w, e)
			flusher.Flush()
		}
	}))
	defer server.Close()

	adapter := newTestAnthropicAdapter(server.URL, "claude-3-5-sonnet")
	chat := llm.NewChatContext()
	chat.AddUserMessage("time?")

	var toolChunks []*llm.ToolCallChunk

	err := adapter.GenerateTextStream(context.Background(), chat, func(chunk llm.StreamChunk) error {
		toolChunks = append(toolChunks, chunk.ToolCallChunks...)
		return nil
	})

	if err != nil {
		t.Fatalf("GenerateTextStream: %v", err)
	}

	// Should emit start chunk and stop chunk with ArgumentsDelta "{}"
	if len(toolChunks) != 2 {
		t.Fatalf("expected 2 tool chunks, got %d", len(toolChunks))
	}
	if toolChunks[0].ID != "call_empty" || toolChunks[0].Name != "get_time" {
		t.Errorf("chunk 0 unexpected: %+v", toolChunks[0])
	}
	if toolChunks[1].ArgumentsDelta != "{}" {
		t.Errorf("chunk 1 arguments delta = %q, want '{}'", toolChunks[1].ArgumentsDelta)
	}
}

func TestAnthropicGenerateText_EmptyArgsToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":    "msg_empty_args",
			"type":  "message",
			"role":  "assistant",
			"model": "claude-3-5-sonnet",
			"content": []map[string]any{
				{
					"type":  "tool_use",
					"id":    "",
					"name":  "get_time",
					"input": nil,
				},
			},
			"stop_reason": "tool_use",
			"usage": map[string]any{
				"input_tokens":  5,
				"output_tokens": 5,
			},
		})
	}))
	defer server.Close()

	adapter := newTestAnthropicAdapter(server.URL, "claude-3-5-sonnet")
	chat := llm.NewChatContext()
	chat.AddUserMessage("what time is it?")

	resp, err := adapter.GenerateText(context.Background(), chat)
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if len(resp.Choices[0].ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.Choices[0].ToolCalls))
	}
	tc := resp.Choices[0].ToolCalls[0]
	if tc.Function.Arguments != "{}" {
		t.Errorf("arguments = %q, want '{}'", tc.Function.Arguments)
	}
	if tc.ID == "" {
		t.Error("expected non-empty tool call ID")
	}
}

func TestAnthropicGenerateTextStream_ErrorAfterMessageStartDoesNotEmitChunk(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected Flusher")
		}

		// Emit message_start and then immediately close/send invalid stream event to trigger error
		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_err\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3-5-sonnet\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n")
		flusher.Flush()

		// Send invalid event or close connection so stream errors out before any content block
		_, _ = fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	adapter := newTestAnthropicAdapter(server.URL, "claude-3-5-sonnet")
	chat := llm.NewChatContext()
	chat.AddUserMessage("hello")

	chunkCount := 0
	err := adapter.GenerateTextStream(context.Background(), chat, func(chunk llm.StreamChunk) error {
		chunkCount++
		return nil
	})

	if err == nil {
		t.Fatal("expected stream error")
	}
	if chunkCount != 0 {
		t.Errorf("expected 0 chunks emitted before content error, got %d", chunkCount)
	}
}

func TestAnthropicPromptCacheBreakpoints(t *testing.T) {
	tools := []*llm.Tool{
		{Type: "function", Function: llm.FunctionDefinition{Name: "a", Parameters: &llm.ResponseSchema{Type: llm.SchemaPropertyTypeObject}}},
		{Type: "function", Function: llm.FunctionDefinition{Name: "b", Parameters: &llm.ResponseSchema{Type: llm.SchemaPropertyTypeObject}}},
	}
	type block struct {
		CacheControl json.RawMessage `json:"cache_control"`
	}
	type body struct {
		System   []block `json:"system"`
		Tools    []block `json:"tools"`
		Messages []struct {
			Content []block `json:"content"`
		} `json:"messages"`
	}
	build := func(cache, soft bool, system string, turns ...string) body {
		adapter := NewAnthropicAdapter(nil, "claude-test", nil)
		adapter.SetPromptCache(cache)
		adapter.SetSoftSchema(soft)
		chat := llm.NewChatContext()
		if system != "" {
			chat.AddSystemMessage(system)
		}
		for i, turn := range turns {
			if i%2 == 0 {
				chat.AddUserMessage(turn)
			} else {
				chat.AddAssistantMessage(turn)
			}
		}
		chat.SetTools(tools)
		if soft {
			chat.SetResponseSchema(&llm.ResponseSchema{Type: llm.SchemaPropertyTypeObject,
				Properties: map[string]*llm.SchemaProperty{"answer": {Type: llm.SchemaPropertyTypeString}}})
		}
		raw, err := json.Marshal(adapter.buildParams(chat, 0))
		if err != nil {
			t.Fatal(err)
		}
		var b body
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	// marks lists which blocks carry a breakpoint: "L" on the last block, "x" elsewhere.
	marks := func(bs []block) string {
		out := ""
		for i, b := range bs {
			if string(b.CacheControl) == `{"type":"ephemeral"}` {
				if i == len(bs)-1 {
					out += "L"
				} else {
					out += "x"
				}
			}
		}
		return out
	}
	lastTurn := func(b body) string { return marks(b.Messages[len(b.Messages)-1].Content) }

	b := build(false, false, "instructions", "hi")
	if marks(b.Tools)+marks(b.System)+lastTurn(b) != "" {
		t.Fatalf("breakpoints without the capability")
	}
	b = build(true, false, "instructions", "hi")
	if marks(b.Tools) != "L" || marks(b.System) != "L" || lastTurn(b) != "" {
		t.Fatalf("single turn: tools %q system %q turn %q, want tools and system only", marks(b.Tools), marks(b.System), lastTurn(b))
	}
	b = build(true, true, "instructions", "hi")
	if marks(b.System) != "L" {
		t.Fatalf("soft schema: system %q, want the breakpoint after the appended shape", marks(b.System))
	}
	b = build(true, false, "instructions")
	if len(b.System) != 0 || marks(b.Tools) != "L" {
		t.Fatalf("system sent as the only message: system %v tools %q", b.System, marks(b.Tools))
	}
	b = build(true, false, "instructions", "hi", "hello", "and now?")
	if lastTurn(b) != "L" || marks(b.System) != "L" {
		t.Fatalf("conversation: turn %q system %q, want the latest turn marked too", lastTurn(b), marks(b.System))
	}
}
