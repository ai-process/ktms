package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ai-process/llm-proxy/internal/llm"
)

// judgeTimeout caps one judgment call. Judgments are a request-path decision,
// not a generation: a slow one is worse than none. It is a ceiling, not the
// deadline — the caller's own context still wins when it is shorter.
const judgeTimeout = 8 * time.Second

// TypeSafeAdapter talks to TypeSafe's System One API. It is judgment-only: the
// vendor writes no text, so GenerateText is refused rather than faked.
type TypeSafeAdapter struct {
	http     *http.Client
	endpoint string
	apiKey   string
	model    string
}

func NewTypeSafeAdapter(endpoint, apiKey, model string) *TypeSafeAdapter {
	return &TypeSafeAdapter{
		// No client-level Timeout: the per-request context carries the deadline,
		// and a client timeout would override a caller's shorter one.
		http:     &http.Client{},
		endpoint: strings.TrimSuffix(endpoint, "/"),
		apiKey:   apiKey,
		model:    model,
	}
}

// ErrTextGenerationUnsupported marks a judgment-only vendor asked to write text.
// The capability filter keeps these models out of text chains; this is the
// backstop for a registry that says otherwise.
var ErrTextGenerationUnsupported = fmt.Errorf("text generation not supported by this vendor")

func (a *TypeSafeAdapter) GenerateText(context.Context, *llm.ChatContext) (*llm.Response, error) {
	return nil, ErrTextGenerationUnsupported
}

// TypeSafeError is a non-2xx reply. The status drives retryability; the body is
// kept short and never logged for 401, where it may echo the credential.
type TypeSafeError struct {
	Status int
	Body   string
}

func (e *TypeSafeError) Error() string {
	if e.Status == http.StatusUnauthorized {
		return "typesafe: 401 unauthorized"
	}
	return fmt.Sprintf("typesafe: %d %s", e.Status, e.Body)
}

// HTTPStatus makes 429 and 5xx (including the vendor's 529 overloaded) retryable
// through llm.IsRetryableError, leaving 401 and 422 terminal.
func (e *TypeSafeError) HTTPStatus() int { return e.Status }

type typeSafeQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type typeSafeRequest struct {
	State     json.RawMessage             `json:"state"`
	Model     string                      `json:"model"`
	Questions map[string]typeSafeQuestion `json:"questions"`
}

type typeSafeAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Noul          float64            `json:"noul"`
	Score         float64            `json:"score"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
	Legend        map[string]string  `json:"legend"`
}

type typeSafeResponse struct {
	Model   string                    `json:"model"`
	Answers map[string]typeSafeAnswer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// Judge asks every question against one state in a single call. userID and meta
// are accepted for interface symmetry: the vendor takes neither, and the
// tracking adapter is what records them.
func (a *TypeSafeAdapter) Judge(ctx context.Context, req *llm.JudgeRequest, _ string, _ map[string]string) (*llm.JudgeResult, error) {
	failed := func(err error) (*llm.JudgeResult, error) {
		return &llm.JudgeResult{Model: a.model}, err
	}
	if req == nil || len(req.Questions) == 0 {
		return failed(fmt.Errorf("typesafe: no questions"))
	}

	body, err := json.Marshal(typeSafeRequest{
		State:     json.RawMessage(req.StateJSON),
		Model:     a.model,
		Questions: toTypeSafeQuestions(req.Questions),
	})
	if err != nil {
		return failed(fmt.Errorf("typesafe: encode request: %w", err))
	}

	raw, statusCode, err := executeJudgeRequest(ctx, a.http, a.endpoint+"/v1/systemone", a.apiKey, body)
	if err != nil {
		if statusCode > 0 {
			return failed(&TypeSafeError{Status: statusCode, Body: err.Error()})
		}
		return failed(fmt.Errorf("typesafe: %w", err))
	}

	var parsed typeSafeResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return failed(fmt.Errorf("typesafe: decode response: %w", err))
	}

	result := &llm.JudgeResult{
		Answers: make(map[string]*llm.JudgeAnswer, len(parsed.Answers)),
		Model:   a.model,
		Usage:   llm.TokensUsage{Input: parsed.Usage.InputTokens, Output: parsed.Usage.OutputTokens},
	}
	if parsed.Model != "" {
		result.Model = parsed.Model
	}
	for id, ans := range parsed.Answers {
		result.Answers[id] = &llm.JudgeAnswer{
			Type:          ans.Type,
			Choice:        ans.Choice,
			Noul:          ans.Noul,
			Score:         ans.Score,
			Probabilities: ans.Probabilities,
			Confidence:    ans.Confidence,
			Legend:        ans.Legend,
		}
	}
	// A missing answer would read as a zero-valued judgment downstream, which
	// is a decision nobody made.
	for id := range req.Questions {
		if _, ok := result.Answers[id]; !ok {
			return failed(fmt.Errorf("typesafe: no answer for question %q", id))
		}
	}
	return result, nil
}

// toTypeSafeQuestions renders the vendor-neutral questions in the vendor's
// shape: criteria is a map for choice, a list for score, a true/false pair for
// noul, and absent when a noul states no criteria.
func toTypeSafeQuestions(in map[string]*llm.JudgeQuestion) map[string]typeSafeQuestion {
	out := make(map[string]typeSafeQuestion, len(in))
	for id, q := range in {
		converted := typeSafeQuestion{Type: q.Type, Instructions: q.Instructions}
		switch q.Type {
		case llm.JudgeChoice:
			converted.Criteria = q.Options
		case llm.JudgeScore:
			converted.Criteria = q.Levels
		case llm.JudgeNoul:
			if q.TrueCriteria != "" || q.FalseCriteria != "" {
				converted.Criteria = map[string]string{"true": q.TrueCriteria, "false": q.FalseCriteria}
			}
		}
		out[id] = converted
	}
	return out
}
