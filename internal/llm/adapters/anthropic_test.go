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

	"github.com/ai-process/llm-proxy/internal/llm"
)

func newTestAnthropicAdapter(serverURL string, model string) *AnthropicAdapter {
	noThrottle := llm.NewThrottleControl(0, 60)
	client := anthropic.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(serverURL),
	)
	return NewAnthropicAdapter(&client, model, noThrottle)
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
	})
	chat.AddToolMessage("call_1", `{"temp":18}`)
	chat.AddToolMessage("call_2", `{"forecast":"rain"}`)
	chat.AddUserMessage("and what about tomorrow?")

	msgs := adapter.contextToMessages(chat.GetMessages())

	// Expect alternating turns:
	// 1. user: "hello"
	// 2. assistant: "how can I help?" + tool_use block
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
	if len(msgs[1].Content) != 2 {
		t.Fatalf("turn 1 expected 2 content blocks (text + tool_use), got %d", len(msgs[1].Content))
	}
	if msgs[1].Content[0].OfText == nil || msgs[1].Content[0].OfText.Text != "how can I help?" {
		t.Errorf("turn 1 block 0 text = %+v", msgs[1].Content[0])
	}
	if msgs[1].Content[1].OfToolUse == nil || msgs[1].Content[1].OfToolUse.Name != "get_weather" {
		t.Errorf("turn 1 block 1 tool_use = %+v", msgs[1].Content[1])
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

func TestAnthropicContextToMessagesPrependsUserWhenStartingWithAssistant(t *testing.T) {
	adapter := newTestAnthropicAdapter("http://localhost", "claude-3-5-sonnet")

	chat := llm.NewChatContext()
	chat.AddAssistantMessage("initial bot turn")

	msgs := adapter.contextToMessages(chat.GetMessages())
	if len(msgs) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(msgs))
	}
	if msgs[0].Role != anthropic.MessageParamRoleUser {
		t.Errorf("turn 0 role = %v, want user", msgs[0].Role)
	}
	if msgs[1].Role != anthropic.MessageParamRoleAssistant {
		t.Errorf("turn 1 role = %v, want assistant", msgs[1].Role)
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
			"answer": {Type: llm.SchemaPropertyTypeString},
		},
		Required: []string{"answer"},
	}
	chat.SetResponseSchema(schema)

	chat.SetTools([]*llm.Tool{
		{
			Type: "function",
			Function: llm.FunctionDefinition{
				Name:        "search",
				Description: "search tool",
				Strict:      true,
				Parameters:  schema,
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
	if len(params.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(params.Tools))
	}
	if params.Tools[0].OfTool.Name != "search" {
		t.Errorf("tool name = %s, want search", params.Tools[0].OfTool.Name)
	}
	if params.ToolChoice.OfTool == nil || params.ToolChoice.OfTool.Name != "search" {
		t.Errorf("tool choice = %+v, want search", params.ToolChoice)
	}

	// OutputConfig should be populated
	if params.OutputConfig.Format.Schema == nil {
		t.Error("expected OutputConfig.Format.Schema to be set")
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
		if params.MaxTokens != defaultAnthropicMaxTokens {
			t.Errorf("expected default max tokens %d, got %d", defaultAnthropicMaxTokens, params.MaxTokens)
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

	resp, err := adapter.GenerateText(chat)
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

	resp, err := adapter.GenerateText(chat)
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
	var attempt2System string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		var reqBody struct {
			System []struct {
				Text string `json:"text"`
			} `json:"system"`
		}
		_ = json.NewDecoder(r.Body).Decode(&reqBody)
		if len(reqBody.System) > 0 {
			if attempts == 2 {
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

	resp, err := adapter.GenerateText(chat)
	if err != nil {
		t.Fatalf("GenerateText: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts)
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
				"output_tokens": 4096,
			},
		})
	}))
	defer server.Close()

	adapter := newTestAnthropicAdapter(server.URL, "claude-3-5-sonnet")
	chat := llm.NewChatContext()
	chat.AddUserMessage("write long")

	_, err := adapter.GenerateText(chat)
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
	)

	err := adapter.GenerateTextStream(context.Background(), chat, func(chunk llm.StreamChunk) error {
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

	resp, err := adapter.GenerateText(chat)
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
