package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/ai-process/llm-proxy/internal/llm"
)

type mockBedrockClient struct {
	converseFn       func(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
	converseStreamFn func(ctx context.Context, params *bedrockruntime.ConverseStreamInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error)
}

func (m *mockBedrockClient) Converse(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	if m.converseFn != nil {
		return m.converseFn(ctx, params, optFns...)
	}
	return nil, errors.New("unimplemented")
}

func (m *mockBedrockClient) ConverseStream(ctx context.Context, params *bedrockruntime.ConverseStreamInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error) {
	if m.converseStreamFn != nil {
		return m.converseStreamFn(ctx, params, optFns...)
	}
	return nil, errors.New("unimplemented")
}

type mockStreamReader struct {
	events chan types.ConverseStreamOutput
	err    error
}

func (r *mockStreamReader) Events() <-chan types.ConverseStreamOutput {
	return r.events
}

func (r *mockStreamReader) Close() error {
	return nil
}

func (r *mockStreamReader) Err() error {
	return r.err
}

func newMockConverseStreamOutput(events ...types.ConverseStreamOutput) *bedrockruntime.ConverseStreamOutput {
	ch := make(chan types.ConverseStreamOutput, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	es := bedrockruntime.NewConverseStreamEventStream(func(s *bedrockruntime.ConverseStreamEventStream) {
		s.Reader = &mockStreamReader{events: ch}
	})
	out := &bedrockruntime.ConverseStreamOutput{}
	val := reflect.ValueOf(out).Elem()
	field := val.FieldByName("eventStream")
	reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Set(reflect.ValueOf(es))
	return out
}

func TestBedrockGenerateText_Success(t *testing.T) {
	mockClient := &mockBedrockClient{
		converseFn: func(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
			if *params.ModelId != "anthropic.claude-3-5-sonnet-20240620-v1:0" {
				t.Fatalf("unexpected model: %s", *params.ModelId)
			}
			return &bedrockruntime.ConverseOutput{
				Output: &types.ConverseOutputMemberMessage{
					Value: types.Message{
						Role: types.ConversationRoleAssistant,
						Content: []types.ContentBlock{
							&types.ContentBlockMemberText{Value: "Hello from Bedrock!"},
						},
					},
				},
				StopReason: types.StopReasonEndTurn,
				Usage: &types.TokenUsage{
					InputTokens:  aws.Int32(12),
					OutputTokens: aws.Int32(8),
				},
			}, nil
		},
	}

	adapter := NewBedrockAdapter(mockClient, "anthropic.claude-3-5-sonnet-20240620-v1:0", llm.NewThrottleControl(0, 60))
	chat := llm.NewChatContext()
	chat.AddUserMessage("Hi")

	resp, err := adapter.GenerateText(context.Background(), chat)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resp.Choices[0].Text != "Hello from Bedrock!" {
		t.Errorf("got text %q, want %q", resp.Choices[0].Text, "Hello from Bedrock!")
	}
	if resp.Usage.Input != 12 || resp.Usage.Output != 8 {
		t.Errorf("got usage %+v, want 12 in, 8 out", resp.Usage)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("got finish reason %q, want %q", resp.Choices[0].FinishReason, "stop")
	}
}

func TestBedrockGenerateText_ToolCalls(t *testing.T) {
	mockClient := &mockBedrockClient{
		converseFn: func(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
			if len(params.ToolConfig.Tools) != 1 {
				t.Fatalf("expected 1 tool, got %d", len(params.ToolConfig.Tools))
			}
			return &bedrockruntime.ConverseOutput{
				Output: &types.ConverseOutputMemberMessage{
					Value: types.Message{
						Role: types.ConversationRoleAssistant,
						Content: []types.ContentBlock{
							&types.ContentBlockMemberToolUse{
								Value: types.ToolUseBlock{
									ToolUseId: aws.String("call_abc123"),
									Name:      aws.String("get_weather"),
									Input:     document.NewLazyDocument(map[string]any{"city": "Paris"}),
								},
							},
						},
					},
				},
				StopReason: types.StopReasonToolUse,
				Usage: &types.TokenUsage{
					InputTokens:  aws.Int32(20),
					OutputTokens: aws.Int32(15),
				},
			}, nil
		},
	}

	adapter := NewBedrockAdapter(mockClient, "anthropic.claude-3-5-sonnet-20240620-v1:0", llm.NewThrottleControl(0, 60))
	chat := llm.NewChatContext()
	chat.SetTools([]*llm.Tool{
		{
			Type: "function",
			Function: llm.FunctionDefinition{
				Name:        "get_weather",
				Description: "Get weather for a city",
				Parameters: &llm.ResponseSchema{
					Type: llm.SchemaPropertyTypeObject,
					Properties: map[string]*llm.SchemaProperty{
						"city": {Type: llm.SchemaPropertyTypeString},
					},
					Required: []string{"city"},
				},
			},
		},
	})
	chat.AddUserMessage("What's the weather in Paris?")

	resp, err := adapter.GenerateText(context.Background(), chat)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msg := resp.Choices[0]
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_abc123" {
		t.Errorf("tc ID = %q, want call_abc123", tc.ID)
	}
	if tc.Function.Name != "get_weather" {
		t.Errorf("tc Name = %q, want get_weather", tc.Function.Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("unmarshal tc args: %v", err)
	}
	if args["city"] != "Paris" {
		t.Errorf("tc city = %v, want Paris", args["city"])
	}
	if msg.FinishReason != "tool_calls" {
		t.Errorf("finish reason = %q, want tool_calls", msg.FinishReason)
	}
}

func TestBedrockGenerateText_MultimodalAndToolResult(t *testing.T) {
	var capturedMessages []types.Message
	mockClient := &mockBedrockClient{
		converseFn: func(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
			capturedMessages = params.Messages
			return &bedrockruntime.ConverseOutput{
				Output: &types.ConverseOutputMemberMessage{
					Value: types.Message{
						Role: types.ConversationRoleAssistant,
						Content: []types.ContentBlock{
							&types.ContentBlockMemberText{Value: "Processed image and document."},
						},
					},
				},
				StopReason: types.StopReasonEndTurn,
			}, nil
		},
	}

	adapter := NewBedrockAdapter(mockClient, "amazon.nova-pro-v1:0", llm.NewThrottleControl(0, 60))
	chat := llm.NewChatContext()
	chat.AddUserMessageWithParts("Here is an image and a doc", []*llm.ContentPart{
		{Type: llm.ContentPartTypeText, Text: "Here is an image and a doc"},
		{Type: llm.ContentPartTypeImage, MimeType: "image/png", Data: []byte("fake-png-data")},
		{Type: llm.ContentPartTypeDocument, MimeType: "application/pdf", Data: []byte("fake-pdf-data")},
	})
	chat.AddAssistantMessageWithToolCalls("", []*llm.ToolCall{
		{
			ID:   "call_doc_1",
			Type: "function",
			Function: llm.FunctionCall{
				Name:      "inspect_doc",
				Arguments: `{"doc_id":"1"}`,
			},
		},
	})
	chat.AddToolMessage("call_doc_1", `{"status":"ok"}`)

	_, err := adapter.GenerateText(context.Background(), chat)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(capturedMessages) != 3 {
		t.Fatalf("expected 3 conversation turns (user, assistant, user), got %d", len(capturedMessages))
	}
	// Check user multimodal blocks
	userTurn1 := capturedMessages[0]
	if userTurn1.Role != types.ConversationRoleUser {
		t.Errorf("turn 0 role = %v, want user", userTurn1.Role)
	}
	if len(userTurn1.Content) != 3 {
		t.Fatalf("expected 3 blocks in turn 0, got %d", len(userTurn1.Content))
	}
	// Check assistant tool use
	asstTurn := capturedMessages[1]
	if asstTurn.Role != types.ConversationRoleAssistant {
		t.Errorf("turn 1 role = %v, want assistant", asstTurn.Role)
	}
	// Check tool result in user turn 2
	userTurn2 := capturedMessages[2]
	if userTurn2.Role != types.ConversationRoleUser {
		t.Errorf("turn 2 role = %v, want user", userTurn2.Role)
	}
	resBlock, ok := userTurn2.Content[0].(*types.ContentBlockMemberToolResult)
	if !ok {
		t.Fatalf("turn 2 block 0 is %T, want *types.ContentBlockMemberToolResult", userTurn2.Content[0])
	}
	if *resBlock.Value.ToolUseId != "call_doc_1" {
		t.Errorf("toolUseId = %s, want call_doc_1", *resBlock.Value.ToolUseId)
	}
}

func TestBedrockGenerateText_TruncatedError(t *testing.T) {
	mockClient := &mockBedrockClient{
		converseFn: func(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
			return &bedrockruntime.ConverseOutput{
				Output: &types.ConverseOutputMemberMessage{
					Value: types.Message{
						Role: types.ConversationRoleAssistant,
						Content: []types.ContentBlock{
							&types.ContentBlockMemberText{Value: "Incomplete sentenc"},
						},
					},
				},
				StopReason: types.StopReasonMaxTokens,
			}, nil
		},
	}

	adapter := NewBedrockAdapter(mockClient, "anthropic.claude-3-haiku-20240307-v1:0", llm.NewThrottleControl(0, 60))
	chat := llm.NewChatContext()
	chat.AddUserMessage("Write an essay")

	_, err := adapter.GenerateText(context.Background(), chat)
	if !errors.Is(err, llm.ErrOutputTruncated) {
		t.Fatalf("expected ErrOutputTruncated, got: %v", err)
	}
}

func TestBedrockGenerateText_ContentFilter(t *testing.T) {
	mockClient := &mockBedrockClient{
		converseFn: func(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
			return &bedrockruntime.ConverseOutput{
				Output: &types.ConverseOutputMemberMessage{
					Value: types.Message{
						Role:    types.ConversationRoleAssistant,
						Content: []types.ContentBlock{},
					},
				},
				StopReason: types.StopReasonContentFiltered,
			}, nil
		},
	}

	adapter := NewBedrockAdapter(mockClient, "meta.llama3-70b-instruct-v1:0", llm.NewThrottleControl(0, 60))
	chat := llm.NewChatContext()
	chat.AddUserMessage("Harmful request")

	_, err := adapter.GenerateText(context.Background(), chat)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !llm.IsContentFilterError(err) {
		t.Errorf("expected IsContentFilterError(err) = true, got false for: %v", err)
	}
}

func TestBedrockGenerateText_SoftSchema(t *testing.T) {
	mockClient := &mockBedrockClient{
		converseFn: func(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
			return &bedrockruntime.ConverseOutput{
				Output: &types.ConverseOutputMemberMessage{
					Value: types.Message{
						Role: types.ConversationRoleAssistant,
						Content: []types.ContentBlock{
							&types.ContentBlockMemberText{Value: `{"name":"test"}`},
						},
					},
				},
				StopReason: types.StopReasonEndTurn,
			}, nil
		},
	}

	adapter := NewBedrockAdapter(mockClient, "anthropic.claude-3-5-sonnet-20240620-v1:0", llm.NewThrottleControl(0, 60))
	adapter.SetSoftSchema(true)

	chat := llm.NewChatContext()
	chat.SetResponseSchema(&llm.ResponseSchema{
		Type: llm.SchemaPropertyTypeObject,
		Properties: map[string]*llm.SchemaProperty{
			"name": {Type: llm.SchemaPropertyTypeString},
		},
		Required: []string{"name"},
	})
	chat.AddUserMessage("Generate JSON")

	resp, err := adapter.GenerateText(context.Background(), chat)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Choices[0].Text != `{"name":"test"}` {
		t.Errorf("got %q, want %q", resp.Choices[0].Text, `{"name":"test"}`)
	}
}

func TestBedrockGenerateTextStream(t *testing.T) {
	mockClient := &mockBedrockClient{
		converseStreamFn: func(ctx context.Context, params *bedrockruntime.ConverseStreamInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error) {
			blockIdx0 := int32(0)
			blockIdx1 := int32(1)
			return newMockConverseStreamOutput(
				&types.ConverseStreamOutputMemberContentBlockStart{
					Value: types.ContentBlockStartEvent{
						ContentBlockIndex: &blockIdx0,
						Start:             &types.ContentBlockStartMemberImage{}, // ignored non-tool
					},
				},
				&types.ConverseStreamOutputMemberContentBlockDelta{
					Value: types.ContentBlockDeltaEvent{
						ContentBlockIndex: &blockIdx0,
						Delta:             &types.ContentBlockDeltaMemberText{Value: "Hello "},
					},
				},
				&types.ConverseStreamOutputMemberContentBlockDelta{
					Value: types.ContentBlockDeltaEvent{
						ContentBlockIndex: &blockIdx0,
						Delta:             &types.ContentBlockDeltaMemberText{Value: "stream!"},
					},
				},
				&types.ConverseStreamOutputMemberContentBlockStart{
					Value: types.ContentBlockStartEvent{
						ContentBlockIndex: &blockIdx1,
						Start: &types.ContentBlockStartMemberToolUse{
							Value: types.ToolUseBlockStart{
								ToolUseId: aws.String("tool_call_1"),
								Name:      aws.String("calc"),
							},
						},
					},
				},
				&types.ConverseStreamOutputMemberContentBlockDelta{
					Value: types.ContentBlockDeltaEvent{
						ContentBlockIndex: &blockIdx1,
						Delta: &types.ContentBlockDeltaMemberToolUse{
							Value: types.ToolUseBlockDelta{
								Input: aws.String(`{"expr":"2+2"}`),
							},
						},
					},
				},
				&types.ConverseStreamOutputMemberContentBlockStop{
					Value: types.ContentBlockStopEvent{
						ContentBlockIndex: &blockIdx1,
					},
				},
				&types.ConverseStreamOutputMemberMessageStop{
					Value: types.MessageStopEvent{
						StopReason: types.StopReasonToolUse,
					},
				},
				&types.ConverseStreamOutputMemberMetadata{
					Value: types.ConverseStreamMetadataEvent{
						Usage: &types.TokenUsage{
							InputTokens:  aws.Int32(10),
							OutputTokens: aws.Int32(20),
						},
					},
				},
			), nil
		},
	}

	adapter := NewBedrockAdapter(mockClient, "anthropic.claude-3-5-sonnet-20240620-v1:0", llm.NewThrottleControl(0, 60))
	chat := llm.NewChatContext()
	chat.AddUserMessage("Calculate 2+2")

	var collectedText strings.Builder
	var toolCalls []*llm.ToolCallChunk
	var finishReason string
	var usage *llm.TokensUsage

	err := adapter.GenerateTextStream(context.Background(), chat, func(chunk llm.StreamChunk) error {
		if chunk.Delta != "" {
			collectedText.WriteString(chunk.Delta)
		}
		if len(chunk.ToolCallChunks) > 0 {
			toolCalls = append(toolCalls, chunk.ToolCallChunks...)
		}
		if chunk.FinishReason != "" {
			finishReason = chunk.FinishReason
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		return nil
	})

	if err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}

	if collectedText.String() != "Hello stream!" {
		t.Errorf("collected text = %q, want 'Hello stream!'", collectedText.String())
	}
	if finishReason != "tool_calls" {
		t.Errorf("finishReason = %q, want tool_calls", finishReason)
	}
	if usage == nil || usage.Input != 10 || usage.Output != 20 {
		t.Errorf("usage = %+v, want 10 in, 20 out", usage)
	}
	if len(toolCalls) < 2 {
		t.Fatalf("expected at least 2 tool chunks, got %d", len(toolCalls))
	}
	if toolCalls[0].Name != "calc" || toolCalls[0].ID != "tool_call_1" {
		t.Errorf("tool chunk 0 = %+v", toolCalls[0])
	}
	if toolCalls[1].ArgumentsDelta != `{"expr":"2+2"}` {
		t.Errorf("tool chunk 1 args = %q", toolCalls[1].ArgumentsDelta)
	}
}
