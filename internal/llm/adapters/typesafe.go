package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/ai-process/ktms/internal/llm"
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
		http:     llm.VendorHTTPClient(),
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
	State     json.RawMessage               `json:"state"`
	Model     string                        `json:"model"`
	Questions map[string]typeSafeQuestion   `json:"questions"`
	Assets    map[string]typeSafeAssetEntry `json:"assets,omitempty"`
}

// typeSafeAssetEntry points an image reference at its multipart file part.
type typeSafeAssetEntry struct {
	Type   string `json:"type"`
	Part   string `json:"part"`
	Format string `json:"format"`
}

type typeSafeAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Noul          float64            `json:"noul"`
	Score         float64            `json:"score"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    *float64           `json:"confidence"`
	Legend        map[string]string  `json:"legend"`
	Box           *struct {
		Top    float64 `json:"top"`
		Bottom float64 `json:"bottom"`
		Left   float64 `json:"left"`
		Right  float64 `json:"right"`
	} `json:"box"`
}

// typeSafeImagesHeader opts into images and Box ("Dreamer-ready" at TypeSafe):
// without it System One answers 400.
const typeSafeImagesHeader = "TypeSafe-Danger-Zone-Images"

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
	call, err := a.systemOneCall(req)
	if err != nil {
		return failed(err)
	}

	raw, statusCode, err := executeJudgeCall(ctx, a.http, a.endpoint+"/v1/systemone", a.apiKey, call)
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
		out := &llm.JudgeAnswer{
			Type:            ans.Type,
			Choice:          ans.Choice,
			Noul:            ans.Noul,
			Score:           ans.Score,
			Probabilities:   ans.Probabilities,
			Legend:          ans.Legend,
			ConfidenceUnset: ans.Confidence == nil,
		}
		if ans.Confidence != nil {
			out.Confidence = *ans.Confidence
		}
		if ans.Box != nil {
			out.Box = &llm.Box{Top: ans.Box.Top, Bottom: ans.Box.Bottom, Left: ans.Box.Left, Right: ans.Box.Right}
		} else if ans.Type == llm.JudgeBox {
			return failed(fmt.Errorf("typesafe: box answer %q has no box", id))
		}
		result.Answers[id] = out
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

// systemOneCall builds the request: plain JSON without images, else multipart
// with each image as a file part named in "assets". A named image is referred
// to by its name wherever the caller wrote it; unnamed images get a generated
// reference, listed next to the caller's state (see unnamedState).
func (a *TypeSafeAdapter) systemOneCall(req *llm.JudgeRequest) (judgeCall, error) {
	sr := typeSafeRequest{
		State:     json.RawMessage(req.StateJSON),
		Model:     a.model,
		Questions: toTypeSafeQuestions(req.Questions),
	}
	box := false
	for _, q := range req.Questions {
		box = box || q.Type == llm.JudgeBox
	}
	if len(req.StateMedia) == 0 {
		if len(sr.State) == 0 {
			sr.State = json.RawMessage("null")
		}
		body, err := json.Marshal(sr)
		if err != nil {
			return judgeCall{}, fmt.Errorf("typesafe: encode request: %w", err)
		}
		call := judgeCall{body: body, contentType: "application/json"}
		if box {
			call.headers = map[string]string{typeSafeImagesHeader: "true"}
		}
		return call, nil
	}

	type file struct {
		part, format string
		data         []byte
	}
	var files []file
	var unnamed []string
	sr.Assets = make(map[string]typeSafeAssetEntry, len(req.StateMedia))
	for i, p := range req.StateMedia {
		// Only image bytes reach System One: a URL it cannot fetch through
		// us, and audio, embeddings or loose text it has no slot for. Judging
		// without them would answer about another state, so yield instead.
		if p.Type != llm.JudgeMediaImage || len(p.Data) == 0 {
			return judgeCall{}, llm.ErrJudgmentUnsupported
		}
		format, err := typeSafeImageFormat(i, p.Data)
		if err != nil {
			return judgeCall{}, err
		}
		ref := p.Name
		if ref == "" {
			ref = fmt.Sprintf("ktms-image-%d", i+1)
			unnamed = append(unnamed, ref)
		}
		if _, dup := sr.Assets[ref]; dup {
			return judgeCall{}, &llm.InvalidMediaError{Msg: fmt.Sprintf("state_media[%d]: name %q is used twice", i, ref)}
		}
		part := fmt.Sprintf("image-%d", i+1)
		sr.Assets[ref] = typeSafeAssetEntry{Type: "image", Part: part, Format: format}
		files = append(files, file{part: part, format: format, data: p.Data})
	}
	sr.State = unnamedState(sr.State, unnamed)

	reqJSON, err := json.Marshal(sr)
	if err != nil {
		return judgeCall{}, fmt.Errorf("typesafe: encode request: %w", err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="request"`)
	h.Set("Content-Type", "application/json")
	w, err := mw.CreatePart(h)
	if err == nil {
		_, err = w.Write(reqJSON)
	}
	for _, f := range files {
		if err != nil {
			break
		}
		fh := textproto.MIMEHeader{}
		fh.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, f.part, f.part+"."+f.format))
		fh.Set("Content-Type", "image/"+map[string]string{"png": "png", "jpg": "jpeg"}[f.format])
		if w, err = mw.CreatePart(fh); err == nil {
			_, err = w.Write(f.data)
		}
	}
	if err == nil {
		err = mw.Close()
	}
	if err != nil {
		return judgeCall{}, fmt.Errorf("typesafe: encode multipart: %w", err)
	}
	return judgeCall{
		body:        buf.Bytes(),
		contentType: mw.FormDataContentType(),
		headers:     map[string]string{typeSafeImagesHeader: "true"},
		timeout:     judgeTimeoutFor(buf.Len()),
	}, nil
}

// unnamedState puts references to unnamed images where System One sees them:
// the state itself when the caller sent none, else {"state": ..., "images": [...]}.
func unnamedState(state json.RawMessage, refs []string) json.RawMessage {
	if strings.TrimSpace(string(state)) == "null" {
		state = nil // an explicit null is no state either
	}
	if len(refs) == 0 {
		if len(state) == 0 {
			return json.RawMessage("null")
		}
		return state
	}
	var v any = refs
	if len(refs) == 1 {
		v = refs[0]
	}
	if len(state) > 0 {
		v = map[string]any{"state": state, "images": refs}
	}
	out, _ := json.Marshal(v) // strings and raw JSON always marshal
	return out
}

// judgeTimeoutFor gives a request with images time to upload and be read:
// the text-only ceiling plus 2 s per MiB, at most a minute.
func judgeTimeoutFor(bodyBytes int) time.Duration {
	t := judgeTimeout + time.Duration(bodyBytes>>20)*2*time.Second
	return min(t, time.Minute)
}
