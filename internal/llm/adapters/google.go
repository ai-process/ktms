package adapters

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"
	"google.golang.org/genai"
	"google.golang.org/grpc/codes"

	"github.com/ai-process/llm-proxy/internal/audio"
	"github.com/ai-process/llm-proxy/internal/llm"
)

// ttsOutputSampleRate is what Gemini TTS produces (16-bit LE mono PCM) when the
// response MIME type omits an explicit rate.
const ttsOutputSampleRate = 24000

var inlineBatchSizeThreshold = 15 * 1024 * 1024 // 15 MB

type GoogleAdapter struct {
	geminiClient *genai.Client
	throttle     *llm.ThrottleControl
	model        string
	ttsModel     string
	ttsVoice     string
	imageModel   string
	softSchema   bool
}

func NewGoogleAdapter(geminiClient *genai.Client, model string, throttle *llm.ThrottleControl) *GoogleAdapter {
	return &GoogleAdapter{
		geminiClient: geminiClient,
		model:        model,
		throttle:     throttle,
		// Nano Banana 2 Lite: newest and cheapest of the flash image line.
	}
}

func (a *GoogleAdapter) SetSoftSchema(s bool) {
	a.softSchema = s
}

func (a *GoogleAdapter) GenerateText(chat *llm.ChatContext) (*llm.Response, error) {
	ctx := context.Background()

	// Built once, not per attempt: buildSystemInstruction consumes the chat's
	// per-request instruction, so rebuilding would silently drop it on the retry.
	config := a.buildGenerateConfig(chat)

	contents := a.contextToContents(chat.GetMessages())
	if len(contents) == 0 {
		return nil, fmt.Errorf("GoogleAdapter: no non-empty messages, cannot generate text")
	}

	result, finishReason, err := a.generateOnce(ctx, contents, config)
	if err != nil {
		return nil, err
	}

	// Thinking tokens are charged against MaxOutputTokens, and thinking is on by
	// default for every model here, so a ceiling can be spent entirely on thinking
	// and come back with nothing. Lift it and retry rather than fail a job that
	// would have succeeded uncapped — this log line is the signal to raise the tier.
	if finishReason == genai.FinishReasonMaxTokens && config.MaxOutputTokens > 0 {
		log.Warn().Str("model", a.model).Int32("ceiling", config.MaxOutputTokens).
			Msg("GoogleAdapter: output ceiling exhausted, retrying uncapped")
		config.MaxOutputTokens = 0
		result, finishReason, err = a.generateOnce(ctx, contents, config)
		if err != nil {
			return nil, err
		}
	}

	return a.parseGenerateContentResponse(result, finishReason, chat.GetResponseSchema())
}

func (a *GoogleAdapter) parseGenerateContentResponse(result *genai.GenerateContentResponse, finishReason genai.FinishReason, schema *llm.ResponseSchema) (*llm.Response, error) {
	text := result.Text()

	// Truncated even without a ceiling: the model's own limit was reached, so the
	// payload is half-written and every structured caller here unmarshals JSON.
	if finishReason == genai.FinishReasonMaxTokens {
		return nil, llm.TruncatedError("GoogleAdapter", a.model, len(text))
	}

	var toolCalls []*llm.ToolCall
	if len(result.Candidates) > 0 && result.Candidates[0].Content != nil {
		for _, part := range result.Candidates[0].Content.Parts {
			if part.FunctionCall != nil {
				argsJSON, _ := json.Marshal(part.FunctionCall.Args)
				id := part.FunctionCall.ID
				if id == "" {
					id = "call_" + part.FunctionCall.Name
				}
				toolCalls = append(toolCalls, &llm.ToolCall{
					ID:   id,
					Type: "function",
					Function: llm.FunctionCall{
						Name:      part.FunctionCall.Name,
						Arguments: string(argsJSON),
					},
				})
			}
		}
	}

	if text == "" && len(toolCalls) == 0 {
		return nil, fmt.Errorf("GoogleAdapter: empty response (finish reason: %s)", finishReason)
	}

	//trim "```json" at the beginning and "```" at the end
	if len(text) >= 10 && text[:7] == "```json" {
		text = text[7:]
		if text[len(text)-3:] == "```" {
			text = text[:len(text)-3]
		}
	}

	if schema != nil && a.softSchema {
		if err := llm.CheckResponseSchema(text, schema); err != nil {
			return nil, err
		}
	}

	calculatedFinishReason := "stop"
	if len(toolCalls) > 0 {
		calculatedFinishReason = "tool_calls"
	} else if finishReason == genai.FinishReasonMaxTokens {
		calculatedFinishReason = "length"
	}

	response := &llm.Response{}
	response.AddMessage(&llm.Message{
		Type:         llm.MessageTypeBot,
		Text:         text,
		ToolCalls:    toolCalls,
		FinishReason: calculatedFinishReason,
	})
	if result.UsageMetadata != nil {
		response.Usage.Input = int(result.UsageMetadata.PromptTokenCount)
		if result.UsageMetadata.CandidatesTokenCount > 0 {
			response.Usage.Output = int(result.UsageMetadata.CandidatesTokenCount)
		} else if result.UsageMetadata.TotalTokenCount >= result.UsageMetadata.PromptTokenCount {
			response.Usage.Output = int(result.UsageMetadata.TotalTokenCount - result.UsageMetadata.PromptTokenCount)
		}
	}
	return response, nil
}

// contextToContents maps the whole conversation to genai contents, keeping roles:
// user messages stay user turns, assistant messages stay model turns. A trailing
// model turn is valid for Gemini — generation continues from it, so flows that
// end the context with an assistant message (e.g. answer generation, where the
// exercise question is a bot turn) keep their intended semantics.
// System messages are excluded: they go through buildSystemInstruction.
func (a *GoogleAdapter) contextToContents(messages []*llm.Message) []*genai.Content {
	var contents []*genai.Content
	for _, message := range messages {
		switch message.Type {
		case llm.MessageTypeUser:
			if message.Text != "" {
				contents = append(contents, genai.NewContentFromText(message.Text, genai.RoleUser))
			}
		case llm.MessageTypeBot:
			var parts []*genai.Part
			if message.Text != "" {
				parts = append(parts, genai.NewPartFromText(message.Text))
			}
			for _, tc := range message.ToolCalls {
				var args map[string]any
				if tc.Function.Arguments != "" {
					_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
				}
				fcPart := genai.NewPartFromFunctionCall(tc.Function.Name, args)
				if fcPart.FunctionCall != nil {
					fcPart.FunctionCall.ID = tc.ID
				}
				parts = append(parts, fcPart)
			}
			if len(parts) > 0 {
				contents = append(contents, genai.NewContentFromParts(parts, genai.RoleModel))
			}
		case llm.MessageTypeTool:
			var respMap map[string]any
			if err := json.Unmarshal([]byte(message.Text), &respMap); err != nil {
				respMap = map[string]any{"output": message.Text}
			}
			respPart := genai.NewPartFromFunctionResponse(message.ToolCallID, respMap)
			if respPart.FunctionResponse != nil {
				respPart.FunctionResponse.ID = message.ToolCallID
			}
			contents = append(contents, genai.NewContentFromParts([]*genai.Part{respPart}, genai.RoleUser))
		}
	}
	return contents
}

// generateOnce performs a single GenerateContent call, reporting the candidate's
// finish reason alongside the result so callers can react to a truncated response.
func (a *GoogleAdapter) generateOnce(ctx context.Context, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, genai.FinishReason, error) {
	a.throttle.WaitForSlot()
	result, err := a.geminiClient.Models.GenerateContent(ctx, a.model, contents, config)
	if err != nil {
		return nil, genai.FinishReasonUnspecified, err
	}
	finishReason := genai.FinishReasonUnspecified
	if len(result.Candidates) > 0 {
		finishReason = result.Candidates[0].FinishReason
	}
	return result, finishReason, nil
}

// GenerateTextStream implements llm.StreamAdapter for Google Gemini.
func (a *GoogleAdapter) GenerateTextStream(ctx context.Context, chat *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
	config := a.buildGenerateConfig(chat)
	contents := a.contextToContents(chat.GetMessages())
	if len(contents) == 0 {
		return fmt.Errorf("GoogleAdapter: no non-empty messages, cannot generate text")
	}

	a.throttle.WaitForSlot()
	for resp, err := range a.geminiClient.Models.GenerateContentStream(ctx, a.model, contents, config) {
		if err != nil {
			return err
		}

		var streamChunk llm.StreamChunk
		if resp.UsageMetadata != nil {
			streamChunk.Usage = &llm.TokensUsage{
				Input: int(resp.UsageMetadata.PromptTokenCount),
			}
			if resp.UsageMetadata.CandidatesTokenCount > 0 {
				streamChunk.Usage.Output = int(resp.UsageMetadata.CandidatesTokenCount)
			}
		}

		if len(resp.Candidates) > 0 {
			cand := resp.Candidates[0]
			if cand.FinishReason != "" && cand.FinishReason != genai.FinishReasonUnspecified {
				switch cand.FinishReason {
				case genai.FinishReasonStop:
					streamChunk.FinishReason = "stop"
				case genai.FinishReasonMaxTokens:
					streamChunk.FinishReason = "length"
				default:
					streamChunk.FinishReason = string(cand.FinishReason)
				}
			}

			if cand.Content != nil {
				for _, part := range cand.Content.Parts {
					if part.Text != "" {
						streamChunk.Delta += part.Text
					}
					if part.FunctionCall != nil {
						argsJSON, _ := json.Marshal(part.FunctionCall.Args)
						streamChunk.ToolCallChunks = append(streamChunk.ToolCallChunks, &llm.ToolCallChunk{
							ID:             part.FunctionCall.ID,
							Type:           "function",
							Name:           part.FunctionCall.Name,
							ArgumentsDelta: string(argsJSON),
						})
					}
				}
			}
		}

		if len(streamChunk.ToolCallChunks) > 0 && streamChunk.FinishReason == "" {
			streamChunk.FinishReason = "tool_calls"
		}

		if streamChunk.Delta != "" || streamChunk.FinishReason != "" || streamChunk.Usage != nil || len(streamChunk.ToolCallChunks) > 0 {
			if err := onChunk(streamChunk); err != nil {
				return err
			}
		}
	}

	return nil
}

// buildGenerateConfig assembles the per-request vendor config: system instruction,
// output ceiling, and JSON response schema. Split out from GenerateText so the
// wiring is testable without a live client.
func (a *GoogleAdapter) buildGenerateConfig(chat *llm.ChatContext) *genai.GenerateContentConfig {
	config := &genai.GenerateContentConfig{
		SystemInstruction: a.buildSystemInstruction(chat),
	}

	// Clamped: the field is int32 and a caller-supplied value beyond that would
	// wrap negative, which the vendor reads as a nonsense ceiling.
	if max := chat.GetMaxOutputTokens(); max > 0 {
		if max > math.MaxInt32 {
			max = math.MaxInt32
		}
		config.MaxOutputTokens = int32(max)
	}

	if chat.GetResponseSchema() != nil {
		llm.EnforceAllRequired(chat.GetResponseSchema())
		if genaiSchema := convertSchemaToGenai(chat.GetResponseSchema()); genaiSchema != nil {
			config.ResponseSchema = genaiSchema
			// Response schema requires application/json MIME type
			config.ResponseMIMEType = "application/json"
		}
	}

	if len(chat.GetTools()) > 0 {
		var fds []*genai.FunctionDeclaration
		for _, t := range chat.GetTools() {
			fd := &genai.FunctionDeclaration{
				Name:        t.Function.Name,
				Description: t.Function.Description,
			}
			if t.Function.Parameters != nil {
				fd.Parameters = convertSchemaToGenai(t.Function.Parameters)
			}
			fds = append(fds, fd)
		}
		tool := &genai.Tool{FunctionDeclarations: fds}
		if chat.GoogleSearchEnabled() && config.ResponseSchema == nil {
			tool.GoogleSearch = &genai.GoogleSearch{}
		}
		config.Tools = append(config.Tools, tool)

		if tc := chat.GetToolChoice(); tc != nil {
			switch tc.Mode {
			case "auto":
				config.ToolConfig = &genai.ToolConfig{
					FunctionCallingConfig: &genai.FunctionCallingConfig{
						Mode: genai.FunctionCallingConfigModeAuto,
					},
				}
			case "none":
				config.ToolConfig = &genai.ToolConfig{
					FunctionCallingConfig: &genai.FunctionCallingConfig{
						Mode: genai.FunctionCallingConfigModeNone,
					},
				}
			case "required":
				config.ToolConfig = &genai.ToolConfig{
					FunctionCallingConfig: &genai.FunctionCallingConfig{
						Mode: genai.FunctionCallingConfigModeAny,
					},
				}
			case "specific":
				config.ToolConfig = &genai.ToolConfig{
					FunctionCallingConfig: &genai.FunctionCallingConfig{
						Mode:                 genai.FunctionCallingConfigModeAny,
						AllowedFunctionNames: []string{tc.SpecificFunctionName},
					},
				}
			}
		}
	} else if chat.GoogleSearchEnabled() {
		if config.ResponseSchema != nil {
			// Gemini rejects search grounding combined with a JSON response schema;
			// the schema is the caller's contract, so the tool is what gets dropped.
			log.Warn().Msg("google search grounding dropped: incompatible with a response schema")
		} else {
			config.Tools = []*genai.Tool{{GoogleSearch: &genai.GoogleSearch{}}}
		}
	}
	return config
}

// buildSystemInstruction combines base system instruction and per-request instruction
func (a *GoogleAdapter) buildSystemInstruction(chat *llm.ChatContext) *genai.Content {
	text := buildSystemInstructionText(chat)
	if text == "" {
		return genai.NewContentFromText(defaultSystemInstruction, genai.RoleUser)
	}
	return genai.NewContentFromText(text, "")
}

func convertLLMSchemaPropertyTypeToGenaiType(llmType llm.SchemaPropertyType) genai.Type {
	switch llmType {
	case llm.SchemaPropertyTypeString:
		return genai.TypeString
	case llm.SchemaPropertyTypeInteger:
		return genai.TypeInteger
	case llm.SchemaPropertyTypeBoolean:
		return genai.TypeBoolean
	case llm.SchemaPropertyTypeObject:
		return genai.TypeObject
	case llm.SchemaPropertyTypeArray:
		return genai.TypeArray
	default:
		return genai.TypeUnspecified
	}
}

func convertPropertySchemaToGenai(llmProperty *llm.SchemaProperty) *genai.Schema {
	if llmProperty == nil {
		return nil
	}

	genaiSchema := &genai.Schema{
		Type:        convertLLMSchemaPropertyTypeToGenaiType(llmProperty.Type),
		Description: llmProperty.Description,
	}

	// Handle nullable types: JSON Schema uses ["type", "null"] for nullable fields
	// Since genai.Schema might not support this directly, we note it in the description
	// The field being optional (not in Required) should allow null values
	if llmProperty.Nullable && genaiSchema.Description != "" {
		genaiSchema.Description += " (nullable: can be null)"
	} else if llmProperty.Nullable {
		genaiSchema.Description = "nullable: can be null"
	}

	if len(llmProperty.Properties) > 0 {
		genaiSchema.Properties = make(map[string]*genai.Schema)
		for name, prop := range llmProperty.Properties {
			genaiSchema.Properties[name] = convertPropertySchemaToGenai(prop)
		}
	}

	if llmProperty.Items != nil {
		genaiSchema.Items = convertPropertySchemaToGenai(llmProperty.Items)
	}

	// Add required fields if this is an object type with required fields
	if len(llmProperty.Required) > 0 {
		genaiSchema.Required = llmProperty.Required
	}

	return genaiSchema
}

func convertSchemaToGenai(llmResponseSchema *llm.ResponseSchema) *genai.Schema {
	if llmResponseSchema == nil {
		return nil
	}

	genaiSchema := &genai.Schema{
		Type:     convertLLMSchemaPropertyTypeToGenaiType(llmResponseSchema.Type),
		Required: llmResponseSchema.Required,
	}

	if len(llmResponseSchema.Properties) > 0 {
		genaiSchema.Properties = make(map[string]*genai.Schema)
		for name, prop := range llmResponseSchema.Properties {
			genaiSchema.Properties[name] = convertPropertySchemaToGenai(prop)
		}
	}

	if llmResponseSchema.Items != nil {
		genaiSchema.Items = convertPropertySchemaToGenai(llmResponseSchema.Items)
	}

	return genaiSchema
}

// speakInstruction steers the TTS model to read the phrase verbatim; without it
// it occasionally treats the text as a task and answers it ("tried to generate
// text" 400s). Naming the language is load-bearing: Cyrillic scripts (Serbian,
// Russian, Bulgarian, …) share most letters, so without an explicit language
// Gemini defaults to the dominant one (Russian) and mispronounces the rest.
func speakInstruction(language string) string {
	if l := strings.TrimSpace(language); l != "" {
		return "Read the following aloud in " + l + ", exactly as written, with native " + l + " pronunciation. Do not translate it:\n\n"
	}
	return "Read the following aloud exactly as written, in its native pronunciation:\n\n"
}

// mimeSampleRate parses the rate from a PCM MIME type like
// "audio/L16;codec=pcm;rate=24000"; 0 when absent.
func mimeSampleRate(mime string) int {
	for _, param := range strings.Split(mime, ";") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(param), "rate="); ok {
			if rate, err := strconv.Atoi(v); err == nil && rate > 0 {
				return rate
			}
		}
	}
	return 0
}

// SetImageModel overrides the native image model used by GenerateImage.
// Empty keeps the default.
func (a *GoogleAdapter) SetImageModel(model string) {
	if model != "" {
		a.imageModel = model
	}
}

// GenerateImage renders one image from a text prompt via Gemini's native image
// modality. Like TTS, the image model is its own setting, not a chat tier.
func (a *GoogleAdapter) GenerateImage(prompt, userID string, meta map[string]string) (*llm.ImageResult, error) {
	failed := func(err error) (*llm.ImageResult, error) {
		return &llm.ImageResult{Model: a.imageModel}, err
	}

	cfg := &genai.GenerateContentConfig{
		ResponseModalities: []string{string(genai.ModalityImage)},
		// Landscape: these images are chat photo captions, never posters.
		ImageConfig: &genai.ImageConfig{AspectRatio: "16:9"},
	}
	contents := []*genai.Content{genai.NewContentFromText(prompt, genai.RoleUser)}

	a.throttle.WaitForSlot()
	resp, err := a.geminiClient.Models.GenerateContent(context.Background(), a.imageModel, contents, cfg)
	if err != nil {
		return failed(fmt.Errorf("gemini image: %w", err))
	}

	result := &llm.ImageResult{Model: a.imageModel}
	for _, cand := range resp.Candidates {
		if cand.Content == nil {
			continue
		}
		for _, part := range cand.Content.Parts {
			if part.InlineData == nil || len(part.InlineData.Data) == 0 {
				continue
			}
			result.Data = part.InlineData.Data
			result.MimeType = part.InlineData.MIMEType
			break
		}
		if len(result.Data) > 0 {
			break
		}
	}
	if len(result.Data) == 0 {
		return failed(fmt.Errorf("no image in Gemini response"))
	}
	if resp.UsageMetadata != nil {
		result.Usage.Input = int(resp.UsageMetadata.PromptTokenCount)
		result.Usage.Output = int(resp.UsageMetadata.CandidatesTokenCount)
	}
	return result, nil
}

// SetTTS overrides the text-to-speech model and voice used by SynthesizeSpeech.
// Empty values keep the defaults.
func (a *GoogleAdapter) SetTTS(model, voice string) {
	if model != "" {
		a.ttsModel = model
	}
	if voice != "" {
		a.ttsVoice = voice
	}
}

// SynthesizeSpeech generates OGG/Opus audio for a short phrase. Gemini returns
// raw PCM, which is transcoded to OGG/Opus via ffmpeg (must be on PATH).
func (a *GoogleAdapter) SynthesizeSpeech(text, language, userID string, meta map[string]string) (*llm.SpeechResult, error) {
	// failed carries the actual TTS model alongside an error so usage tracking
	// records the real model (not the wrapping candidate's chat-model label).
	failed := func(err error) (*llm.SpeechResult, error) {
		return &llm.SpeechResult{Model: a.ttsModel}, err
	}
	if !audio.Available() {
		return failed(audio.ErrFFmpegMissing)
	}

	cfg := &genai.GenerateContentConfig{
		ResponseModalities: []string{string(genai.ModalityAudio)},
		SpeechConfig:       &genai.SpeechConfig{},
	}
	if a.ttsVoice != "" {
		cfg.SpeechConfig.VoiceConfig = &genai.VoiceConfig{
			PrebuiltVoiceConfig: &genai.PrebuiltVoiceConfig{VoiceName: a.ttsVoice},
		}
	}
	contents := []*genai.Content{genai.NewContentFromText(speakInstruction(language)+text, genai.RoleUser)}

	ctx := context.Background()
	var lastErr error
	// One local retry for a transient API error. A reply carrying no audio is
	// NOT retried here — it is retryable at the resilience layer, which also
	// reaches the other vendor; retrying both places multiplies the round trips.
	for attempt := 0; attempt < 2; attempt++ {
		a.throttle.WaitForSlot()
		resp, err := a.geminiClient.Models.GenerateContent(ctx, a.ttsModel, contents, cfg)
		if err != nil {
			lastErr = err
			continue
		}
		var pcm []byte
		rate := ttsOutputSampleRate
		for _, cand := range resp.Candidates {
			if cand.Content == nil {
				continue
			}
			for _, part := range cand.Content.Parts {
				if part.InlineData == nil || len(part.InlineData.Data) == 0 {
					continue
				}
				pcm = append(pcm, part.InlineData.Data...)
				if r := mimeSampleRate(part.InlineData.MIMEType); r > 0 {
					rate = r
				}
			}
		}
		if len(pcm) == 0 {
			return failed(fmt.Errorf("gemini returned text, not speech: %w", llm.ErrNoAudioContent))
		}

		ogg, err := audio.EncodePCMToOggOpus(ctx, pcm, rate)
		if err != nil {
			return failed(err)
		}

		durationMs := audio.PCMDurationMs(pcm, rate)
		if durationMs < audio.MinClipMillis {
			// Encoder pads short clips with trailing silence to this length.
			durationMs = audio.MinClipMillis
		}
		result := &llm.SpeechResult{
			Audio:      ogg,
			MimeType:   "audio/ogg",
			DurationMs: durationMs,
			Model:      a.ttsModel,
		}
		if resp.UsageMetadata != nil {
			result.Usage.Input = int(resp.UsageMetadata.PromptTokenCount)
			result.Usage.Output = int(resp.UsageMetadata.CandidatesTokenCount)
		}
		return result, nil
	}
	return failed(fmt.Errorf("gemini tts: %w", lastErr))
}

// SubmitBatch submits a batch of text generation requests to the Gemini Batch API.
// Batches up to ~15 MB are sent as InlinedRequests; larger ones are uploaded as a JSONL file.
func (a *GoogleAdapter) SubmitBatch(ctx context.Context, batchID string, items []*llm.BatchSubmitItem) (string, error) {
	if len(items) == 0 {
		return "", fmt.Errorf("GoogleAdapter: batch is empty")
	}

	inlineRequests := make([]*genai.InlinedRequest, len(items))
	for i, it := range items {
		contents := a.contextToContents(it.Chat.GetMessages())
		config := a.buildGenerateConfig(it.Chat)
		inlineRequests[i] = &genai.InlinedRequest{
			Contents: contents,
			Config:   config,
			Metadata: map[string]string{"key": it.CustomID},
		}
	}

	data, err := json.Marshal(inlineRequests)
	if err != nil {
		return "", fmt.Errorf("GoogleAdapter: marshal inlined requests: %w", err)
	}

	var src *genai.BatchJobSource
	if len(data) <= inlineBatchSizeThreshold {
		src = &genai.BatchJobSource{
			InlinedRequests: inlineRequests,
		}
	} else {
		var buf bytes.Buffer
		for i, it := range items {
			lineReq := map[string]any{
				"contents": inlineRequests[i].Contents,
			}
			if inlineRequests[i].Config != nil {
				cfgMap := map[string]any{}
				if inlineRequests[i].Config.SystemInstruction != nil {
					lineReq["system_instruction"] = inlineRequests[i].Config.SystemInstruction
				}
				if inlineRequests[i].Config.MaxOutputTokens > 0 {
					cfgMap["max_output_tokens"] = inlineRequests[i].Config.MaxOutputTokens
				}
				if inlineRequests[i].Config.ResponseMIMEType != "" {
					cfgMap["response_mime_type"] = inlineRequests[i].Config.ResponseMIMEType
				}
				if inlineRequests[i].Config.ResponseSchema != nil {
					cfgMap["response_schema"] = inlineRequests[i].Config.ResponseSchema
				}
				if len(cfgMap) > 0 {
					lineReq["generation_config"] = cfgMap
				}
			}
			lineObj := map[string]any{
				"key":     it.CustomID,
				"request": lineReq,
			}
			lineBytes, err := json.Marshal(lineObj)
			if err != nil {
				return "", fmt.Errorf("GoogleAdapter: marshal jsonl line: %w", err)
			}
			buf.Write(lineBytes)
			buf.WriteByte('\n')
		}

		uploaded, err := a.geminiClient.Files.Upload(ctx, bytes.NewReader(buf.Bytes()), &genai.UploadFileConfig{
			MIMEType:    "jsonl",
			DisplayName: "batch-" + batchID,
		})
		if err != nil {
			return "", fmt.Errorf("GoogleAdapter: upload batch jsonl file: %w", err)
		}
		src = &genai.BatchJobSource{
			FileName: uploaded.Name,
		}
	}

	job, err := a.geminiClient.Batches.Create(ctx, a.model, src, &genai.CreateBatchJobConfig{
		DisplayName: "batch-" + batchID,
	})
	if err != nil {
		return "", fmt.Errorf("GoogleAdapter: create batch job: %w", err)
	}
	return job.Name, nil
}

// GetBatch polls the vendor for the batch job state.
func (a *GoogleAdapter) GetBatch(ctx context.Context, vendorJobID string) (*llm.BatchJobStatus, error) {
	job, err := a.geminiClient.Batches.Get(ctx, vendorJobID, nil)
	if err != nil {
		return nil, fmt.Errorf("GoogleAdapter: get batch job %s: %w", vendorJobID, err)
	}

	status := &llm.BatchJobStatus{
		State: mapJobState(job.State),
	}
	if job.CompletionStats != nil {
		status.DoneCount = int32(job.CompletionStats.SuccessfulCount)
		status.FailedCount = int32(job.CompletionStats.FailedCount)
		status.TotalCount = status.DoneCount + status.FailedCount + int32(job.CompletionStats.IncompleteCount)
	}
	if !job.EndTime.IsZero() {
		status.CompletedAt = &job.EndTime
	}
	if job.Error != nil {
		status.Error = job.Error.Message
	}
	return status, nil
}

func mapJobState(state genai.JobState) string {
	switch state {
	case genai.JobStatePending, genai.JobStateQueued:
		return "PENDING"
	case genai.JobStateRunning, genai.JobStateUpdating, genai.JobStateCancelling:
		return "RUNNING"
	case genai.JobStateSucceeded:
		return "SUCCEEDED"
	case genai.JobStateFailed:
		return "FAILED"
	case genai.JobStateCancelled:
		return "CANCELLED"
	case genai.JobStateExpired:
		return "EXPIRED"
	default:
		return "PENDING"
	}
}

// CancelBatch requests cancellation of the batch job at the vendor.
func (a *GoogleAdapter) CancelBatch(ctx context.Context, vendorJobID string) error {
	err := a.geminiClient.Batches.Cancel(ctx, vendorJobID, nil)
	if err != nil {
		var apiErr genai.APIError
		var apiErrPtr *genai.APIError
		if (errors.As(err, &apiErr) && apiErr.Status == "FAILED_PRECONDITION") ||
			(errors.As(err, &apiErrPtr) && apiErrPtr != nil && apiErrPtr.Status == "FAILED_PRECONDITION") {
			return nil
		}
		errLower := strings.ToLower(err.Error())
		if strings.Contains(errLower, "already") ||
			strings.Contains(errLower, "cancelling") ||
			strings.Contains(errLower, "canceling") ||
			strings.Contains(errLower, "failed_precondition") {
			return nil
		}
		return fmt.Errorf("GoogleAdapter: cancel batch job %s: %w", vendorJobID, err)
	}
	return nil
}

// FetchBatchResults retrieves results from either inlined responses or output file download.
func (a *GoogleAdapter) FetchBatchResults(ctx context.Context, vendorJobID string, items []*llm.BatchSubmitItem) ([]*llm.BatchItemResult, error) {
	job, err := a.geminiClient.Batches.Get(ctx, vendorJobID, nil)
	if err != nil {
		return nil, fmt.Errorf("GoogleAdapter: fetch batch job %s: %w", vendorJobID, err)
	}

	results := make([]*llm.BatchItemResult, len(items))
	for i, it := range items {
		results[i] = &llm.BatchItemResult{
			CustomID: it.CustomID,
		}
	}

	if job.State == genai.JobStateExpired {
		for i := range results {
			results[i].Error = &llm.BatchItemError{
				Code:    int32(codes.DeadlineExceeded),
				Reason:  "batch_expired",
				Message: "batch expired before completion",
			}
		}
		return results, nil
	}

	if job.State == genai.JobStateFailed && (job.Dest == nil || (len(job.Dest.InlinedResponses) == 0 && job.Dest.FileName == "")) {
		errMsg := "batch job failed"
		if job.Error != nil && job.Error.Message != "" {
			errMsg = job.Error.Message
		}
		for i := range results {
			results[i].Error = &llm.BatchItemError{
				Code:    int32(codes.Internal),
				Reason:  "vendor_error",
				Message: errMsg,
			}
		}
		return results, nil
	}

	// 1. Inlined responses
	if job.Dest != nil && len(job.Dest.InlinedResponses) > 0 {
		for i, inlineResp := range job.Dest.InlinedResponses {
			if i >= len(results) {
				break
			}
			it := items[i]
			if inlineResp.Error != nil {
				results[i].Error = &llm.BatchItemError{
					Code:    int32(codes.Internal),
					Reason:  "vendor_error",
					Message: inlineResp.Error.Message,
				}
				continue
			}
			if inlineResp.Response != nil {
				finishReason := genai.FinishReasonUnspecified
				if len(inlineResp.Response.Candidates) > 0 {
					finishReason = inlineResp.Response.Candidates[0].FinishReason
				}
				resp, parseErr := a.parseGenerateContentResponse(inlineResp.Response, finishReason, it.Chat.GetResponseSchema())
				if parseErr != nil {
					results[i].Error = mapItemError(parseErr)
				} else {
					results[i].Response = resp
				}
			}
		}
		return results, nil
	}

	// 2. Output file responses
	if job.Dest != nil && job.Dest.FileName != "" {
		f := &genai.File{DownloadURI: job.Dest.FileName, URI: job.Dest.FileName}
		data, err := a.geminiClient.Files.Download(ctx, genai.NewDownloadURIFromFile(f), nil)
		if err != nil {
			return nil, fmt.Errorf("GoogleAdapter: download batch results file %s: %w", job.Dest.FileName, err)
		}

		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

		itemIndexByCustomID := make(map[string]int, len(items))
		for i, it := range items {
			itemIndexByCustomID[it.CustomID] = i
		}

		lineIdx := 0
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}

			var parsedLine struct {
				Key      string                          `json:"key"`
				CustomID string                          `json:"custom_id"`
				Response *genai.GenerateContentResponse `json:"response"`
				Error    *genai.JobError                 `json:"error"`
			}
			if err := json.Unmarshal(line, &parsedLine); err == nil && (parsedLine.Response != nil || parsedLine.Error != nil) {
				idx := lineIdx
				k := parsedLine.Key
				if k == "" {
					k = parsedLine.CustomID
				}
				if k != "" {
					if targetIdx, ok := itemIndexByCustomID[k]; ok {
						idx = targetIdx
					}
				}
				if idx < len(results) {
					if parsedLine.Error != nil {
						results[idx].Error = &llm.BatchItemError{
							Code:    int32(codes.Internal),
							Reason:  "vendor_error",
							Message: parsedLine.Error.Message,
						}
					} else if parsedLine.Response != nil {
						finishReason := genai.FinishReasonUnspecified
						if len(parsedLine.Response.Candidates) > 0 {
							finishReason = parsedLine.Response.Candidates[0].FinishReason
						}
						resp, parseErr := a.parseGenerateContentResponse(parsedLine.Response, finishReason, items[idx].Chat.GetResponseSchema())
						if parseErr != nil {
							results[idx].Error = mapItemError(parseErr)
						} else {
							results[idx].Response = resp
						}
					}
				}
			} else {
				var directResp genai.GenerateContentResponse
				if err := json.Unmarshal(line, &directResp); err == nil && len(directResp.Candidates) > 0 {
					if lineIdx < len(results) {
						finishReason := directResp.Candidates[0].FinishReason
						resp, parseErr := a.parseGenerateContentResponse(&directResp, finishReason, items[lineIdx].Chat.GetResponseSchema())
						if parseErr != nil {
							results[lineIdx].Error = mapItemError(parseErr)
						} else {
							results[lineIdx].Response = resp
						}
					}
				}
			}
			lineIdx++
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("GoogleAdapter: scan batch results jsonl: %w", err)
		}
		return results, nil
	}

	return results, nil
}

func mapItemError(err error) *llm.BatchItemError {
	if errors.Is(err, llm.ErrOutputTruncated) {
		return &llm.BatchItemError{
			Code:    int32(codes.FailedPrecondition),
			Reason:  "output_truncated",
			Message: err.Error(),
		}
	}
	return &llm.BatchItemError{
		Code:    int32(codes.Internal),
		Reason:  "vendor_error",
		Message: err.Error(),
	}
}
