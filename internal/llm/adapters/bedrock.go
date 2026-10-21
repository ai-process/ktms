package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/ai-process/ktms/internal/llm"
)

// BedrockRuntimeClient abstracts the AWS Bedrock Runtime client for testing.
type BedrockRuntimeClient interface {
	Converse(ctx context.Context, params *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
	ConverseStream(ctx context.Context, params *bedrockruntime.ConverseStreamInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseStreamOutput, error)
}

type BedrockAdapter struct {
	client     BedrockRuntimeClient
	throttle   *llm.ThrottleControl
	model      string
	softSchema bool
}

func NewBedrockAdapter(client BedrockRuntimeClient, model string, throttle *llm.ThrottleControl) *BedrockAdapter {
	return &BedrockAdapter{
		client:   client,
		model:    model,
		throttle: throttle,
	}
}

// SetSoftSchema switches this adapter to prompt-described schemas.
func (a *BedrockAdapter) SetSoftSchema(soft bool) { a.softSchema = soft }

func (a *BedrockAdapter) GenerateText(ctx context.Context, chat *llm.ChatContext) (*llm.Response, error) {
	ceiling := chat.GetMaxOutputTokens()
	params := a.buildConverseInput(chat, ceiling)

	a.throttle.WaitForSlot()
	if ctx == nil {
		ctx = context.Background()
	}

	resp, err := a.client.Converse(ctx, params)
	if err != nil {
		return nil, err
	}

	return a.toResponse(resp, chat.GetResponseSchema())
}

func (a *BedrockAdapter) GenerateTextStream(ctx context.Context, chat *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
	params := a.buildConverseStreamInput(chat, chat.GetMaxOutputTokens())

	a.throttle.WaitForSlot()
	if ctx == nil {
		ctx = context.Background()
	}

	output, err := a.client.ConverseStream(ctx, params)
	if err != nil {
		return err
	}
	stream := output.GetStream()
	if stream == nil {
		return fmt.Errorf("bedrock stream is nil")
	}
	defer stream.Close()

	var (
		accumulatedInputTokens  int64
		accumulatedOutputTokens int64
		stopReason              types.StopReason
		toolIndexByBlock        = make(map[int32]int32)
		toolArgsReceived        = make(map[int32]bool)
		nextToolIndex           int32
	)

	for event := range stream.Events() {
		var streamChunk llm.StreamChunk

		switch ev := event.(type) {
		case *types.ConverseStreamOutputMemberContentBlockStart:
			var blockIdx int32
			if ev.Value.ContentBlockIndex != nil {
				blockIdx = *ev.Value.ContentBlockIndex
			}
			if toolUse, ok := ev.Value.Start.(*types.ContentBlockStartMemberToolUse); ok {
				toolIdx := nextToolIndex
				toolIndexByBlock[blockIdx] = toolIdx
				toolArgsReceived[blockIdx] = false
				nextToolIndex++
				name := ""
				if toolUse.Value.Name != nil {
					name = *toolUse.Value.Name
				}
				id := ""
				if toolUse.Value.ToolUseId != nil {
					id = *toolUse.Value.ToolUseId
				}
				streamChunk.ToolCallChunks = append(streamChunk.ToolCallChunks, &llm.ToolCallChunk{
					Index: toolIdx,
					ID:    id,
					Type:  "function",
					Name:  name,
				})
			}
		case *types.ConverseStreamOutputMemberContentBlockDelta:
			var blockIdx int32
			if ev.Value.ContentBlockIndex != nil {
				blockIdx = *ev.Value.ContentBlockIndex
			}
			switch delta := ev.Value.Delta.(type) {
			case *types.ContentBlockDeltaMemberText:
				streamChunk.Delta = delta.Value
			case *types.ContentBlockDeltaMemberToolUse:
				toolIdx := toolIndexByBlock[blockIdx]
				toolArgsReceived[blockIdx] = true
				argDelta := ""
				if delta.Value.Input != nil {
					argDelta = *delta.Value.Input
				}
				streamChunk.ToolCallChunks = append(streamChunk.ToolCallChunks, &llm.ToolCallChunk{
					Index:          toolIdx,
					ArgumentsDelta: argDelta,
				})
			}
		case *types.ConverseStreamOutputMemberContentBlockStop:
			var blockIdx int32
			if ev.Value.ContentBlockIndex != nil {
				blockIdx = *ev.Value.ContentBlockIndex
			}
			if toolIdx, ok := toolIndexByBlock[blockIdx]; ok && !toolArgsReceived[blockIdx] {
				toolArgsReceived[blockIdx] = true
				streamChunk.ToolCallChunks = append(streamChunk.ToolCallChunks, &llm.ToolCallChunk{
					Index:          toolIdx,
					ArgumentsDelta: "{}",
				})
			}
		case *types.ConverseStreamOutputMemberMessageStop:
			if ev.Value.StopReason != "" {
				stopReason = ev.Value.StopReason
				streamChunk.FinishReason = mapBedrockStopReason(stopReason)
			}
		case *types.ConverseStreamOutputMemberMetadata:
			if ev.Value.Usage != nil {
				if ev.Value.Usage.InputTokens != nil {
					accumulatedInputTokens = int64(*ev.Value.Usage.InputTokens)
				}
				if ev.Value.Usage.OutputTokens != nil {
					accumulatedOutputTokens = int64(*ev.Value.Usage.OutputTokens)
				}
				if accumulatedInputTokens > 0 || accumulatedOutputTokens > 0 {
					streamChunk.Usage = &llm.TokensUsage{
						Input:  int(accumulatedInputTokens),
						Output: int(accumulatedOutputTokens),
					}
				}
			}
		}

		if streamChunk.Delta != "" || streamChunk.FinishReason != "" || streamChunk.Usage != nil || len(streamChunk.ToolCallChunks) > 0 {
			if err := onChunk(streamChunk); err != nil {
				return err
			}
		}
	}

	if err := stream.Err(); err != nil {
		return err
	}
	return nil
}

func mapBedrockStopReason(reason types.StopReason) string {
	switch reason {
	case types.StopReasonEndTurn, types.StopReasonStopSequence:
		return "stop"
	case types.StopReasonToolUse:
		return "tool_calls"
	case types.StopReasonMaxTokens, types.StopReasonModelContextWindowExceeded:
		return "length"
	case types.StopReasonContentFiltered, types.StopReasonGuardrailIntervened:
		return "content_filter"
	default:
		return string(reason)
	}
}

func bedrockUsage(u *types.TokenUsage) llm.TokensUsage {
	if u == nil {
		return llm.TokensUsage{}
	}
	var in, out int
	if u.InputTokens != nil {
		in = int(*u.InputTokens)
	}
	if u.OutputTokens != nil {
		out = int(*u.OutputTokens)
	}
	return llm.TokensUsage{
		Input:  in,
		Output: out,
	}
}

func (a *BedrockAdapter) toResponse(resp *bedrockruntime.ConverseOutput, schema *llm.ResponseSchema) (*llm.Response, error) {
	if resp == nil {
		return nil, fmt.Errorf("empty bedrock response")
	}

	outMsg, ok := resp.Output.(*types.ConverseOutputMemberMessage)
	if !ok {
		return nil, fmt.Errorf("unexpected bedrock response output type %T", resp.Output)
	}

	var textParts []string
	var toolCalls []*llm.ToolCall

	for _, block := range outMsg.Value.Content {
		switch b := block.(type) {
		case *types.ContentBlockMemberText:
			if b.Value != "" {
				textParts = append(textParts, b.Value)
			}
		case *types.ContentBlockMemberToolUse:
			var args string
			if b.Value.Input != nil {
				if m, ok := b.Value.Input.(interface{ MarshalSmithyDocument() ([]byte, error) }); ok {
					if bytes, err := m.MarshalSmithyDocument(); err == nil && len(bytes) > 0 {
						args = string(bytes)
					}
				}
				if args == "" {
					var raw any
					if err := b.Value.Input.UnmarshalSmithyDocument(&raw); err == nil && raw != nil {
						if bytes, err := json.Marshal(raw); err == nil {
							args = string(bytes)
						}
					}
				}
			}
			if args == "" || args == "null" {
				args = "{}"
			}
			name := ""
			if b.Value.Name != nil {
				name = *b.Value.Name
			}
			id := ""
			if b.Value.ToolUseId != nil {
				id = *b.Value.ToolUseId
			}
			if id == "" {
				id = fmt.Sprintf("call_%s_%d", name, len(toolCalls))
			}
			toolCalls = append(toolCalls, &llm.ToolCall{
				ID:   id,
				Type: "function",
				Function: llm.FunctionCall{
					Name:      name,
					Arguments: args,
				},
			})
		}
	}
	text := strings.Join(textParts, "")

	if resp.StopReason == types.StopReasonMaxTokens || resp.StopReason == types.StopReasonModelContextWindowExceeded {
		return nil, llm.TruncatedError("BedrockAdapter", a.model, len(text))
	}

	if resp.StopReason == types.StopReasonContentFiltered || resp.StopReason == types.StopReasonGuardrailIntervened {
		if text == "" && len(toolCalls) == 0 {
			return nil, fmt.Errorf("bedrock content filter: %s", resp.StopReason)
		}
	}

	if text == "" && len(toolCalls) == 0 {
		return nil, fmt.Errorf("empty response from Bedrock")
	}

	if schema != nil && a.softSchema {
		if err := llm.CheckResponseSchema(text, schema); err != nil {
			return nil, err
		}
	}

	response := &llm.Response{}
	response.AddMessage(&llm.Message{
		Type:         llm.MessageTypeBot,
		Text:         text,
		ToolCalls:    toolCalls,
		FinishReason: mapBedrockStopReason(resp.StopReason),
	})
	response.Usage = bedrockUsage(resp.Usage)
	return response, nil
}

func mapBedrockImageFormat(mime string) types.ImageFormat {
	switch strings.ToLower(mime) {
	case "image/png":
		return types.ImageFormatPng
	case "image/gif":
		return types.ImageFormatGif
	case "image/webp":
		return types.ImageFormatWebp
	default:
		return types.ImageFormatJpeg
	}
}

func (a *BedrockAdapter) contextToMessages(messages []*llm.Message, systemInstruction string) ([]types.Message, bool) {
	var out []types.Message
	var prevBotCalls []assistantToolCall

	for i := 0; i < len(messages); {
		msg := messages[i]
		switch msg.Type {
		case llm.MessageTypeSystem:
			i++
		case llm.MessageTypeUser, llm.MessageTypeTool:
			var blocks []types.ContentBlock
			for i < len(messages) && (messages[i].Type == llm.MessageTypeUser || messages[i].Type == llm.MessageTypeTool) {
				m := messages[i]
				if m.Type == llm.MessageTypeUser {
					if len(m.Parts) > 0 {
						hasTextPart := false
						for _, p := range m.Parts {
							switch p.Type {
							case llm.ContentPartTypeText:
								if strings.TrimSpace(p.Text) != "" {
									hasTextPart = true
									blocks = append(blocks, &types.ContentBlockMemberText{Value: p.Text})
								}
							case llm.ContentPartTypeImage:
								if len(p.Data) > 0 {
									blocks = append(blocks, &types.ContentBlockMemberImage{
										Value: types.ImageBlock{
											Format: mapBedrockImageFormat(p.MimeType),
											Source: &types.ImageSourceMemberBytes{Value: p.Data},
										},
									})
								}
							case llm.ContentPartTypeDocument:
								if len(p.Data) > 0 {
									blocks = append(blocks, &types.ContentBlockMemberDocument{
										Value: types.DocumentBlock{
											Format: types.DocumentFormatPdf,
											Name:   aws.String("document"),
											Source: &types.DocumentSourceMemberBytes{Value: p.Data},
										},
									})
								}
							}
						}
						if !hasTextPart && strings.TrimSpace(m.Text) != "" {
							blocks = append([]types.ContentBlock{&types.ContentBlockMemberText{Value: m.Text}}, blocks...)
						}
					} else if strings.TrimSpace(m.Text) != "" {
						blocks = append(blocks, &types.ContentBlockMemberText{Value: m.Text})
					}
				} else if m.Type == llm.MessageTypeTool {
					text := m.Text
					if text == "" {
						text = "{}"
					}
					matchedID := matchToolCallID(prevBotCalls, m.ToolCallID)
					if matchedID != "" {
						blocks = append(blocks, &types.ContentBlockMemberToolResult{
							Value: types.ToolResultBlock{
								ToolUseId: aws.String(matchedID),
								Content: []types.ToolResultContentBlock{
									&types.ToolResultContentBlockMemberText{Value: text},
								},
							},
						})
					} else {
						blocks = append(blocks, &types.ContentBlockMemberText{Value: text})
					}
				}
				i++
			}
			prevBotCalls = nil
			if len(blocks) > 0 {
				out = append(out, types.Message{
					Role:    types.ConversationRoleUser,
					Content: blocks,
				})
			}
		case llm.MessageTypeBot:
			var blocks []types.ContentBlock
			prevBotCalls = nil
			for i < len(messages) && messages[i].Type == llm.MessageTypeBot {
				m := messages[i]
				if strings.TrimSpace(m.Text) != "" {
					blocks = append(blocks, &types.ContentBlockMemberText{Value: m.Text})
				}
				for idx, tc := range m.ToolCalls {
					var input any
					if tc.Function.Arguments != "" {
						_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
					}
					if input == nil {
						input = map[string]any{}
					}
					id := tc.ID
					if id == "" {
						id = fmt.Sprintf("call_%s_%d", tc.Function.Name, idx)
					}
					prevBotCalls = append(prevBotCalls, assistantToolCall{
						id:   id,
						name: tc.Function.Name,
					})
					blocks = append(blocks, &types.ContentBlockMemberToolUse{
						Value: types.ToolUseBlock{
							ToolUseId: aws.String(id),
							Name:      aws.String(tc.Function.Name),
							Input:     document.NewLazyDocument(input),
						},
					})
				}
				i++
			}
			if len(blocks) > 0 {
				out = append(out, types.Message{
					Role:    types.ConversationRoleAssistant,
					Content: blocks,
				})
			}
		default:
			i++
		}
	}

	systemConsumed := false
	if len(out) == 0 {
		if strings.TrimSpace(systemInstruction) != "" {
			out = append(out, types.Message{
				Role:    types.ConversationRoleUser,
				Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: systemInstruction}},
			})
			systemConsumed = true
		} else {
			out = append(out, types.Message{
				Role:    types.ConversationRoleUser,
				Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: "Please continue."}},
			})
		}
	} else if out[0].Role == types.ConversationRoleAssistant {
		out = append([]types.Message{{
			Role:    types.ConversationRoleUser,
			Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: "Please continue."}},
		}}, out...)
	}
	return out, systemConsumed
}

func (a *BedrockAdapter) buildCommonConfig(chat *llm.ChatContext, ceiling int) (
	messages []types.Message,
	system []types.SystemContentBlock,
	infConfig *types.InferenceConfiguration,
	outConfig *types.OutputConfig,
	toolConfig *types.ToolConfiguration,
) {
	systemText := buildSystemInstructionText(chat)
	if a.softSchema {
		if shape := llm.SchemaPromptInstruction(chat.GetResponseSchema()); shape != "" {
			systemText = strings.TrimSpace(systemText + systemInstructionSeparator + shape)
		}
	}

	messages, systemConsumed := a.contextToMessages(chat.GetMessages(), systemText)

	if systemText != "" && !systemConsumed {
		system = []types.SystemContentBlock{
			&types.SystemContentBlockMemberText{Value: systemText},
		}
	}

	if ceiling > 0 {
		infConfig = &types.InferenceConfiguration{
			MaxTokens: aws.Int32(int32(ceiling)),
		}
	}

	if chat.GetResponseSchema() != nil && !a.softSchema {
		schema := llm.EnforceAllRequired(chat.GetResponseSchema())
		rawSchema := convertLLMResponseSchemaToOpenAISchema(schema)
		if b, err := json.Marshal(rawSchema); err == nil {
			outConfig = &types.OutputConfig{
				TextFormat: &types.OutputFormat{
					Type: types.OutputFormatTypeJsonSchema,
					Structure: &types.OutputFormatStructureMemberJsonSchema{
						Value: types.JsonSchemaDefinition{
							Name:   aws.String("response"),
							Schema: aws.String(string(b)),
						},
					},
				},
			}
		}
	}

	if len(chat.GetTools()) > 0 {
		var tools []types.Tool
		for _, t := range chat.GetTools() {
			var inputSchemaMap map[string]any
			if t.Function.Parameters != nil {
				paramSchema := t.Function.Parameters
				if t.Function.Strict {
					paramSchema = llm.EnforceAllRequired(paramSchema)
				}
				propertiesMap := make(map[string]any)
				for name, prop := range paramSchema.Properties {
					propertiesMap[name] = convertLLMSchemaPropertyToOpenAISchemaProperty(prop)
				}
				inputSchemaMap = map[string]any{
					"type":       "object",
					"properties": propertiesMap,
				}
				if len(paramSchema.Required) > 0 {
					inputSchemaMap["required"] = paramSchema.Required
				}
				if t.Function.Strict {
					inputSchemaMap["additionalProperties"] = false
				}
			} else {
				inputSchemaMap = map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				}
			}
			spec := types.ToolSpecification{
				Name: aws.String(t.Function.Name),
				InputSchema: &types.ToolInputSchemaMemberJson{
					Value: document.NewLazyDocument(inputSchemaMap),
				},
			}
			if t.Function.Description != "" {
				spec.Description = aws.String(t.Function.Description)
			}
			if t.Function.Strict {
				spec.Strict = aws.Bool(true)
			}
			tools = append(tools, &types.ToolMemberToolSpec{Value: spec})
		}

		var choice types.ToolChoice
		if tc := chat.GetToolChoice(); tc != nil {
			switch tc.Mode {
			case "auto":
				choice = &types.ToolChoiceMemberAuto{Value: types.AutoToolChoice{}}
			case "required":
				choice = &types.ToolChoiceMemberAny{Value: types.AnyToolChoice{}}
			case "specific":
				choice = &types.ToolChoiceMemberTool{
					Value: types.SpecificToolChoice{Name: aws.String(tc.SpecificFunctionName)},
				}
			}
		}

		toolConfig = &types.ToolConfiguration{
			Tools:      tools,
			ToolChoice: choice,
		}
	}

	return messages, system, infConfig, outConfig, toolConfig
}

func (a *BedrockAdapter) buildConverseInput(chat *llm.ChatContext, ceiling int) *bedrockruntime.ConverseInput {
	msgs, sys, inf, out, tools := a.buildCommonConfig(chat, ceiling)
	return &bedrockruntime.ConverseInput{
		ModelId:         aws.String(a.model),
		Messages:        msgs,
		System:          sys,
		InferenceConfig: inf,
		OutputConfig:    out,
		ToolConfig:      tools,
	}
}

func (a *BedrockAdapter) buildConverseStreamInput(chat *llm.ChatContext, ceiling int) *bedrockruntime.ConverseStreamInput {
	msgs, sys, inf, out, tools := a.buildCommonConfig(chat, ceiling)
	return &bedrockruntime.ConverseStreamInput{
		ModelId:         aws.String(a.model),
		Messages:        msgs,
		System:          sys,
		InferenceConfig: inf,
		OutputConfig:    out,
		ToolConfig:      tools,
	}
}
