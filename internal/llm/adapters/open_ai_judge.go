package adapters

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/ai-process/llm-proxy/internal/llm"
)

// OpenAIDecisionError is a non-2xx response from OpenAI's Decisions API.
// Implements HTTPStatusError so the router retries 429 and 5xx across fallback models.
type OpenAIDecisionError struct {
	Status int
	Body   string
}

func (e *OpenAIDecisionError) Error() string {
	// 401 response bodies may echo the rejected API key; suppress it.
	if e.Status == http.StatusUnauthorized {
		return "openai decisions: 401 unauthorized"
	}
	return fmt.Sprintf("openai decisions: %d %s", e.Status, e.Body)
}

func (e *OpenAIDecisionError) HTTPStatus() int { return e.Status }

type openAIDecisionChoice struct {
	Value       string `json:"value"`
	Description string `json:"description"`
}

type openAIDecisionLevel struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

type openAIDecisionQuestion struct {
	Name         string                 `json:"name"`
	Type         string                 `json:"type"`
	Instructions string                 `json:"instructions"`
	Choices      []openAIDecisionChoice `json:"choices,omitempty"`
	Levels       []openAIDecisionLevel  `json:"levels,omitempty"`
}

// openAIDecisionInputPart follows the Responses API input content shape;
// input_embedding is the one extension, for vectors no text schema covers.
type openAIDecisionInputPart struct {
	Type       string                    `json:"type"`
	Text       string                    `json:"text,omitempty"`
	ImageURL   string                    `json:"image_url,omitempty"`
	Detail     string                    `json:"detail,omitempty"`
	InputAudio *openAIDecisionInputAudio `json:"input_audio,omitempty"`
	Embedding  []float32                 `json:"embedding,omitempty"`
	Model      string                    `json:"model,omitempty"`
}

type openAIDecisionInputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

type openAIDecisionRequest struct {
	Model     string                   `json:"model"`
	Input     json.RawMessage          `json:"input"`
	Questions []openAIDecisionQuestion `json:"questions"`
}

type openAIDecisionAnswer struct {
	Name          string             `json:"name"`
	Type          string             `json:"type"`
	Probability   *float64           `json:"probability,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
}

type openAIDecisionResponse struct {
	Model   string                 `json:"model"`
	Answers []openAIDecisionAnswer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// Judge calls OpenAI's /v1/decisions endpoint. Answers typed questions about state
// in a single pass, returning probabilities without prose generation.
func (a *OpenAIAdapter) Judge(ctx context.Context, req *llm.JudgeRequest, _ string, _ map[string]string) (*llm.JudgeResult, error) {
	failed := func(err error) (*llm.JudgeResult, error) {
		return &llm.JudgeResult{Model: a.model}, err
	}
	if a.apiKey == "" {
		return failed(llm.ErrJudgmentUnsupported)
	}
	if req == nil || len(req.Questions) == 0 {
		return failed(fmt.Errorf("openai decisions: no questions"))
	}

	questions, err := toOpenAIDecisionQuestions(req.Questions)
	if err != nil {
		return failed(err)
	}

	inputRaw, err := openAIDecisionInput(req)
	if err != nil {
		return failed(err)
	}

	body, err := json.Marshal(openAIDecisionRequest{
		Model:     a.model,
		Input:     inputRaw,
		Questions: questions,
	})
	if err != nil {
		return failed(fmt.Errorf("openai decisions: encode request: %w", err))
	}

	endpoint := a.endpoint
	if endpoint == "" {
		endpoint = "https://api.openai.com"
	}

	raw, statusCode, err := executeJudgeRequest(ctx, a.http, endpoint+"/v1/decisions", a.apiKey, body)
	if err != nil {
		if statusCode > 0 {
			return failed(&OpenAIDecisionError{Status: statusCode, Body: err.Error()})
		}
		return failed(fmt.Errorf("openai decisions: %w", err))
	}

	var parsed openAIDecisionResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return failed(fmt.Errorf("openai decisions: decode response: %w", err))
	}

	result := &llm.JudgeResult{
		Answers: make(map[string]*llm.JudgeAnswer, len(parsed.Answers)),
		Model:   a.model,
		Usage:   llm.TokensUsage{Input: parsed.Usage.InputTokens, Output: parsed.Usage.OutputTokens},
	}
	if parsed.Model != "" {
		result.Model = parsed.Model
	}

	for _, ans := range parsed.Answers {
		ja := &llm.JudgeAnswer{
			Probabilities: ans.Probabilities,
		}
		if ans.Confidence != nil {
			ja.Confidence = *ans.Confidence
		}
		switch ans.Type {
		case "predicate", "noul":
			ja.Type = llm.JudgeNoul
			if ans.Probability != nil {
				ja.Noul = *ans.Probability
			}
		case "choice":
			ja.Type = llm.JudgeChoice
			ja.Choice = ans.Choice
		case "score":
			ja.Type = llm.JudgeScore
			if ans.Score != nil {
				ja.Score = *ans.Score
			}
			if q, ok := req.Questions[ans.Name]; ok {
				ja.Legend = make(map[string]string, len(q.Levels))
				for i, lvl := range q.Levels {
					ja.Legend[strconv.Itoa(i)] = lvl
				}
			}
		default:
			ja.Type = ans.Type
		}
		result.Answers[ans.Name] = ja
	}

	// Verify all requested questions received an answer; missing answers lead to zero-valued decisions.
	for id := range req.Questions {
		if _, ok := result.Answers[id]; !ok {
			return failed(fmt.Errorf("openai decisions: no answer for question %q", id))
		}
	}

	return result, nil
}

// openAIDecisionInput keeps a JSON-only state verbatim, as before. With media
// it becomes a parts array, the state JSON leading as text so the vendor reads
// the structured state before the media it describes.
func openAIDecisionInput(req *llm.JudgeRequest) (json.RawMessage, error) {
	if len(req.StateMedia) == 0 {
		if req.StateJSON == "" {
			return json.RawMessage("{}"), nil
		}
		return json.RawMessage(req.StateJSON), nil
	}
	parts := make([]openAIDecisionInputPart, 0, len(req.StateMedia)+1)
	if req.StateJSON != "" {
		parts = append(parts, openAIDecisionInputPart{Type: "input_text", Text: req.StateJSON})
	}
	for i, p := range req.StateMedia {
		switch p.Type {
		case llm.JudgeMediaText:
			parts = append(parts, openAIDecisionInputPart{Type: "input_text", Text: p.Text})
		case llm.JudgeMediaImage:
			url := p.URL
			if len(p.Data) > 0 {
				url = "data:" + p.MimeType + ";base64," + base64.StdEncoding.EncodeToString(p.Data)
			}
			parts = append(parts, openAIDecisionInputPart{Type: "input_image", ImageURL: url, Detail: p.Detail})
		case llm.JudgeMediaAudio:
			parts = append(parts, openAIDecisionInputPart{
				Type:       "input_audio",
				InputAudio: &openAIDecisionInputAudio{Data: base64.StdEncoding.EncodeToString(p.Data), Format: p.Format},
			})
		case llm.JudgeMediaEmbedding:
			parts = append(parts, openAIDecisionInputPart{Type: "input_embedding", Embedding: p.Embedding, Model: p.EmbeddingModel})
		default:
			return nil, fmt.Errorf("openai decisions: unsupported state media type %q at %d", p.Type, i)
		}
	}
	raw, err := json.Marshal(parts)
	if err != nil {
		return nil, fmt.Errorf("openai decisions: encode input: %w", err)
	}
	return raw, nil
}

// toOpenAIDecisionQuestions transforms vendor-neutral questions to OpenAI's Decisions API schema.
// Keys are sorted for deterministic JSON serialization in test fixtures and logs.
func toOpenAIDecisionQuestions(in map[string]*llm.JudgeQuestion) ([]openAIDecisionQuestion, error) {
	ids := make([]string, 0, len(in))
	for id := range in {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]openAIDecisionQuestion, 0, len(ids))
	for _, id := range ids {
		q := in[id]
		oq := openAIDecisionQuestion{
			Name:         id,
			Instructions: q.Instructions,
		}
		switch q.Type {
		case llm.JudgeNoul:
			oq.Type = "predicate"
			if q.TrueCriteria != "" || q.FalseCriteria != "" {
				var criteria []string
				if q.TrueCriteria != "" {
					criteria = append(criteria, "True criteria: "+q.TrueCriteria)
				}
				if q.FalseCriteria != "" {
					criteria = append(criteria, "False criteria: "+q.FalseCriteria)
				}
				oq.Instructions = oq.Instructions + " (" + strings.Join(criteria, "; ") + ")"
			}
		case llm.JudgeChoice:
			oq.Type = "choice"
			optKeys := make([]string, 0, len(q.Options))
			for k := range q.Options {
				optKeys = append(optKeys, k)
			}
			sort.Strings(optKeys)
			oq.Choices = make([]openAIDecisionChoice, 0, len(optKeys))
			for _, k := range optKeys {
				oq.Choices = append(oq.Choices, openAIDecisionChoice{
					Value:       k,
					Description: q.Options[k],
				})
			}
		case llm.JudgeScore:
			oq.Type = "score"
			oq.Levels = make([]openAIDecisionLevel, 0, len(q.Levels))
			for i, lvl := range q.Levels {
				oq.Levels = append(oq.Levels, openAIDecisionLevel{
					Label:       strconv.Itoa(i),
					Description: lvl,
				})
			}
		default:
			return nil, fmt.Errorf("openai decisions: unsupported question type %q", q.Type)
		}
		out = append(out, oq)
	}
	return out, nil
}
