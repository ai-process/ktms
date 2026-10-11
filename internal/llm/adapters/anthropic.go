package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/llm-proxy/internal/llm"
)

// defaultAnthropicMaxTokens is used when the caller does not specify a ceiling,
// as Anthropic requires max_tokens on every message creation.
const defaultAnthropicMaxTokens = 4096

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

func (a *AnthropicAdapter) GenerateText(chat *llm.ChatContext) (*llm.Response, error) {
	ceiling := chat.GetMaxOutputTokens()
	resp, err := a.completeOnce(chat, ceiling)
	if err != nil {
		return nil, err
	}

	var abandoned llm.TokensUsage
	if ceiling > 0 && resp.StopReason == anthropic.StopReasonMaxTokens {
		log.Warn().Str("model", a.model).Int("ceiling", ceiling).
			Msg("AnthropicAdapter: output ceiling exhausted, retrying uncapped")
		abandoned = anthropicUsage(resp)
		resp, err = a.completeOnce(chat, 0)
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
	)

	for stream.Next() {
		event := stream.Current()
		var streamChunk llm.StreamChunk

		switch event.Type {
		case "message_start":
			accumulatedInputTokens = event.Message.Usage.InputTokens +
				event.Message.Usage.CacheCreationInputTokens +
				event.Message.Usage.CacheReadInputTokens
			if accumulatedInputTokens > 0 {
				streamChunk.Usage = &llm.TokensUsage{
					Input: int(accumulatedInputTokens),
				}
			}
		case "content_block_start":
			if event.ContentBlock.Type == "tool_use" {
				streamChunk.ToolCallChunks = append(streamChunk.ToolCallChunks, &llm.ToolCallChunk{
					Index: int32(event.Index),
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
				streamChunk.ToolCallChunks = append(streamChunk.ToolCallChunks, &llm.ToolCallChunk{
					Index:          int32(event.Index),
					ArgumentsDelta: event.Delta.PartialJSON,
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
			toolCalls = append(toolCalls, &llm.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: llm.FunctionCall{
					Name:      block.Name,
					Arguments: string(block.Input),
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
		FinishReason: mapAnthropicStopReason(resp.StopReason),
	})

	spent := anthropicUsage(resp)
	response.Usage.Input = spent.Input + carried.Input
	response.Usage.Output = spent.Output + carried.Output
	return response, nil
}

func (a *AnthropicAdapter) completeOnce(chat *llm.ChatContext, ceiling int) (*anthropic.Message, error) {
	params := a.buildParams(chat, ceiling)
	a.throttle.WaitForSlot()
	return a.client.Messages.New(context.Background(), params)
}

// contextToMessages maps llm.Message items to Anthropic turns, grouping consecutive
// user and tool results into alternating user turns, as required by the Anthropic API.
func (a *AnthropicAdapter) contextToMessages(messages []*llm.Message) []anthropic.MessageParam {
	var out []anthropic.MessageParam
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
					if m.Text != "" {
						blocks = append(blocks, anthropic.NewTextBlock(m.Text))
					}
				} else if m.Type == llm.MessageTypeTool {
					blocks = append(blocks, anthropic.NewToolResultBlock(m.ToolCallID, m.Text, false))
				}
				i++
			}
			if len(blocks) > 0 {
				out = append(out, anthropic.NewUserMessage(blocks...))
			}
		case llm.MessageTypeBot:
			var blocks []anthropic.ContentBlockParamUnion
			for i < len(messages) && messages[i].Type == llm.MessageTypeBot {
				m := messages[i]
				if m.Text != "" {
					blocks = append(blocks, anthropic.NewTextBlock(m.Text))
				}
				for _, tc := range m.ToolCalls {
					var input any
					if tc.Function.Arguments != "" {
						_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
					}
					if input == nil {
						input = map[string]any{}
					}
					blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, input, tc.Function.Name))
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
	if len(out) == 0 {
		out = append(out, anthropic.NewUserMessage(anthropic.NewTextBlock("Hello")))
	} else if out[0].Role == anthropic.MessageParamRoleAssistant {
		out = append([]anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Hello"))}, out...)
	}
	return out
}

func (a *AnthropicAdapter) buildParams(chat *llm.ChatContext, ceiling int) anthropic.MessageNewParams {
	maxTokens := int64(defaultAnthropicMaxTokens)
	if ceiling > 0 {
		maxTokens = int64(ceiling)
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		Messages:  a.contextToMessages(chat.GetMessages()),
		MaxTokens: maxTokens,
	}

	system := buildSystemInstructionText(chat)
	if a.softSchema {
		if shape := llm.SchemaPromptInstruction(chat.GetResponseSchema()); shape != "" {
			system = strings.TrimSpace(system + systemInstructionSeparator + shape)
		}
	}
	if system != "" {
		params.System = []anthropic.TextBlockParam{{Text: system}}
	}

	if chat.GetResponseSchema() != nil && !a.softSchema {
		llm.EnforceAllRequired(chat.GetResponseSchema())
		params.OutputConfig = anthropic.OutputConfigParam{
			Format: anthropic.JSONOutputFormatParam{
				Schema: convertLLMResponseSchemaToOpenAISchema(chat.GetResponseSchema()),
			},
		}
	}

	if len(chat.GetTools()) > 0 {
		var tools []anthropic.ToolUnionParam
		for _, t := range chat.GetTools() {
			var propertiesMap map[string]any
			var required []string
			if t.Function.Parameters != nil {
				required = t.Function.Parameters.Required
				if len(t.Function.Parameters.Properties) > 0 {
					propertiesMap = make(map[string]any)
					for name, prop := range t.Function.Parameters.Properties {
						propertiesMap[name] = convertLLMSchemaPropertyToOpenAISchemaProperty(prop)
					}
				}
			}
			inputSchema := anthropic.ToolInputSchemaParam{
				Properties: propertiesMap,
				Required:   required,
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
