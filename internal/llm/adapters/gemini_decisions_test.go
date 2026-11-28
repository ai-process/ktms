package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ai-process/ktms/internal/llm"
)

const geminiDecisionReply = `{"answers":{
	"ok":{"type":"noul","noul":0.99,"confidence":0.99},
	"topic":{"type":"choice","choice":"billing","probabilities":{"billing":0.99,"other":0.01},"confidence":0.99},
	"wear":{"type":"score","score":1.12,"legend":{"0":"intact","1":"cracks","2":"potholes"},"probabilities":{"0":0.03,"1":0.83,"2":0.15},"confidence":0.83}},
	"usage":{"total_input_tokens":100,"input_tokens_by_modality":[{"modality":"text","tokens":100}],"total_output_tokens":0,"total_tokens":100}}`

func geminiDecisionServer(t *testing.T, got *map[string]json.RawMessage, key, path *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*key, *path = r.Header.Get("x-goog-api-key"), r.URL.Path
		if r.Header.Get("Authorization") != "" {
			t.Error("Authorization sent to Gemini")
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, got)
		_, _ = io.WriteString(w, geminiDecisionReply)
	}))
}

func geminiQuestions() map[string]*llm.JudgeQuestion {
	return map[string]*llm.JudgeQuestion{
		"ok":    {Type: llm.JudgeNoul, Instructions: "usable?", TrueCriteria: "a road is visible", FalseCriteria: "no road"},
		"topic": {Type: llm.JudgeChoice, Instructions: "what?", Options: map[string]string{"billing": "payments", "other": ""}},
		"wear":  {Type: llm.JudgeScore, Instructions: "how worn?", Levels: []string{"intact", "cracks", "potholes"}},
	}
}

func TestGeminiDecisionsTextRequest(t *testing.T) {
	var got map[string]json.RawMessage
	var key, path string
	srv := geminiDecisionServer(t, &got, &key, &path)
	defer srv.Close()
	res, err := NewGeminiDecisionsAdapter(srv.URL, "AIza-k", "gemini-decision-1").Judge(context.Background(),
		&llm.JudgeRequest{StateJSON: `{"message":"charged twice"}`, Questions: geminiQuestions()}, "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	if key != "AIza-k" || path != "/v1beta/decisions" {
		t.Errorf("key %q path %q", key, path)
	}
	if string(got["input"]) != `"{\"message\":\"charged twice\"}"` || string(got["model"]) != `"gemini-decision-1"` {
		t.Errorf("input %s model %s", got["input"], got["model"])
	}
	if _, ok := got["state"]; ok {
		t.Error("state sent to Gemini")
	}
	var qs map[string]geminiDecisionQuestion
	_ = json.Unmarshal(got["questions"], &qs)
	crit, _ := json.Marshal(qs["ok"].Criteria)
	if string(crit) != `{"false_description":"no road","true_description":"a road is visible"}` {
		t.Errorf("noul criteria = %s", crit)
	}
	if res.Answers["ok"].Noul != 0.99 || res.Answers["topic"].Choice != "billing" || res.Answers["wear"].Legend["1"] != "cracks" ||
		res.Usage.Input != 100 || res.Usage.Output != 0 || res.Model != "gemini-decision-1" {
		t.Errorf("result = %+v", res)
	}
}

// Media go as content blocks after the state, a named part labelled before it.
func TestGeminiDecisionsMediaBlocks(t *testing.T) {
	var got map[string]json.RawMessage
	var key, path string
	srv := geminiDecisionServer(t, &got, &key, &path)
	defer srv.Close()
	_, err := NewGeminiDecisionsAdapter(srv.URL, "k", "gemini-decision-1").Judge(context.Background(), &llm.JudgeRequest{
		StateJSON: `{"camera":"front"}`,
		StateMedia: []*llm.JudgeMediaPart{
			{Type: llm.JudgeMediaImage, Name: "road", Data: jpegOf(t, 640, 480), Detail: "low"},
			{Type: llm.JudgeMediaAudio, Data: []byte("RIFF"), Format: "wav"},
			{Type: llm.JudgeMediaImage, URL: "https://example.test/a.png", MimeType: "image/png"},
		},
		Questions: geminiQuestions(),
	}, "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	var blocks []geminiDecisionBlock
	if err := json.Unmarshal(got["input"], &blocks); err != nil || len(blocks) != 5 {
		t.Fatalf("input = %.300s (%v)", got["input"], err)
	}
	if blocks[0].Text != `{"camera":"front"}` || blocks[1].Text != "`road`:" ||
		blocks[2].Type != "image" || blocks[2].MimeType != "image/jpeg" || blocks[2].Resolution != "low" || blocks[2].Data == "" ||
		blocks[3].Type != "audio" || blocks[3].MimeType != "audio/wav" ||
		blocks[4].URI != "https://example.test/a.png" || blocks[4].Data != "" {
		t.Errorf("blocks = %+v", blocks)
	}
}

// Box and embeddings have no Gemini form: the chain moves on.
func TestGeminiDecisionsYields(t *testing.T) {
	a := NewGeminiDecisionsAdapter("http://unused", "k", "gemini-decision-1")
	for name, req := range map[string]*llm.JudgeRequest{
		"box": {StateJSON: `{}`, Questions: map[string]*llm.JudgeQuestion{"b": {Type: llm.JudgeBox, Instructions: "?"}}},
		"embedding": {StateMedia: []*llm.JudgeMediaPart{{Type: llm.JudgeMediaEmbedding, Embedding: []float32{1}}},
			Questions: map[string]*llm.JudgeQuestion{"q": {Type: llm.JudgeNoul, Instructions: "?"}}},
	} {
		if _, err := a.Judge(context.Background(), req, "u", nil); !errors.Is(err, llm.ErrJudgmentUnsupported) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := a.GenerateText(context.Background(), llm.NewChatContext()); err == nil {
		t.Error("text generation accepted")
	}
}

// A safety block moves to the next judge; any other 400 ends the chain.
func TestGeminiDecisionsSafetySkipsToNextModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"safety","message":"Request blocked due to safety violations"}}`)
	}))
	defer srv.Close()
	_, err := NewGeminiDecisionsAdapter(srv.URL, "k", "gemini-decision-1").Judge(context.Background(),
		&llm.JudgeRequest{StateJSON: `{}`, Questions: geminiQuestions()}, "u", nil)
	if err == nil || llm.IsRetryableError(err) || !llm.ShouldSkipToNextModel(err) || !errors.Is(err, llm.ErrContentBlocked) ||
		!strings.HasPrefix(err.Error(), "gemini decisions: 400") {
		t.Fatalf("err = %v", err)
	}
	if geminiDecisionSafetyBlock([]byte(`{"error":{"code":"invalid_argument","message":"too many questions"}}`)) || geminiDecisionSafetyBlock([]byte("not json")) {
		t.Error("a limit error or a non-JSON body read as a safety block")
	}
}

func TestGeminiDecisionSafetyBlockClassification(t *testing.T) {
	long := `{"error":{"message":"` + strings.Repeat("x", 400) + `","code":"image_safety"}}`
	for body, want := range map[string]bool{
		`{"error":{"code":"prohibited_content","message":"no"}}`: true,
		`{"error":{"code":"blocklist"}}`:                         true,
		`{"error":{"code":"SAFETY"}}`:                            true,
		long:                                                     true, // past 300 bytes, code after message
		`{"error":{"code":400,"message":"blocked","status":"SAFETY"}}`:                                   true,
		`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"reason":"PROHIBITED_CONTENT"}]}}`: true,
		`{"error":{"code":400,"message":"bad","status":"INVALID_ARGUMENT"}}`:                             false,
		`{"error":{"code":"invalid_argument"}}`:                                                          false,
		`<html>bad gateway</html>`:                                                                       false,
	} {
		if got := geminiDecisionSafetyBlock([]byte(body)); got != want {
			t.Errorf("%.60s: %v, want %v", body, got, want)
		}
	}
}

// Only a 400 can be a safety block: a 503 naming one is still retried.
func TestGeminiDecisionsSafetyCodeOn5xxRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"safety","message":"overloaded"}}`)
	}))
	defer srv.Close()
	_, err := NewGeminiDecisionsAdapter(srv.URL, "k", "gemini-decision-1").Judge(context.Background(),
		&llm.JudgeRequest{StateJSON: `{}`, Questions: geminiQuestions()}, "u", nil)
	if !llm.IsRetryableError(err) || llm.ShouldSkipToNextModel(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestGeminiDecisionsQuestionForms(t *testing.T) {
	qs, err := toGeminiDecisionQuestions(map[string]*llm.JudgeQuestion{
		"native":     {Type: llm.JudgeNoul, Instructions: "?", CriteriaJSON: json.RawMessage(`{"true_description":"broken","false_description":"fine"}`)},
		"systemone":  {Type: llm.JudgeNoul, Instructions: "?", CriteriaJSON: json.RawMessage(`{"true":"broken","false":"fine"}`)},
		"structured": {Type: llm.JudgeChoice, InstructionsJSON: json.RawMessage(`["Which defect is in","road","?"]`), Options: map[string]string{"a": "x", "b": "y"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"native", "systemone"} {
		c, _ := json.Marshal(qs[id].Criteria)
		if string(c) != `{"false_description":"fine","true_description":"broken"}` {
			t.Errorf("%s criteria = %s", id, c)
		}
	}
	if qs["structured"].Instructions != `["Which defect is in","road","?"]` {
		t.Errorf("structured instructions = %q", qs["structured"].Instructions)
	}
}

func TestGeminiDecisionMediaParts(t *testing.T) {
	b, err := geminiDecisionMedia(0, &llm.JudgeMediaPart{Type: llm.JudgeMediaAudio, URL: "https://example.test/a.wav", MimeType: "audio/wav"})
	if err != nil || b.URI == "" || b.Type != "audio" {
		t.Errorf("audio url: %+v %v", b, err)
	}
	b, err = geminiDecisionMedia(0, &llm.JudgeMediaPart{Type: llm.JudgeMediaImage, Data: pngOf(t, 320, 240), Detail: "high"})
	if err != nil || b.Resolution != "high" || b.MimeType != "image/png" {
		t.Errorf("image: %+v %v", b, err)
	}
	for name, p := range map[string]*llm.JudgeMediaPart{
		"audio without a type": {Type: llm.JudgeMediaAudio, Data: []byte("not audio at all")},
		"image with no bytes":  {Type: llm.JudgeMediaImage, MimeType: "image/png"},
	} {
		if _, err := geminiDecisionMedia(2, p); !errors.Is(err, llm.ErrInvalidMedia) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A URL's type comes from its extension; without one Gemini yields to the next judge.
	b, err = geminiDecisionMedia(0, &llm.JudgeMediaPart{Type: llm.JudgeMediaImage, URL: "https://cdn.test/a/x.png?v=2"})
	if err != nil || b.MimeType != "image/png" {
		t.Errorf("url with extension: %+v %v", b, err)
	}
	if _, err := geminiDecisionMedia(0, &llm.JudgeMediaPart{Type: llm.JudgeMediaImage, URL: "https://cdn.test/x"}); !errors.Is(err, llm.ErrJudgmentUnsupported) {
		t.Errorf("url without a type: %v", err)
	}
	a := NewGeminiDecisionsAdapter("http://unused", "k", "gemini-decision-1")
	if err := a.CheckJudgeMedia([]*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, MimeType: "image/png"}}, nil); !errors.Is(err, llm.ErrInvalidMedia) {
		t.Errorf("CheckJudgeMedia: %v", err)
	}
}

func TestGeminiDecisionsMissingAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"answers":{"ok":{"type":"noul","noul":0.5}},"usage":{"total_input_tokens":5}}`)
	}))
	defer srv.Close()
	_, err := NewGeminiDecisionsAdapter(srv.URL, "k", "gemini-decision-1").Judge(context.Background(),
		&llm.JudgeRequest{StateJSON: `{}`, Questions: geminiQuestions()}, "u", nil)
	if err == nil || !strings.Contains(err.Error(), "no answer for question") {
		t.Fatalf("err = %v", err)
	}
}
