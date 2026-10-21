package adapters

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/ktms/internal/llm"
)

// modelMaxOutputTokens returns the output token limit for Claude models.
// Claude 3.5 and 3.7 support 8192 output tokens; Claude 3 supports 4096.
func modelMaxOutputTokens(model string) int {
	m := strings.ToLower(model)
	if strings.Contains(m, "claude-3-5") || strings.Contains(m, "claude-3.5") ||
		strings.Contains(m, "claude-3-7") || strings.Contains(m, "claude-3.7") {
		return 8192
	}
	if strings.Contains(m, "claude-3") {
		return 4096
	}
	return 8192
}

type AnthropicAdapter struct {
	client     *anthropic.Client
	throttle   *llm.ThrottleControl
	model      string
	softSchema bool
}

func NewAnthropicAdapter(client *anthropic.Client, model string, throttle *llm.ThrottleControl) *AnthropicAdapter {
	return &AnthropicAdapter{
		client:   client,
		model:    model,
		throttle: throttle,
	}
}

// SetSoftSchema switches this adapter to prompt-described schemas.
func (a *AnthropicAdapter) SetSoftSchema(soft bool) { a.softSchema = soft }

func (a *AnthropicAdapter) GenerateText(ctx context.Context, chat *llm.ChatContext) (*llm.Response, error) {
	ceiling := chat.GetMaxOutputTokens()
	// Built once, not per attempt: buildParams consumes the chat's per-request
	// instruction, so rebuilding would silently drop it on retry.
	params := a.buildParams(chat, ceiling)

	resp, err := a.completeOnce(ctx, params)
	if err != nil {
		return nil, err
	}

	modelMax := int64(modelMaxOutputTokens(a.model))
	var abandoned llm.TokensUsage
	if params.MaxTokens < modelMax && resp.StopReason == anthropic.StopReasonMaxTokens {
		log.Warn().Str("model", a.model).Int64("ceiling", params.MaxTokens).
			Msg("AnthropicAdapter: output ceiling exhausted, retrying uncapped")
		abandoned = anthropicUsage(resp)
		params.MaxTokens = modelMax
		resp, err = a.completeOnce(ctx, params)
		if err != nil {
			return nil, err
		}
	}

	return a.toResponse(resp, abandoned, chat.GetResponseSchema())
}

// GenerateTextStream implements llm.StreamAdapter for Anthropic.
func (a *AnthropicAdapter) GenerateTextStream(ctx context.Context, chat *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
	params := a.buildParams(chat, chat.GetMaxOutputTokens())
	a.throttle.WaitForSlot()
	stream := a.client.Messages.NewStreaming(ctx, params)
	defer stream.Close()

	var (
		accumulatedInputTokens int64
		stopReason             anthropic.StopReason
		toolIndexByBlock       = make(map[int64]int32)
		toolArgsReceived       = make(map[int64]bool)
		nextToolIndex          int32
	)

	for stream.Next() {
		event := stream.Current()
		var streamChunk llm.StreamChunk

		switch event.Type {
		case "message_start":
			// Token counts are buffered until message_delta so no chunk is emitted
			// before actual content, preserving router fallback on early errors.
			accumulatedInputTokens = event.Message.Usage.InputTokens +
				event.Message.Usage.CacheCreationInputTokens +
				event.Message.Usage.CacheReadInputTokens
		case "content_block_start":
			if event.ContentBlock.Type == "tool_use" {
				toolIdx := nextToolIndex
				toolIndexByBlock[event.Index] = toolIdx
				toolArgsReceived[event.Index] = false
				nextToolIndex++
				streamChunk.ToolCallChunks = append(streamChunk.ToolCallChunks, &llm.ToolCallChunk{
					Index: toolIdx,
					ID:    event.ContentBlock.ID,
					Type:  "function",
					Name:  event.ContentBlock.Name,
				})
			}
		case "content_block_delta":
			switch event.Delta.Type {
			case "text_delta":
				streamChunk.Delta = event.Delta.Text
			case "input_json_delta":
				toolIdx := toolIndexByBlock[event.Index]
				toolArgsReceived[event.Index] = true
				streamChunk.ToolCallChunks = append(streamChunk.ToolCallChunks, &llm.ToolCallChunk{
					Index:          toolIdx,
					ArgumentsDelta: event.Delta.PartialJSON,
				})
			}
		case "content_block_stop":
			// Anthropic omits deltas for empty input; downstream expects valid JSON.
			if toolIdx, ok := toolIndexByBlock[event.Index]; ok && !toolArgsReceived[event.Index] {
				toolArgsReceived[event.Index] = true
				streamChunk.ToolCallChunks = append(streamChunk.ToolCallChunks, &llm.ToolCallChunk{
					Index:          toolIdx,
					ArgumentsDelta: "{}",
				})
			}
		case "message_delta":
			if event.Delta.StopReason != "" {
				stopReason = event.Delta.StopReason
				streamChunk.FinishReason = mapAnthropicStopReason(stopReason)
			}
			outTokens := event.Usage.OutputTokens
			if event.Usage.InputTokens > 0 {
				accumulatedInputTokens = event.Usage.InputTokens +
					event.Usage.CacheCreationInputTokens +
					event.Usage.CacheReadInputTokens
			}
			if outTokens > 0 || accumulatedInputTokens > 0 {
				streamChunk.Usage = &llm.TokensUsage{
					Input:  int(accumulatedInputTokens),
					Output: int(outTokens),
				}
			}
		}

		if streamChunk.Delta != "" || streamChunk.FinishReason != "" || streamChunk.Usage != nil || len(streamChunk.ToolCallChunks) > 0 {
			if err := onChunk(streamChunk); err != nil {
				return err
			}
		}
	}

	return stream.Err()
}

func anthropicUsage(resp *anthropic.Message) llm.TokensUsage {
	if resp == nil {
		return llm.TokensUsage{}
	}
	return llm.TokensUsage{
		Input:  int(resp.Usage.InputTokens + resp.Usage.CacheCreationInputTokens + resp.Usage.CacheReadInputTokens),
		Output: int(resp.Usage.OutputTokens),
	}
}

func mapAnthropicStopReason(reason anthropic.StopReason) string {
	switch reason {
	case anthropic.StopReasonEndTurn, anthropic.StopReasonStopSequence:
		return "stop"
	case anthropic.StopReasonToolUse:
		return "tool_calls"
	case anthropic.StopReasonMaxTokens:
		return "length"
	case anthropic.StopReasonRefusal:
		return "content_filter"
	default:
		return string(reason)
	}
}

func (a *AnthropicAdapter) toResponse(resp *anthropic.Message, carried llm.TokensUsage, schema *llm.ResponseSchema) (*llm.Response, error) {
	var textParts []string
	var toolCalls []*llm.ToolCall

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				textParts = append(textParts, block.Text)
			}
		case "tool_use":
			args := string(block.Input)
			if len(block.Input) == 0 || args == "null" {
				args = "{}"
			}
			id := block.ID
			if id == "" {
				id = fmt.Sprintf("call_%s_%d", block.Name, len(toolCalls))
			}
			toolCalls = append(toolCalls, &llm.ToolCall{
				ID:   id,
				Type: "function",
				Function: llm.FunctionCall{
					Name:      block.Name,
					Arguments: args,
				},
			})
		}
	}
	text := strings.Join(textParts, "")

	if resp.StopReason == anthropic.StopReasonMaxTokens {
		return nil, llm.TruncatedError("AnthropicAdapter", a.model, len(text))
	}

	if text == "" && len(toolCalls) == 0 {
		return nil, fmt.Errorf("empty response from Anthropic")
	}

	// A tool-call reply has no text to check; its arguments are checked upstream.
	if schema != nil && a.softSchema && len(toolCalls) == 0 {
		if err := llm.CheckResponseSchema(text, schema); err != nil {
			return nil, err
		}
	}

	response := &llm.Response{}
	response.AddMessage(&llm.Message{
		Type:         llm.MessageTypeBot,
		Text:         text,
		ToolCalls:    toolCalls,
		FinishReason: mapAnthropicStopReason(resp.StopReason),
	})

	spent := anthropicUsage(resp)
	response.Usage.Input = spent.Input + carried.Input
	response.Usage.Output = spent.Output + carried.Output
	return response, nil
}

func (a *AnthropicAdapter) completeOnce(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	a.throttle.WaitForSlot()
	if ctx == nil {
		ctx = context.Background()
	}
	return a.client.Messages.New(ctx, params)
}

type assistantToolCall struct {
	id   string
	name string
	used bool
}

// contextToMessages maps llm.Message items to Anthropic turns.
// Consecutive user and tool messages are grouped into alternating user turns.
func (a *AnthropicAdapter) contextToMessages(messages []*llm.Message, systemInstruction string) ([]anthropic.MessageParam, bool) {
	var out []anthropic.MessageParam
	var prevBotCalls []assistantToolCall

	for i := 0; i < len(messages); {
		msg := messages[i]
		switch msg.Type {
		case llm.MessageTypeSystem:
			i++
		case llm.MessageTypeUser, llm.MessageTypeTool:
			var blocks []anthropic.ContentBlockParamUnion
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
									blocks = append(blocks, anthropic.NewTextBlock(p.Text))
								}
							case llm.ContentPartTypeImage:
								if len(p.Data) > 0 {
									mime := p.MimeType
									if mime == "" {
										mime = "image/jpeg"
									}
									b64 := base64.StdEncoding.EncodeToString(p.Data)
									blocks = append(blocks, anthropic.NewImageBlockBase64(mime, b64))
								}
							case llm.ContentPartTypeDocument:
								if len(p.Data) > 0 {
									b64 := base64.StdEncoding.EncodeToString(p.Data)
									blocks = append(blocks, anthropic.NewDocumentBlock(anthropic.Base64PDFSourceParam{
										Data: b64,
									}))
								}
							}
						}
						if !hasTextPart && strings.TrimSpace(m.Text) != "" {
							blocks = append([]anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(m.Text)}, blocks...)
						}
					} else if strings.TrimSpace(m.Text) != "" {
						blocks = append(blocks, anthropic.NewTextBlock(m.Text))
					}
				} else if m.Type == llm.MessageTypeTool {
					text := m.Text
					if text == "" {
						text = "{}"
					}
					// Match against preceding assistant tool_use blocks to avoid Anthropic 400.
					matchedID := matchToolCallID(prevBotCalls, m.ToolCallID)
					if matchedID != "" {
						blocks = append(blocks, anthropic.NewToolResultBlock(matchedID, text, false))
					} else {
						blocks = append(blocks, anthropic.NewTextBlock(text))
					}
				}
				i++
			}
			prevBotCalls = nil
			if len(blocks) > 0 {
				out = append(out, anthropic.NewUserMessage(blocks...))
			}
		case llm.MessageTypeBot:
			var blocks []anthropic.ContentBlockParamUnion
			prevBotCalls = nil
			for i < len(messages) && messages[i].Type == llm.MessageTypeBot {
				m := messages[i]
				if strings.TrimSpace(m.Text) != "" {
					blocks = append(blocks, anthropic.NewTextBlock(m.Text))
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
					blocks = append(blocks, anthropic.NewToolUseBlock(id, input, tc.Function.Name))
				}
				i++
			}
			if len(blocks) > 0 {
				out = append(out, anthropic.NewAssistantMessage(blocks...))
			}
		default:
			i++
		}
	}

	systemConsumed := false
	if len(out) == 0 {
		if strings.TrimSpace(systemInstruction) != "" {
			out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock(systemInstruction)))
			systemConsumed = true
		} else {
			out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock("Please continue.")))
		}
	} else if out[0].Role == anthropic.MessageParamRoleAssistant {
		out = append([]anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Please continue."))}, out...)
	}
	return out, systemConsumed
}

func matchToolCallID(calls []assistantToolCall, targetID string) string {
	if targetID != "" {
		for i := range calls {
			if !calls[i].used && calls[i].id == targetID {
				calls[i].used = true
				return calls[i].id
			}
		}
		for i := range calls {
			if !calls[i].used && calls[i].name == targetID {
				calls[i].used = true
				return calls[i].id
			}
		}
	}
	for i := range calls {
		if !calls[i].used {
			calls[i].used = true
			return calls[i].id
		}
	}
	return ""
}

func (a *AnthropicAdapter) buildParams(chat *llm.ChatContext, ceiling int) anthropic.MessageNewParams {
	modelMax := modelMaxOutputTokens(a.model)
	maxTokens := int64(modelMax)
	if ceiling > 0 {
		if ceiling > modelMax {
			maxTokens = int64(modelMax)
		} else {
			maxTokens = int64(ceiling)
		}
	}

	system := buildSystemInstructionText(chat)
	if a.softSchema {
		if shape := llm.SchemaPromptInstruction(chat.GetResponseSchema()); shape != "" {
			system = strings.TrimSpace(system + systemInstructionSeparator + shape)
		}
	}

	messages, systemConsumed := a.contextToMessages(chat.GetMessages(), system)

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		Messages:  messages,
		MaxTokens: maxTokens,
	}

	if system != "" && !systemConsumed {
		params.System = []anthropic.TextBlockParam{{Text: system}}
	}

	if chat.GetResponseSchema() != nil && !a.softSchema {
		schema := llm.EnforceAllRequired(chat.GetResponseSchema())
		params.OutputConfig = anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{
				Schema: convertLLMResponseSchemaToOpenAISchema(schema),
			},
		}
	}

	if len(chat.GetTools()) > 0 {
		var tools []anthropic.ToolUnionParam
		for _, t := range chat.GetTools() {
			var propertiesMap map[string]any
			var required []string
			if t.Function.Parameters != nil {
				paramSchema := t.Function.Parameters
				if t.Function.Strict {
					paramSchema = llm.EnforceAllRequired(paramSchema)
				}
				required = paramSchema.Required
				if len(paramSchema.Properties) > 0 {
					propertiesMap = make(map[string]any)
					for name, prop := range paramSchema.Properties {
						propertiesMap[name] = convertLLMSchemaPropertyToOpenAISchemaProperty(prop)
					}
				}
			}
			inputSchema := anthropic.ToolInputSchemaParam{
				Properties: propertiesMap,
				Required:   required,
			}
			if t.Function.Strict {
				inputSchema.ExtraFields = map[string]any{
					"additionalProperties": false,
				}
			}
			toolParam := anthropic.ToolParam{
				Name:        t.Function.Name,
				InputSchema: inputSchema,
			}
			if t.Function.Description != "" {
				toolParam.Description = param.NewOpt(t.Function.Description)
			}
			if t.Function.Strict {
				toolParam.Strict = param.NewOpt(true)
			}
			tools = append(tools, anthropic.ToolUnionParam{OfTool: &toolParam})
		}
		params.Tools = tools
	}

	if tc := chat.GetToolChoice(); tc != nil {
		switch tc.Mode {
		case "auto":
			params.ToolChoice = anthropic.ToolChoiceUnionParam{
				OfAuto: &anthropic.ToolChoiceAutoParam{},
			}
		case "required":
			params.ToolChoice = anthropic.ToolChoiceUnionParam{
				OfAny: &anthropic.ToolChoiceAnyParam{},
			}
		case "none":
			params.ToolChoice = anthropic.ToolChoiceUnionParam{
				OfNone: &anthropic.ToolChoiceNoneParam{},
			}
		case "specific":
			params.ToolChoice = anthropic.ToolChoiceParamOfTool(tc.SpecificFunctionName)
		}
	}

	return params
}
