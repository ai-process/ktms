package adapters

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/ai-process/ktms/internal/llm"
)

// GeminiDecisionsEndpoint is the Gemini Developer API; Vertex AI has no
// Decisions API yet (aistudio.google.com/docs/decisions-api, 2026-11-28).
const GeminiDecisionsEndpoint = "https://generativelanguage.googleapis.com"

// GeminiDecisionsAdapter talks to Gemini's Decisions API (gemini-decision-1).
// The questions are System One's; the state goes in "input" as content blocks
// and usage comes back per modality. Judgment-only, like the System One vendors.
type GeminiDecisionsAdapter struct {
	http     *http.Client
	endpoint string
	apiKey   string
	model    string
}

func NewGeminiDecisionsAdapter(endpoint, apiKey, model string) *GeminiDecisionsAdapter {
	if endpoint == "" {
		endpoint = GeminiDecisionsEndpoint
	}
	return &GeminiDecisionsAdapter{
		http:     llm.VendorHTTPClient(),
		endpoint: strings.TrimSuffix(endpoint, "/"),
		apiKey:   apiKey,
		model:    model,
	}
}

func (a *GeminiDecisionsAdapter) GenerateText(context.Context, *llm.ChatContext) (*llm.Response, error) {
	return nil, ErrTextGenerationUnsupported
}

type geminiDecisionQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

type geminiDecisionRequest struct {
	Model     string                            `json:"model"`
	Input     any                               `json:"input"`
	Questions map[string]geminiDecisionQuestion `json:"questions"`
}

type geminiDecisionResponse struct {
	Answers map[string]systemOneAnswer `json:"answers"`
	Usage   struct {
		TotalInputTokens  int `json:"total_input_tokens"`
		TotalOutputTokens int `json:"total_output_tokens"`
	} `json:"usage"`
}

// geminiDecisionBlock is one content block of "input".
type geminiDecisionBlock struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	Data       string `json:"data,omitempty"`
	URI        string `json:"uri,omitempty"`
	MimeType   string `json:"mime_type,omitempty"`
	Resolution string `json:"resolution,omitempty"`
}

// Judge asks every question against one input in a single call.
func (a *GeminiDecisionsAdapter) Judge(ctx context.Context, req *llm.JudgeRequest, _ string, _ map[string]string) (*llm.JudgeResult, error) {
	failed := func(err error) (*llm.JudgeResult, error) {
		return &llm.JudgeResult{Model: a.model}, err
	}
	if req == nil || len(req.Questions) == 0 {
		return failed(fmt.Errorf("gemini decisions: no questions"))
	}
	questions, err := toGeminiDecisionQuestions(req.Questions)
	if err != nil {
		return failed(err)
	}
	input, err := geminiDecisionInput(req)
	if err != nil {
		return failed(err)
	}
	body, err := json.Marshal(geminiDecisionRequest{Model: a.model, Input: input, Questions: questions})
	if err != nil {
		return failed(fmt.Errorf("gemini decisions: encode request: %w", err))
	}
	call := judgeCall{
		body:        body,
		contentType: "application/json",
		headers:     map[string]string{"x-goog-api-key": a.apiKey},
		timeout:     judgeTimeoutFor(len(body)),
	}
	raw, statusCode, err := executeJudgeCall(ctx, a.http, a.endpoint+"/v1beta/decisions", "", call)
	if err != nil {
		if statusCode > 0 {
			vendorErr := &SystemOneError{Vendor: "gemini decisions", Status: statusCode, Body: err.Error()}
			if statusCode == http.StatusBadRequest && geminiDecisionSafetyBlock(raw) {
				return failed(&geminiSafetyError{vendorErr})
			}
			return failed(vendorErr)
		}
		return failed(fmt.Errorf("gemini decisions: %w", err))
	}
	var parsed geminiDecisionResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return failed(fmt.Errorf("gemini decisions: decode response: %w", err))
	}
	result := &llm.JudgeResult{
		Answers: make(map[string]*llm.JudgeAnswer, len(parsed.Answers)),
		Model:   a.model,
		Usage:   llm.TokensUsage{Input: parsed.Usage.TotalInputTokens, Output: parsed.Usage.TotalOutputTokens},
	}
	for id, ans := range parsed.Answers {
		out := &llm.JudgeAnswer{
			Type:            ans.Type,
			Choice:          ans.Choice,
			Noul:            ans.Noul,
			Score:           ans.Score,
			Probabilities:   ans.Probabilities,
			Legend:          systemOneLegend(ans.Legend),
			ConfidenceUnset: ans.Confidence == nil,
		}
		if ans.Confidence != nil {
			out.Confidence = *ans.Confidence
		}
		result.Answers[id] = out
	}
	for id := range req.Questions {
		if _, ok := result.Answers[id]; !ok {
			return failed(fmt.Errorf("gemini decisions: no answer for question %q", id))
		}
	}
	return result, nil
}

// toGeminiDecisionQuestions renders questions in Gemini's shape: instructions
// are text only, so structured forms go flat; noul criteria are
// true_description/false_description. Box has no Gemini form: yield.
func toGeminiDecisionQuestions(in map[string]*llm.JudgeQuestion) (map[string]geminiDecisionQuestion, error) {
	out := make(map[string]geminiDecisionQuestion, len(in))
	for id, q := range in {
		native := geminiNoulCriteria(q)
		q = q.Flat()
		converted := geminiDecisionQuestion{Type: q.Type, Instructions: q.Instructions}
		switch q.Type {
		case llm.JudgeChoice:
			converted.Criteria = q.Options
		case llm.JudgeScore:
			converted.Criteria = q.Levels
		case llm.JudgeNoul:
			if native != nil {
				converted.Criteria = native
			} else if q.TrueCriteria != "" || q.FalseCriteria != "" {
				converted.Criteria = map[string]string{"true_description": q.TrueCriteria, "false_description": q.FalseCriteria}
			}
		default:
			return nil, llm.ErrJudgmentUnsupported
		}
		out[id] = converted
	}
	return out, nil
}

// geminiNoulCriteria passes noul criteria_json already in Gemini's form
// ({"true_description", "false_description"}) as it is.
func geminiNoulCriteria(q *llm.JudgeQuestion) map[string]string {
	if q.Type != llm.JudgeNoul || len(q.CriteriaJSON) == 0 {
		return nil
	}
	var m map[string]string
	if json.Unmarshal(q.CriteriaJSON, &m) != nil || (m["true_description"] == "" && m["false_description"] == "") {
		return nil
	}
	return map[string]string{"true_description": m["true_description"], "false_description": m["false_description"]}
}

// geminiDecisionInput is the state as Gemini's input: the state text alone, or
// content blocks with each media part after it. Named parts get a text label
// before them, since Gemini refers to blocks by position.
func geminiDecisionInput(req *llm.JudgeRequest) (any, error) {
	state := strings.TrimSpace(req.StateJSON)
	if state == "null" {
		state = ""
	}
	if len(req.StateMedia) == 0 {
		if state == "" {
			state = "{}"
		}
		return state, nil
	}
	var blocks []geminiDecisionBlock
	if state != "" {
		blocks = append(blocks, geminiDecisionBlock{Type: "text", Text: state})
	}
	for i, p := range req.StateMedia {
		if p.Name != "" {
			blocks = append(blocks, geminiDecisionBlock{Type: "text", Text: "`" + p.Name + "`:"})
		}
		b, err := geminiDecisionMedia(i, p)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, b)
	}
	return blocks, nil
}

func geminiDecisionMedia(i int, p *llm.JudgeMediaPart) (geminiDecisionBlock, error) {
	bad := func(msg string, args ...any) (geminiDecisionBlock, error) {
		return geminiDecisionBlock{}, &llm.InvalidMediaError{Part: i, Msg: fmt.Sprintf(msg, args...)}
	}
	var b geminiDecisionBlock
	switch p.Type {
	case llm.JudgeMediaText:
		return geminiDecisionBlock{Type: "text", Text: p.Text}, nil
	case llm.JudgeMediaImage:
		b = geminiDecisionBlock{Type: "image", MimeType: p.MimeType}
		if p.Detail == string(llm.ImageDetailLow) || p.Detail == string(llm.ImageDetailHigh) {
			b.Resolution = p.Detail
		}
	case llm.JudgeMediaAudio:
		b = geminiDecisionBlock{Type: "audio", MimeType: p.MimeType}
		if b.MimeType == "" && p.Format != "" {
			b.MimeType = "audio/" + p.Format
		}
	default:
		// Embeddings have no block: answering without them would judge another state.
		return geminiDecisionBlock{}, llm.ErrJudgmentUnsupported
	}
	switch {
	case p.URL != "":
		b.URI = p.URL
		if b.MimeType == "" {
			b.MimeType = urlMimeType(p.URL)
		}
		if b.MimeType == "" {
			// Gemini needs the type of a URL it fetches; another judge may not.
			return geminiDecisionBlock{}, fmt.Errorf("%w: %s url without a mime_type", llm.ErrJudgmentUnsupported, b.Type)
		}
	case len(p.Data) > 0:
		if b.MimeType == "" {
			b.MimeType = http.DetectContentType(p.Data)
		}
		b.Data = base64.StdEncoding.EncodeToString(p.Data)
	default:
		return bad("%s part has neither data nor url", b.Type)
	}
	if !strings.HasPrefix(b.MimeType, b.Type+"/") {
		return bad("%s part needs an %s/* mime_type for Gemini (got %q)", b.Type, b.Type, b.MimeType)
	}
	return b, nil
}

// CheckJudgeMedia applies the per-part checks Judge would, before any call, so
// a rollout batch with one bad part fails before a candidate bills.
func (a *GeminiDecisionsAdapter) CheckJudgeMedia(media []*llm.JudgeMediaPart, _ map[string]*llm.JudgeQuestion) error {
	_, err := geminiDecisionInput(&llm.JudgeRequest{StateMedia: media})
	return err
}

// geminiSafetyError is a safety block: like a content filter elsewhere, another
// judge in the chain may answer what Gemini refuses to.
type geminiSafetyError struct{ *SystemOneError }

func (e *geminiSafetyError) SkipToNextModel() bool { return true }
func (e *geminiSafetyError) Unwrap() error         { return e.SystemOneError }
func (e *geminiSafetyError) Is(target error) bool  { return target == llm.ErrContentBlocked }

// geminiSafetyReasons are the error codes, statuses and ErrorInfo reasons that
// mean a safety block, compared without case.
var geminiSafetyReasons = map[string]bool{"safety": true, "prohibited_content": true, "blocklist": true, "image_safety": true, "content_blocked": true}

// geminiDecisionSafetyBlock reads a Google error body: code is a number in the
// usual shape and a string in the Decisions examples, so both are read.
func geminiDecisionSafetyBlock(body []byte) bool {
	var e struct {
		Error struct {
			Code    json.RawMessage `json:"code"`
			Status  string          `json:"status"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return false
	}
	var code string
	_ = json.Unmarshal(e.Error.Code, &code) // a numeric code says nothing about safety
	reasons := []string{code, e.Error.Status}
	for _, d := range e.Error.Details {
		reasons = append(reasons, d.Reason)
	}
	for _, r := range reasons {
		if geminiSafetyReasons[strings.ToLower(r)] {
			return true
		}
	}
	return false
}

// urlMimeType guesses a media type from the URL path's extension, "" when unknown.
func urlMimeType(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	t, _, _ := strings.Cut(mime.TypeByExtension(path.Ext(u.Path)), ";")
	return t
}
