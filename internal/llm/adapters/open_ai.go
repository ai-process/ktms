package adapters

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"github.com/ai-process/llm-proxy/internal/llm"
	"math"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

// openAIFinishReasonLength is the finish_reason OpenAI reports when a completion
// stopped because it ran out of budget rather than finishing its answer.
const openAIFinishReasonLength = "length"

// openAIReasoningHeadroom multiplies the caller's ceiling on the OpenAI path,
// where max_completion_tokens is shared between reasoning and the visible answer.
const openAIReasoningHeadroom = 2

func convertLLMSchemaPropertyTypeToOpenAIType(llmType llm.SchemaPropertyType) string {
	switch llmType {
	case llm.SchemaPropertyTypeString:
		return "string"
	case llm.SchemaPropertyTypeInteger:
		return "integer"
	case llm.SchemaPropertyTypeBoolean:
		return "boolean"
	case llm.SchemaPropertyTypeObject:
		return "object"
	case llm.SchemaPropertyTypeArray:
		return "array"
	default:
		return ""
	}
}

func convertLLMSchemaPropertyToOpenAISchemaProperty(llmProperty *llm.SchemaProperty) map[string]interface{} {
	if llmProperty == nil {
		return nil
	}

	openAIProperty := make(map[string]interface{})

	// Handle nullable types: JSON Schema uses ["type", "null"] for nullable fields
	if llmProperty.Nullable {
		// For nullable types, use array format: ["integer", "null"]
		openAIProperty["type"] = []string{
			convertLLMSchemaPropertyTypeToOpenAIType(llmProperty.Type),
			"null",
		}
	} else {
		openAIProperty["type"] = convertLLMSchemaPropertyTypeToOpenAIType(llmProperty.Type)
	}

	if llmProperty.Description != "" {
		openAIProperty["description"] = llmProperty.Description
	}

	if len(llmProperty.Properties) > 0 {
		propertiesMap := make(map[string]interface{})
		for name, prop := range llmProperty.Properties {
			propertiesMap[name] = convertLLMSchemaPropertyToOpenAISchemaProperty(prop)
		}
		openAIProperty["properties"] = propertiesMap
		openAIProperty["additionalProperties"] = false
	}

	if llmProperty.Items != nil {
		openAIProperty["items"] = convertLLMSchemaPropertyToOpenAISchemaProperty(llmProperty.Items)
	}

	// Structured Outputs expects "required" for object schemas.
	if llmProperty.Type == llm.SchemaPropertyTypeObject && len(llmProperty.Required) > 0 {
		openAIProperty["required"] = llmProperty.Required
	}

	return openAIProperty
}

func convertLLMResponseSchemaToOpenAISchema(llmResponseSchema *llm.ResponseSchema) map[string]interface{} {
	if llmResponseSchema == nil {
		return nil
	}

	openAISchema := make(map[string]interface{})
	openAISchema["type"] = convertLLMSchemaPropertyTypeToOpenAIType(llmResponseSchema.Type)

	if len(llmResponseSchema.Properties) > 0 {
		propertiesMap := make(map[string]interface{})
		for name, prop := range llmResponseSchema.Properties {
			propertiesMap[name] = convertLLMSchemaPropertyToOpenAISchemaProperty(prop)
		}
		openAISchema["properties"] = propertiesMap
		openAISchema["additionalProperties"] = false
	}

	if len(llmResponseSchema.Required) > 0 {
		openAISchema["required"] = llmResponseSchema.Required
	}

	return openAISchema
}

func schemaNameFromActionID(actionID string) string {
	name := actionID
	if name == "" {
		name = "response"
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '_' || r == '-':
			return r
		default:
			return '_'
		}
	}, name)
	if len(name) > 64 {
		name = name[:64]
	}
	if name == "" {
		return "response"
	}
	return name
}

type OpenAIAdapter struct {
	openAI     *openai.Client
	throttle   *llm.ThrottleControl
	model      string
	ttsModel   string
	ttsVoice   string
	imageModel string
	// softSchema marks a vendor whose API rejects response_format json_schema
	// (DeepSeek). The shape is asked for in the prompt instead, and the router
	// verifies the reply against the schema afterwards.
	softSchema bool
	noThinking bool
}

func NewOpenAIAdapter(client *openai.Client, model string, throttle *llm.ThrottleControl) *OpenAIAdapter {
	return &OpenAIAdapter{
		openAI:   client,
		model:    model,
		throttle: throttle,
	}
}

// SetSoftSchema switches this adapter to prompt-described schemas.
func (a *OpenAIAdapter) SetSoftSchema(soft bool) { a.softSchema = soft }

// SetNoThinking asks the vendor not to reason (DeepSeek's thinking switch).
func (a *OpenAIAdapter) SetNoThinking(off bool) { a.noThinking = off }

func (a *OpenAIAdapter) GenerateText(chat *llm.ChatContext) (*llm.Response, error) {
	// Rendered once, not per attempt: toOpenAi consumes the chat's per-request
	// instruction, so re-rendering would silently drop it on the retry below.
	messages := a.toOpenAi(chat)
	ceiling := chat.GetMaxOutputTokens()

	resp, err := a.completeOnce(chat, messages, ceiling)
	if err != nil {
		return nil, err
	}

	// Reasoning is charged against max_completion_tokens, so the ceiling can be
	// spent before any answer is written. Lift it and retry rather than fail a job
	// that would have succeeded uncapped; mirrors the Google adapter.
	var abandoned llm.TokensUsage
	if ceiling > 0 && truncatedOnLength(resp) {
		log.Warn().Str("model", a.model).Int("ceiling", ceiling).
			Msg("OpenAIAdapter: output ceiling exhausted, retrying uncapped")
		// The discarded attempt burned the whole ceiling and OpenAI billed it, so
		// its tokens have to reach the collector and the token budgets too.
		abandoned = usageOf(resp)
		resp, err = a.completeOnce(chat, messages, 0)
		if err != nil {
			return nil, err
		}
	}

	return a.toResponse(resp, abandoned)
}

// GenerateTextStream implements llm.StreamAdapter for OpenAI.
func (a *OpenAIAdapter) GenerateTextStream(ctx context.Context, chat *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
	messages := a.toOpenAi(chat)
	ceiling := chat.GetMaxOutputTokens()
	params := a.buildCompletionParams(chat, messages, ceiling)
	params.StreamOptions = openai.ChatCompletionStreamOptionsParam{
		IncludeUsage: openai.Bool(true),
	}

	a.throttle.WaitForSlot()
	stream := a.openAI.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()

	for stream.Next() {
		chunk := stream.Current()
		var streamChunk llm.StreamChunk

		if chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
			streamChunk.Usage = &llm.TokensUsage{
				Input:  int(chunk.Usage.PromptTokens),
				Output: int(chunk.Usage.CompletionTokens),
			}
		}

		if len(chunk.Choices) > 0 {
			ch := chunk.Choices[0]
			streamChunk.Delta = ch.Delta.Content
			streamChunk.FinishReason = ch.FinishReason

			for _, tc := range ch.Delta.ToolCalls {
				streamChunk.ToolCallChunks = append(streamChunk.ToolCallChunks, &llm.ToolCallChunk{
					Index:          int32(tc.Index),
					ID:             tc.ID,
					Type:           tc.Type,
					Name:           tc.Function.Name,
					ArgumentsDelta: tc.Function.Arguments,
				})
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

// usageOf reads the token counts OpenAI charged for one completion.
func usageOf(resp *openai.ChatCompletion) llm.TokensUsage {
	if resp == nil {
		return llm.TokensUsage{}
	}
	return llm.TokensUsage{
		Input:  int(resp.Usage.PromptTokens),
		Output: int(resp.Usage.CompletionTokens),
	}
}

// toResponse converts a chat completion into the adapter-neutral response,
// carrying token usage through so cost tracking and the token budgets see the
// real OpenAI spend. carried holds tokens from earlier billed attempts.
func (a *OpenAIAdapter) toResponse(resp *openai.ChatCompletion, carried llm.TokensUsage) (*llm.Response, error) {
	response := &llm.Response{}

	for _, choice := range resp.Choices {
		// Truncated even uncapped: the model's own limit was reached, so the JSON
		// is a fragment rather than a document.
		if choice.FinishReason == openAIFinishReasonLength {
			return nil, llm.TruncatedError("OpenAIAdapter", a.model, len(choice.Message.Content))
		}
		var tcs []*llm.ToolCall
		for _, tc := range choice.Message.ToolCalls {
			tcs = append(tcs, &llm.ToolCall{
				ID:   tc.ID,
				Type: tc.Type,
				Function: llm.FunctionCall{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
		response.AddMessage(&llm.Message{
			Type:         llm.MessageTypeBot,
			Text:         choice.Message.Content,
			ToolCalls:    tcs,
			FinishReason: choice.FinishReason,
		})
	}
	// Uphold the contract every caller relies on: a nil error means at least
	// one choice. OpenAI can return zero choices (e.g. content filtering) with
	// no transport error; without this guard callers that read Choices[0] panic.
	if len(response.Choices) == 0 {
		return nil, fmt.Errorf("empty response from OpenAI (no choices)")
	}
	spent := usageOf(resp)
	response.Usage.Input = spent.Input + carried.Input
	response.Usage.Output = spent.Output + carried.Output
	return response, nil
}

// completeOnce issues a single chat completion. ceiling of 0 sends no
// max_completion_tokens at all, letting the model's own limit apply.
func (a *OpenAIAdapter) completeOnce(chat *llm.ChatContext, messages []openai.ChatCompletionMessageParamUnion, ceiling int) (*openai.ChatCompletion, error) {
	params := a.buildCompletionParams(chat, messages, ceiling)
	a.throttle.WaitForSlot()
	return a.openAI.Chat.Completions.New(context.Background(), params)
}

// buildCompletionParams assembles the per-attempt request. Split out from
// completeOnce so the ceiling and schema wiring is testable without a live client.
func (a *OpenAIAdapter) buildCompletionParams(chat *llm.ChatContext, messages []openai.ChatCompletionMessageParamUnion, ceiling int) openai.ChatCompletionNewParams {
	params := openai.ChatCompletionNewParams{
		Model:    a.model,
		Messages: messages,
	}

	if ceiling > 0 {
		// max_completion_tokens is shared between reasoning and the visible answer,
		// and the gpt-5 family reasons before it writes, so the ceiling gets headroom.
		budget := int64(ceiling) * openAIReasoningHeadroom
		if budget > math.MaxInt32 {
			budget = math.MaxInt32
		}
		params.MaxCompletionTokens = openai.Int(budget)
	}

	if chat.GetResponseSchema() != nil && !a.softSchema {
		llm.EnforceAllRequired(chat.GetResponseSchema())
		params.ResponseFormat = openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   schemaNameFromActionID(chat.GetActionID()),
					Strict: openai.Bool(true),
					Schema: convertLLMResponseSchemaToOpenAISchema(chat.GetResponseSchema()),
				},
			},
		}
	}
	if a.noThinking {
		params.SetExtraFields(map[string]any{"thinking": map[string]string{"type": "disabled"}})
	}

	if len(chat.GetTools()) > 0 {
		var tools []openai.ChatCompletionToolUnionParam
		for _, t := range chat.GetTools() {
			fnParam := shared.FunctionDefinitionParam{
				Name: t.Function.Name,
			}
			if t.Function.Description != "" {
				fnParam.Description = openai.String(t.Function.Description)
			}
			if t.Function.Strict {
				fnParam.Strict = openai.Bool(true)
			}
			if t.Function.Parameters != nil {
				fnParam.Parameters = shared.FunctionParameters(convertLLMResponseSchemaToOpenAISchema(t.Function.Parameters))
			}
			tools = append(tools, openai.ChatCompletionFunctionTool(fnParam))
		}
		params.Tools = tools
	}

	if tc := chat.GetToolChoice(); tc != nil {
		switch tc.Mode {
		case "auto", "none", "required":
			params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: openai.String(tc.Mode),
			}
		case "specific":
			params.ToolChoice = openai.ToolChoiceOptionFunctionToolChoice(openai.ChatCompletionNamedToolChoiceFunctionParam{
				Name: tc.SpecificFunctionName,
			})
		}
	}
	return params
}

// truncatedOnLength reports whether any returned choice stopped on the token limit.
func truncatedOnLength(resp *openai.ChatCompletion) bool {
	if resp == nil {
		return false
	}
	for _, choice := range resp.Choices {
		if choice.FinishReason == openAIFinishReasonLength {
			return true
		}
	}
	return false
}

func (a *OpenAIAdapter) convertMessageToOpenAi(message *llm.Message) openai.ChatCompletionMessageParamUnion {
	switch message.Type {
	case llm.MessageTypeSystem:
		return openai.SystemMessage(message.Text)
	case llm.MessageTypeUser:
		return openai.UserMessage(message.Text)
	case llm.MessageTypeTool:
		return openai.ToolMessage(message.Text, message.ToolCallID)
	case llm.MessageTypeBot:
		if len(message.ToolCalls) > 0 {
			var tcs []openai.ChatCompletionMessageToolCallUnionParam
			for _, tc := range message.ToolCalls {
				tcs = append(tcs, openai.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
						ID: tc.ID,
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      tc.Function.Name,
							Arguments: tc.Function.Arguments,
						},
					},
				})
			}
			param := openai.ChatCompletionAssistantMessageParam{
				ToolCalls: tcs,
			}
			if message.Text != "" {
				param.Content = openai.ChatCompletionAssistantMessageParamContentUnion{
					OfString: openai.String(message.Text),
				}
			}
			return openai.ChatCompletionMessageParamUnion{
				OfAssistant: &param,
			}
		}
		return openai.AssistantMessage(message.Text)
	}
	return openai.ChatCompletionMessageParamUnion{}
}

// toOpenAi renders the chat as OpenAI messages, leading with the system
// instruction. ChatContext v2 keeps system text in BaseSystemInstruction rather
// than in the messages array, so reading only GetMessages() sent every fallback
// request with no instructions at all — no methodology, no language rules, no
// "reply with JSON" — and the reply was then discarded as malformed.
func (a *OpenAIAdapter) toOpenAi(chat *llm.ChatContext) []openai.ChatCompletionMessageParamUnion {
	var openAiMessages []openai.ChatCompletionMessageParamUnion
	system := buildSystemInstructionText(chat)
	// Without response_format the shape has to travel in words, or the model
	// answers in prose and every attempt fails the check.
	if a.softSchema {
		if shape := llm.SchemaPromptInstruction(chat.GetResponseSchema()); shape != "" {
			system = strings.TrimSpace(system + systemInstructionSeparator + shape)
		}
	}
	if system != "" {
		openAiMessages = append(openAiMessages, openai.SystemMessage(system))
	}
	for _, message := range chat.GetMessages() {
		// Legacy contexts keep system text inline; buildSystemInstructionText
		// already emitted it, so don't send it twice.
		if message.Type == llm.MessageTypeSystem {
			continue
		}
		openAiMessages = append(openAiMessages, a.convertMessageToOpenAi(message))
	}
	return openAiMessages
}

// SetImageModel overrides the model used by GenerateImage. Empty keeps the default.
func (a *OpenAIAdapter) SetImageModel(model string) {
	if model != "" {
		a.imageModel = model
	}
}

// GenerateImage renders one landscape image from a text prompt. The GPT image
// models always answer base64, never a URL.
func (a *OpenAIAdapter) GenerateImage(prompt, userID string, meta map[string]string) (*llm.ImageResult, error) {
	failed := func(err error) (*llm.ImageResult, error) {
		return &llm.ImageResult{Model: a.imageModel}, err
	}

	a.throttle.WaitForSlot()
	resp, err := a.openAI.Images.Generate(context.Background(), openai.ImageGenerateParams{
		Prompt:       prompt,
		Model:        a.imageModel,
		N:            openai.Int(1),
		Size:         openai.ImageGenerateParamsSize1536x1024,
		OutputFormat: openai.ImageGenerateParamsOutputFormatJPEG,
		// Quality the eye cannot tell apart on a phone, at a fraction of the
		// bytes — these pictures are only ever sent to a chat client.
		OutputCompression: openai.Int(80),
	})
	if err != nil {
		return failed(fmt.Errorf("openai image: %w", err))
	}
	if len(resp.Data) == 0 || resp.Data[0].B64JSON == "" {
		return failed(errors.New("no image in OpenAI response"))
	}
	data, err := base64.StdEncoding.DecodeString(resp.Data[0].B64JSON)
	if err != nil {
		return failed(fmt.Errorf("decode openai image: %w", err))
	}

	result := &llm.ImageResult{Data: data, MimeType: "image/jpeg", Model: a.imageModel}
	result.Usage.Input = int(resp.Usage.InputTokens)
	result.Usage.Output = int(resp.Usage.OutputTokens)
	return result, nil
}

// SetTTS overrides the text-to-speech model and voice used by SynthesizeSpeech.
// Empty values keep the defaults.
func (a *OpenAIAdapter) SetTTS(model, voice string) {
	if model != "" {
		a.ttsModel = model
	}
	if voice != "" {
		a.ttsVoice = voice
	}
}

// SynthesizeSpeech generates OGG/Opus audio for a short phrase. OpenAI returns
// the Opus container directly, so no ffmpeg transcode is needed.
func (a *OpenAIAdapter) SynthesizeSpeech(text, language, userID string, meta map[string]string) (*llm.SpeechResult, error) {
	// failed carries the actual TTS model alongside an error so usage tracking
	// records the real model (not the wrapping candidate's chat-model label).
	failed := func(err error) (*llm.SpeechResult, error) {
		return &llm.SpeechResult{Model: a.ttsModel}, err
	}

	a.throttle.WaitForSlot()

	params := openai.AudioSpeechNewParams{
		Input:          text,
		Model:          a.ttsModel,
		Voice:          openai.AudioSpeechNewParamsVoice(a.ttsVoice),
		ResponseFormat: openai.AudioSpeechNewParamsResponseFormatOpus,
	}
	// Instructions steer pronunciation but are unsupported on tts-1/tts-1-hd.
	if a.ttsModel == string(openai.SpeechModelGPT4oMiniTTS) && language != "" {
		params.Instructions = openai.String("Read the text exactly as written, in " + language + " with native pronunciation.")
	}

	resp, err := a.openAI.Audio.Speech.New(context.Background(), params)
	if err != nil {
		return failed(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return failed(err)
	}
	if len(body) == 0 {
		return failed(fmt.Errorf("empty audio from OpenAI TTS"))
	}

	return &llm.SpeechResult{
		Audio:    body,
		MimeType: "audio/ogg",
		Model:    a.ttsModel,
	}, nil
}
