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
	long     bool // TypeSafe-Danger-Zone-Long-Context on every call
}

// WithLongContext turns on System One's long context for this model.
func (a *TypeSafeAdapter) WithLongContext(on bool) *TypeSafeAdapter {
	a.long = on
	return a
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
	Instructions any    `json:"instructions"` // a string, or the caller's JSON
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
	Type          string                     `json:"type"`
	Choice        string                     `json:"choice"`
	Noul          float64                    `json:"noul"`
	Score         float64                    `json:"score"`
	Probabilities map[string]float64         `json:"probabilities"`
	Confidence    *float64                   `json:"confidence"`
	Legend        map[string]json.RawMessage `json:"legend"`
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

// typeSafeLongContextHeader raises the context to 262k tokens (Jev 1.13+).
const typeSafeLongContextHeader = "TypeSafe-Danger-Zone-Long-Context"

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
	if a.long {
		if call.headers == nil {
			call.headers = map[string]string{}
		}
		call.headers[typeSafeLongContextHeader] = "true"
	}

	raw, statusCode, err := executeJudgeCall(ctx, a.http, a.endpoint+"/v1/systemone", a.apiKey, call)
	if err != nil {
		if statusCode > 0 {
			tsErr := &TypeSafeError{Status: statusCode, Body: err.Error()}
			if statusCode == http.StatusBadRequest && strings.Contains(tsErr.Body, "max_tokens_exceeded") {
				return failed(fmt.Errorf("%w: an image named in a question is sent again for each such question, "+
					"so ask fewer of them per call or name the image in state_json instead: %w", llm.ErrJudgeTooLarge, tsErr))
			}
			return failed(tsErr)
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
			Legend:          typeSafeLegend(ans.Legend),
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
		if len(q.InstructionsJSON) > 0 {
			converted.Instructions = q.InstructionsJSON
		}
		if len(q.CriteriaJSON) > 0 {
			converted.Criteria = q.CriteriaJSON
			out[id] = converted
			continue
		}
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

// CheckJudgeMedia runs the checks systemOneCall would, without building a
// body, so a rollout batch with one bad image fails before any candidate bills.
func (a *TypeSafeAdapter) CheckJudgeMedia(media []*llm.JudgeMediaPart) error {
	refs := make(map[string]bool, len(media))
	for i, p := range media {
		if p.Type != llm.JudgeMediaImage || len(p.Data) == 0 {
			return llm.ErrJudgmentUnsupported
		}
		if _, err := typeSafeImageFormat(i, p.Data); err != nil {
			return err
		}
		ref := p.Name
		if ref == "" {
			ref = fmt.Sprintf("ktms-image-%d", i+1)
		}
		if refs[ref] {
			return &llm.InvalidMediaError{Part: i, Msg: fmt.Sprintf("name %q is used twice", ref)}
		}
		refs[ref] = true
	}
	return nil
}

// systemOneCall builds the request: plain JSON without images, else multipart
// with each image as a file part named in "assets". A named image is referred
// to by its name wherever the caller wrote it; unnamed images get a generated
// reference and, like named ones nothing mentions, are listed next to the state.
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
		if len(sr.State) == 0 || strings.TrimSpace(string(sr.State)) == "null" {
			sr.State = typeSafeNoState
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
	var listed []string // refs System One would otherwise not see
	mentioned := typeSafeMentions(sr.State, sr.Questions)
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
		}
		// System One refuses an image nothing refers to ("Invalid image request"),
		// and it finds a name anywhere in the state or questions text.
		if p.Name == "" || !strings.Contains(mentioned, ref) {
			listed = append(listed, ref)
		}
		if _, dup := sr.Assets[ref]; dup {
			return judgeCall{}, &llm.InvalidMediaError{Part: i, Msg: fmt.Sprintf("name %q is used twice", ref)}
		}
		part := fmt.Sprintf("image-%d", i+1)
		sr.Assets[ref] = typeSafeAssetEntry{Type: "image", Part: part, Format: format}
		files = append(files, file{part: part, format: format, data: p.Data})
	}
	sr.State = unnamedState(sr.State, listed)

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

// typeSafeNoState stands for a missing state: System One requires the field
// and refuses null with a 422.
var typeSafeNoState = json.RawMessage("{}")

// typeSafeLegend reads a legend whose entries are strings or, for criteria
// given as {"description", "examples"}, objects: those read as their description.
func typeSafeLegend(in map[string]json.RawMessage) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, raw := range in {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			out[k] = s
			continue
		}
		var obj struct {
			Description *string `json:"description"`
		}
		if json.Unmarshal(raw, &obj) == nil && obj.Description != nil {
			out[k] = *obj.Description
			continue
		}
		var b bytes.Buffer
		if json.Compact(&b, raw) != nil {
			b.Reset()
			b.Write(raw)
		}
		out[k] = b.String()
	}
	return out
}

// typeSafeMentions is the text System One searches for image names: the string
// values of the state and of each question's instructions and criteria, decoded
// (no JSON escapes, no "null" for absent criteria) and kept apart by NULs. Keys
// and numbers do not count: listing an image twice is harmless, missing one is a 400.
func typeSafeMentions(state json.RawMessage, questions map[string]typeSafeQuestion) string {
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			b.WriteString(v)
			b.WriteByte(0)
		case []any:
			for _, e := range v {
				walk(e)
			}
		case map[string]any:
			for _, e := range v {
				walk(e)
			}
		}
	}
	decode := func(raw []byte) {
		var v any
		if json.Unmarshal(raw, &v) != nil {
			v = string(raw) // not JSON: its text is all there is to search
		}
		walk(v)
	}
	decode(state)
	for _, q := range questions {
		for _, v := range []any{q.Instructions, q.Criteria} {
			if raw, err := json.Marshal(v); err == nil {
				decode(raw)
			}
		}
	}
	return b.String()
}

// unnamedState puts references to unnamed images where System One sees them:
// the state itself when the caller sent none, else {"state": ..., "images": [...]}.
func unnamedState(state json.RawMessage, refs []string) json.RawMessage {
	if strings.TrimSpace(string(state)) == "null" {
		state = nil // an explicit null is no state either
	}
	if len(refs) == 0 {
		if len(state) == 0 {
			return typeSafeNoState
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
