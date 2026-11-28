package adapters

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/ai-process/ktms/internal/llm"
)

// judgeTimeout caps one judgment call. Judgments are a request-path decision,
// not a generation: a slow one is worse than none. It is a ceiling, not the
// deadline — the caller's own context still wins when it is shorter.
const judgeTimeout = 8 * time.Second

// SystemOneProfile is how one vendor speaks the System One protocol: where the
// call goes, how it authenticates, which opt-in headers it wants and which
// images it takes. The request and answer shapes are the same for all of them.
type SystemOneProfile struct {
	Vendor            string                // names the vendor in errors
	Path              string                // appended to the endpoint, "/v1/systemone" when empty
	AuthHeader        string                // "Authorization" when empty
	AuthPrefix        string                // put before the key, e.g. "Bearer "
	ImagesHeader      string                // sent as "true" with images or box questions; "" when none is needed
	LongContextHeader string                // sent as "true" for long_context models; "" when the vendor has none
	Images            *SystemOneImageLimits // nil: text only, a request with images yields
	Media             SystemOneMedia
}

// SystemOneMedia is how a vendor takes images next to the state.
type SystemOneMedia int

const (
	// MediaAssets: multipart, each image a file part named in "assets" (TypeSafe).
	MediaAssets SystemOneMedia = iota
	// MediaImages: a top-level "images" array of data URLs (Liquid, Cloudflare).
	MediaImages
	// MediaStateParts: the state becomes OpenAI-style content parts, text then
	// image_url parts with data URLs (Perplexity).
	MediaStateParts
)

// TypeSafeProfile is TypeSafe's System One (Jev).
var TypeSafeProfile = SystemOneProfile{
	Vendor:            "typesafe",
	Path:              "/v1/systemone",
	AuthPrefix:        "Bearer ",
	ImagesHeader:      typeSafeImagesHeader,
	LongContextHeader: typeSafeLongContextHeader,
	Images:            &typeSafeImageLimits,
}

// SystemOneAdapter talks to a System One API. It is judgment-only: the vendor
// writes no text, so GenerateText is refused rather than faked.
type SystemOneAdapter struct {
	http     *http.Client
	profile  SystemOneProfile
	endpoint string
	apiKey   string
	model    string
	long     bool // ask for the vendor's long context on every call
}

// WithLongContext turns on System One's long context for this model.
func (a *SystemOneAdapter) WithLongContext(on bool) *SystemOneAdapter {
	a.long = on
	return a
}

// NewSystemOneAdapter builds an adapter for one vendor's System One.
func NewSystemOneAdapter(profile SystemOneProfile, endpoint, apiKey, model string) *SystemOneAdapter {
	if profile.Path == "" {
		profile.Path = "/v1/systemone"
	}
	if profile.AuthHeader == "" {
		profile.AuthHeader = "Authorization"
	}
	return &SystemOneAdapter{
		// No client-level Timeout: the per-request context carries the deadline,
		// and a client timeout would override a caller's shorter one.
		http:     llm.VendorHTTPClient(),
		profile:  profile,
		endpoint: strings.TrimSuffix(endpoint, "/"),
		apiKey:   apiKey,
		model:    model,
	}
}

func NewTypeSafeAdapter(endpoint, apiKey, model string) *SystemOneAdapter {
	return NewSystemOneAdapter(TypeSafeProfile, endpoint, apiKey, model)
}

// ErrTextGenerationUnsupported marks a judgment-only vendor asked to write text.
// The capability filter keeps these models out of text chains; this is the
// backstop for a registry that says otherwise.
var ErrTextGenerationUnsupported = fmt.Errorf("text generation not supported by this vendor")

func (a *SystemOneAdapter) GenerateText(context.Context, *llm.ChatContext) (*llm.Response, error) {
	return nil, ErrTextGenerationUnsupported
}

// SystemOneError is a non-2xx reply. The status drives retryability; the body is
// kept short and never logged for 401, where it may echo the credential.
type SystemOneError struct {
	Vendor string
	Status int
	Body   string
}

func (e *SystemOneError) Error() string {
	if e.Status == http.StatusUnauthorized {
		return fmt.Sprintf("%s: %d unauthorized", e.vendor(), e.Status)
	}
	return fmt.Sprintf("%s: %d %s", e.vendor(), e.Status, e.Body)
}

func (e *SystemOneError) vendor() string {
	if e.Vendor == "" {
		return "systemone"
	}
	return e.Vendor
}

// HTTPStatus makes 429 and 5xx (including the vendor's 529 overloaded) retryable
// through llm.IsRetryableError, leaving 401 and 422 terminal.
func (e *SystemOneError) HTTPStatus() int { return e.Status }

type systemOneQuestion struct {
	Type         string `json:"type"`
	Instructions any    `json:"instructions"` // a string, or the caller's JSON
	Criteria     any    `json:"criteria,omitempty"`
}

type systemOneRequest struct {
	State     json.RawMessage              `json:"state"`
	Model     string                       `json:"model"`
	Questions map[string]systemOneQuestion `json:"questions"`
	Assets    map[string]systemOneAsset    `json:"assets,omitempty"`
	Images    []string                     `json:"images,omitempty"` // data URLs, MediaImages
}

// systemOneAsset points an image reference at its multipart file part.
type systemOneAsset struct {
	Type   string `json:"type"`
	Part   string `json:"part"`
	Format string `json:"format"`
}

type systemOneAnswer struct {
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

type systemOneResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]systemOneAnswer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// Judge asks every question against one state in a single call. userID and meta
// are accepted for interface symmetry: the vendor takes neither, and the
// tracking adapter is what records them.
func (a *SystemOneAdapter) Judge(ctx context.Context, req *llm.JudgeRequest, _ string, _ map[string]string) (*llm.JudgeResult, error) {
	failed := func(err error) (*llm.JudgeResult, error) {
		return &llm.JudgeResult{Model: a.model}, err
	}
	if req == nil || len(req.Questions) == 0 {
		return failed(fmt.Errorf("%s: no questions", a.profile.Vendor))
	}
	call, err := a.systemOneCall(req)
	if err != nil {
		return failed(err)
	}
	if call.headers == nil {
		call.headers = map[string]string{}
	}
	if a.long && a.profile.LongContextHeader != "" {
		call.headers[a.profile.LongContextHeader] = "true"
	}
	call.headers[a.profile.AuthHeader] = a.profile.AuthPrefix + a.apiKey

	raw, statusCode, err := executeJudgeCall(ctx, a.http, a.endpoint+strings.ReplaceAll(a.profile.Path, "{model}", url.PathEscape(a.model)), "", call)
	if err != nil {
		if statusCode > 0 {
			tsErr := &SystemOneError{Vendor: a.profile.Vendor, Status: statusCode, Body: err.Error()}
			if a.profile.Media == MediaAssets && statusCode == http.StatusBadRequest && strings.Contains(tsErr.Body, "max_tokens_exceeded") {
				return failed(fmt.Errorf("%w: an image named in a question is sent again for each such question, "+
					"so ask fewer of them per call or name the image in state_json instead: %w", llm.ErrJudgeTooLarge, tsErr))
			}
			return failed(tsErr)
		}
		return failed(fmt.Errorf("%s: %w", a.profile.Vendor, err))
	}

	var parsed systemOneResponse
	if err := json.Unmarshal(systemOneUnwrap(raw), &parsed); err != nil {
		return failed(fmt.Errorf("%s: decode response: %w", a.profile.Vendor, err))
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
			Legend:          systemOneLegend(ans.Legend),
			ConfidenceUnset: ans.Confidence == nil,
		}
		if ans.Confidence != nil {
			out.Confidence = *ans.Confidence
		}
		if ans.Box != nil {
			out.Box = &llm.Box{Top: ans.Box.Top, Bottom: ans.Box.Bottom, Left: ans.Box.Left, Right: ans.Box.Right}
		} else if ans.Type == llm.JudgeBox {
			return failed(fmt.Errorf("%s: box answer %q has no box", a.profile.Vendor, id))
		}
		result.Answers[id] = out
	}
	// A missing answer would read as a zero-valued judgment downstream, which
	// is a decision nobody made.
	for id := range req.Questions {
		if _, ok := result.Answers[id]; !ok {
			return failed(fmt.Errorf("%s: no answer for question %q", a.profile.Vendor, id))
		}
	}
	return result, nil
}

// toSystemOneQuestions renders the vendor-neutral questions in the vendor's
// shape: criteria is a map for choice, a list for score, a true/false pair for
// noul, and absent when a noul states no criteria.
func toSystemOneQuestions(in map[string]*llm.JudgeQuestion) map[string]systemOneQuestion {
	out := make(map[string]systemOneQuestion, len(in))
	for id, q := range in {
		converted := systemOneQuestion{Type: q.Type, Instructions: q.Instructions}
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
func (a *SystemOneAdapter) CheckJudgeMedia(media []*llm.JudgeMediaPart, questions map[string]*llm.JudgeQuestion) error {
	_, err := a.checkImages(media, hasBox(questions))
	return err
}

func hasBox(questions map[string]*llm.JudgeQuestion) bool {
	for _, q := range questions {
		if q.Type == llm.JudgeBox {
			return true
		}
	}
	return false
}

// systemImage is one checked image with the reference the request uses for it.
type systemImage struct {
	ref, format string
	named       bool
	data        []byte
}

// checkImages applies the vendor's limits to every part before any call. Only
// image bytes reach System One: a URL it cannot fetch through us, and audio,
// embeddings or loose text it has no slot for. Judging without them would answer
// about another state, so it yields instead.
func (a *SystemOneAdapter) checkImages(media []*llm.JudgeMediaPart, box bool) ([]systemImage, error) {
	lim := a.profile.Images
	if lim != nil && lim.MaxImages > 0 && len(media) > lim.MaxImages {
		return nil, &llm.InvalidMediaError{Part: lim.MaxImages, Msg: fmt.Sprintf("%d images; %s takes at most %d per call", len(media), lim.Name, lim.MaxImages), VendorLimit: true}
	}
	if lim != nil && box && lim.MaxImagesWithBox > 0 && len(media) > lim.MaxImagesWithBox {
		return nil, &llm.InvalidMediaError{Part: lim.MaxImagesWithBox, Msg: fmt.Sprintf("%d images; %s answers a box question about %d image only", len(media), lim.Name, lim.MaxImagesWithBox), VendorLimit: true}
	}
	out := make([]systemImage, 0, len(media))
	refs := make(map[string]bool, len(media))
	for i, p := range media {
		if p.Type != llm.JudgeMediaImage || len(p.Data) == 0 || lim == nil {
			return nil, llm.ErrJudgmentUnsupported
		}
		format, err := lim.format(i, p.Data)
		if err != nil {
			return nil, err
		}
		ref, err := systemOneImageRef(i, p.Name)
		if err != nil {
			return nil, err
		}
		if refs[ref] {
			return nil, &llm.InvalidMediaError{Part: i, Msg: fmt.Sprintf("name %q is used twice", ref)}
		}
		refs[ref] = true
		out = append(out, systemImage{ref: ref, format: format, named: p.Name != "", data: p.Data})
	}
	return out, nil
}

// systemOneImageRef is the name the vendor sees for image i. Mentions are
// searched in NUL-separated text, so a NUL in a name could match across strings.
func systemOneImageRef(i int, name string) (string, error) {
	if strings.ContainsRune(name, 0) {
		return "", &llm.InvalidMediaError{Part: i, Msg: "name contains a NUL character"}
	}
	if name == "" {
		return fmt.Sprintf("ktms-image-%d", i+1), nil
	}
	return name, nil
}

// systemOneCall builds the request: plain JSON without images, else multipart
// with each image as a file part named in "assets". A named image is referred
// to by its name wherever the caller wrote it; unnamed images get a generated
// reference and, like named ones nothing mentions, are listed next to the state.
func (a *SystemOneAdapter) systemOneCall(req *llm.JudgeRequest) (judgeCall, error) {
	sr := systemOneRequest{
		State:     json.RawMessage(req.StateJSON),
		Model:     a.model,
		Questions: toSystemOneQuestions(req.Questions),
	}
	box := hasBox(req.Questions)
	if len(req.StateMedia) == 0 {
		if len(sr.State) == 0 || strings.TrimSpace(string(sr.State)) == "null" {
			sr.State = systemOneNoState
		}
		body, err := json.Marshal(sr)
		if err != nil {
			return judgeCall{}, fmt.Errorf("%s: encode request: %w", a.profile.Vendor, err)
		}
		call := judgeCall{body: body, contentType: "application/json"}
		if box && a.profile.ImagesHeader != "" {
			call.headers = map[string]string{a.profile.ImagesHeader: "true"}
		}
		return call, nil
	}

	images, err := a.checkImages(req.StateMedia, box)
	if err != nil {
		return judgeCall{}, err
	}
	switch a.profile.Media {
	case MediaImages, MediaStateParts:
		return a.inlineImagesCall(sr, images)
	}
	var listed []string // refs System One would otherwise not see
	mentioned := systemOneMentions(sr.State, sr.Questions)
	sr.Assets = make(map[string]systemOneAsset, len(images))
	for i, img := range images {
		// System One refuses an image nothing refers to ("Invalid image request"),
		// and it finds a name anywhere in the state or questions text.
		if !img.named || !strings.Contains(mentioned, img.ref) {
			listed = append(listed, img.ref)
		}
		sr.Assets[img.ref] = systemOneAsset{Type: "image", Part: fmt.Sprintf("image-%d", i+1), Format: img.format}
	}
	sr.State = unnamedState(sr.State, listed)

	reqJSON, err := json.Marshal(sr)
	if err != nil {
		return judgeCall{}, fmt.Errorf("%s: encode request: %w", a.profile.Vendor, err)
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
	for i, img := range images {
		if err != nil {
			break
		}
		part := fmt.Sprintf("image-%d", i+1)
		fh := textproto.MIMEHeader{}
		fh.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, part, part+"."+img.format))
		fh.Set("Content-Type", img.mime())
		if w, err = mw.CreatePart(fh); err == nil {
			_, err = w.Write(img.data)
		}
	}
	if err == nil {
		err = mw.Close()
	}
	if err != nil {
		return judgeCall{}, fmt.Errorf("%s: encode multipart: %w", a.profile.Vendor, err)
	}
	return judgeCall{
		body:        buf.Bytes(),
		contentType: mw.FormDataContentType(),
		headers:     a.imagesHeaders(),
		timeout:     judgeTimeoutFor(buf.Len()),
	}, nil
}

func (img systemImage) mime() string {
	for m, f := range imageFormats {
		if f == img.format {
			return m
		}
	}
	return "application/octet-stream"
}

func (img systemImage) dataURL() string {
	return "data:" + img.mime() + ";base64," + base64.StdEncoding.EncodeToString(img.data)
}

// inlineImagesCall sends images inside the JSON body. These vendors take images
// by position, not by name: when the caller named any, the names go with the
// state in the images' order, so instructions that say `road` still point somewhere.
func (a *SystemOneAdapter) inlineImagesCall(sr systemOneRequest, images []systemImage) (judgeCall, error) {
	state := sr.State
	if strings.TrimSpace(string(state)) == "null" {
		state = nil
	}
	named := false
	for _, img := range images {
		named = named || img.named
	}
	switch a.profile.Media {
	case MediaImages:
		if named {
			refs := make([]*string, len(images)) // null for unnamed: generated names stay inside the proxy
			for i, img := range images {
				if img.named {
					refs[i] = &img.ref
				}
			}
			wrapped := map[string]any{"images": refs}
			if len(state) > 0 {
				wrapped["state"] = state
			}
			state, _ = json.Marshal(wrapped) // strings and raw JSON always marshal
		}
		for _, img := range images {
			sr.Images = append(sr.Images, img.dataURL())
		}
	case MediaStateParts:
		var parts []map[string]any
		if len(state) > 0 {
			parts = append(parts, map[string]any{"type": "text", "text": string(state)})
		}
		for _, img := range images {
			if img.named {
				parts = append(parts, map[string]any{"type": "text", "text": "Image `" + img.ref + "`:"})
			}
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": img.dataURL()}})
		}
		state, _ = json.Marshal(parts)
	}
	if len(state) == 0 {
		state = systemOneNoState
	}
	sr.State = state
	body, err := json.Marshal(sr)
	if err != nil {
		return judgeCall{}, fmt.Errorf("%s: encode request: %w", a.profile.Vendor, err)
	}
	if lim := a.profile.Images; lim.MaxBody > 0 && len(body) > lim.MaxBody {
		return judgeCall{}, &llm.InvalidMediaError{Part: 0, VendorLimit: true,
			Msg: fmt.Sprintf("request is %d bytes with its images as base64, max %d for %s", len(body), lim.MaxBody, lim.Name)}
	}
	return judgeCall{
		body:        body,
		contentType: "application/json",
		headers:     a.imagesHeaders(),
		timeout:     judgeTimeoutFor(len(body)),
	}, nil
}

// systemOneUnwrap reads answers from a {"result": {...}, "success": ...}
// envelope (Cloudflare's REST API) as well as from a bare System One reply.
func systemOneUnwrap(raw []byte) []byte {
	var env struct {
		Result  json.RawMessage `json:"result"`
		Answers json.RawMessage `json:"answers"`
	}
	if json.Unmarshal(raw, &env) == nil && len(env.Answers) == 0 && len(env.Result) > 0 && env.Result[0] == '{' {
		return env.Result
	}
	return raw
}

func (a *SystemOneAdapter) imagesHeaders() map[string]string {
	if a.profile.ImagesHeader == "" {
		return map[string]string{}
	}
	return map[string]string{a.profile.ImagesHeader: "true"}
}

// systemOneNoState stands for a missing state: System One requires the field
// and refuses null with a 422.
var systemOneNoState = json.RawMessage("{}")

// systemOneLegend reads a legend whose entries are strings or, for criteria
// given as {"description", "examples"}, objects: those read as their description.
func systemOneLegend(in map[string]json.RawMessage) map[string]string {
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

// systemOneMentions is the decoded text System One searches for image names, as
// in 1.8.0 keys and numbers included. Absent criteria add no "null": a name
// left unlisted because of it is a 400, and a name listed needlessly reshapes the state.
func systemOneMentions(state json.RawMessage, questions map[string]systemOneQuestion) string {
	var b strings.Builder
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			b.WriteString(v)
			b.WriteByte(0)
		case json.Number:
			b.WriteString(v.String())
			b.WriteByte(0)
		case []any:
			for _, e := range v {
				walk(e)
			}
		case map[string]any:
			for k, e := range v {
				b.WriteString(k)
				b.WriteByte(0)
				walk(e)
			}
		}
	}
	decode := func(raw []byte) {
		var v any
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		if d.Decode(&v) != nil || d.Decode(new(any)) != io.EOF {
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
			return systemOneNoState
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
