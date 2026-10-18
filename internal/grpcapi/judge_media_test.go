package grpcapi

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/guardrails"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

// recordingDecisions is an OpenAI /v1/decisions fake that keeps request bodies.
type recordingDecisions struct {
	mu     sync.Mutex
	bodies []string
}

func (f *recordingDecisions) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, string(body))
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model":   "gpt-6-luna",
		"answers": []map[string]any{{"name": "is_answer", "type": "predicate", "probability": 0.8}},
		"usage":   map[string]any{"input_tokens": 1200, "output_tokens": 0},
	})
}

func (f *recordingDecisions) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

func multimodalJudgeModel(id, endpoint string, caps ...string) *proxydb.Model {
	m := openAIJudgeModel(id, endpoint)
	m.Capabilities = append([]string{registry.CapabilityJudge}, caps...)
	return m
}

func imagePart(mime string, data []byte, url string) *pb.MediaPart {
	return &pb.MediaPart{Part: &pb.MediaPart_Image{Image: &pb.ImagePart{MimeType: mime, Data: data, Url: url}}}
}

func embeddingPart(values ...float32) *pb.MediaPart {
	return &pb.MediaPart{Part: &pb.MediaPart_Embedding{Embedding: &pb.EmbeddingPart{Values: values, Model: "v-jepa-2"}}}
}

func textPart(text string) *pb.MediaPart {
	return &pb.MediaPart{Part: &pb.MediaPart_Text{Text: &pb.TextPart{Text: text}}}
}

func TestJudgeMediaValidation(t *testing.T) {
	s := newJudgeServer(t,
		[]*proxydb.Model{multimodalJudgeModel("gpt-6-luna", "", registry.CapabilityVision)},
		[]*proxydb.Rule{judgeRule("gpt-6-luna")}, nil)

	tooMany := make([]*pb.MediaPart, maxJudgeMediaParts+1)
	for i := range tooMany {
		tooMany[i] = textPart("x")
	}
	cases := map[string][]*pb.MediaPart{
		"empty part":          {{}},
		"empty text":          {textPart("")},
		"image no source":     {imagePart("", nil, "")},
		"image http url":      {imagePart("", nil, "http://example.com/a.png")},
		"image bad mime":      {imagePart("application/pdf", []byte("x"), "")},
		"image bad detail":    {{Part: &pb.MediaPart_Image{Image: &pb.ImagePart{Url: "https://x/a.png", Detail: "ultra"}}}},
		"audio no data":       {{Part: &pb.MediaPart_Audio{Audio: &pb.AudioPart{Format: "wav"}}}},
		"audio as image mime": {{Part: &pb.MediaPart_Audio{Audio: &pb.AudioPart{Data: []byte("x"), MimeType: "image/png"}}}},
		"embedding empty":     {embeddingPart()},
		"embedding NaN":       {embeddingPart(1, float32(math.NaN()))},
		"embedding too large": {embeddingPart(make([]float32, maxJudgeEmbeddingDims+1)...)},
		"too many parts":      tooMany,
	}
	for name, media := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := s.Judge(testIdentity(), &pb.JudgeRequest{StateMedia: media, Questions: nounQuestion()})
			if status.Code(err) != codes.InvalidArgument {
				t.Errorf("code = %v (%v), want InvalidArgument", status.Code(err), err)
			}
		})
	}
}

// A judge without vision must never see an image request: answering without
// the image would be a judgment about a different state.
func TestJudgeMediaRoutesToCapableModel(t *testing.T) {
	ts := &fakeJudge{}
	tsSrv := httptest.NewServer(http.HandlerFunc(ts.handler))
	defer tsSrv.Close()
	oai := &recordingDecisions{}
	oaiSrv := httptest.NewServer(http.HandlerFunc(oai.handler))
	defer oaiSrv.Close()

	s := newJudgeServer(t,
		[]*proxydb.Model{judgeModel("jev-latest", tsSrv.URL), multimodalJudgeModel("gpt-6-luna", oaiSrv.URL, registry.CapabilityVision)},
		[]*proxydb.Rule{judgeRule("jev-latest", "gpt-6-luna")}, nil)

	resp, err := s.Judge(testIdentity(), &pb.JudgeRequest{
		StateJson:  `{"lane":"left"}`,
		StateMedia: []*pb.MediaPart{imagePart("image/png", []byte{0x89, 'P', 'N', 'G'}, "")},
		Questions:  nounQuestion(),
	})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if resp.GetResolvedModel() != "gpt-6-luna" {
		t.Errorf("resolved %q, want gpt-6-luna", resp.GetResolvedModel())
	}
	if ts.count() != 0 {
		t.Errorf("typesafe calls = %d, want 0", ts.count())
	}
	bodies := oai.calls()
	if len(bodies) != 1 {
		t.Fatalf("openai calls = %d, want 1", len(bodies))
	}
	var sent struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &sent); err != nil {
		t.Fatalf("decode vendor body: %v", err)
	}
	if len(sent.Input) != 2 || sent.Input[0]["type"] != "input_text" || sent.Input[0]["text"] != `{"lane":"left"}` {
		t.Fatalf("input = %v, want state JSON text then image", sent.Input)
	}
	if sent.Input[1]["type"] != "input_image" || !strings.HasPrefix(sent.Input[1]["image_url"].(string), "data:image/png;base64,") {
		t.Errorf("image part = %v", sent.Input[1])
	}

	// Embeddings need their own capability; a vision-only chain cannot serve them.
	_, err = s.Judge(testIdentity(), &pb.JudgeRequest{StateMedia: []*pb.MediaPart{embeddingPart(0.1, 0.2)}, Questions: nounQuestion()})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "no_capable_model") {
		t.Errorf("embedding on vision-only chain: %v, want FailedPrecondition no_capable_model", err)
	}
}

func TestJudgeMediaEmbeddingOnly(t *testing.T) {
	oai := &recordingDecisions{}
	srv := httptest.NewServer(http.HandlerFunc(oai.handler))
	defer srv.Close()
	s := newJudgeServer(t,
		[]*proxydb.Model{multimodalJudgeModel("jepa-judge", srv.URL, registry.CapabilityEmbeddingInput)},
		[]*proxydb.Rule{judgeRule("jepa-judge")}, nil)

	if _, err := s.Judge(testIdentity(), &pb.JudgeRequest{
		StateMedia: []*pb.MediaPart{embeddingPart(0.25, -0.5)},
		Questions:  nounQuestion(),
	}); err != nil {
		t.Fatalf("Judge: %v", err)
	}
	body := oai.calls()[0]
	if !strings.Contains(body, `"input":[{"type":"input_embedding","embedding":[0.25,-0.5],"model":"v-jepa-2"}]`) {
		t.Errorf("vendor body = %s", body)
	}
}

func TestJudgeMediaGuardrails(t *testing.T) {
	oai := &recordingDecisions{}
	srv := httptest.NewServer(http.HandlerFunc(oai.handler))
	defer srv.Close()
	newServer := func(pii string) *ProxyServer {
		s := newJudgeServer(t,
			[]*proxydb.Model{multimodalJudgeModel("gpt-6-luna", srv.URL)},
			[]*proxydb.Rule{judgeRule("gpt-6-luna")}, nil)
		s.WithGuardrails(guardrails.New(true, pii, "block"))
		return s
	}
	req := func() *pb.JudgeRequest {
		return &pb.JudgeRequest{StateMedia: []*pb.MediaPart{textPart("mail leak@domain.com")}, Questions: nounQuestion()}
	}

	_, err := newServer("block").Judge(testIdentity(), req())
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "guardrail_violation") {
		t.Errorf("block: %v, want InvalidArgument guardrail_violation", err)
	}
	if len(oai.calls()) != 0 {
		t.Fatalf("vendor called despite block")
	}

	if _, err := newServer("redact").Judge(testIdentity(), req()); err != nil {
		t.Fatalf("redact: %v", err)
	}
	body := oai.calls()[0]
	if strings.Contains(body, "leak@domain.com") || !strings.Contains(body, "[EMAIL_1]") {
		t.Errorf("vendor body not redacted: %s", body)
	}
}

func TestJudgeEstimateCountsMedia(t *testing.T) {
	base := &llm.JudgeRequest{StateJSON: `{}`}
	withMedia := &llm.JudgeRequest{StateJSON: `{}`, StateMedia: []*llm.JudgeMediaPart{
		{Type: llm.JudgeMediaImage},
		{Type: llm.JudgeMediaAudio},
		{Type: llm.JudgeMediaEmbedding, Embedding: make([]float32, 400)},
	}}
	if got, want := judgeEstimate(withMedia)-judgeEstimate(base), int64(1000+200+100); got < want {
		t.Errorf("media estimate delta = %d, want >= %d", got, want)
	}
}
