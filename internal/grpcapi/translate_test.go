package grpcapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

// fakeTranslator is an OpenAI-compatible chat server that translates the
// prompt's items by prefixing them, or misbehaves as told.
type fakeTranslator struct {
	mu       sync.Mutex
	calls    int
	status   int    // non-zero: answer every call with this HTTP status
	mode     string // "", "short", "reversed", "dupe"
	prompts  []string
	failOnce bool // mismatch on the first call only
}

func (f *fakeTranslator) handler(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.prompts = append(f.prompts, string(raw))
	f.mu.Unlock()
	if f.status != 0 {
		http.Error(w, `{"error":{"message":"boom"}}`, f.status)
		return
	}
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(raw, &req)
	var payload promptPayloadForTest
	for _, m := range req.Messages {
		if m.Role == "user" {
			_ = json.Unmarshal([]byte(contentText(m.Content)), &payload)
		}
	}
	type item struct {
		ID   int    `json:"id"`
		Lang string `json:"source_language"`
		Text string `json:"text"`
	}
	var items []item
	for _, it := range payload.Items {
		items = append(items, item{ID: it.ID, Lang: "en", Text: strings.ToUpper(payload.TargetLanguage) + ":" + it.Text})
	}
	mode := f.mode
	if f.failOnce && call > 1 {
		mode = ""
	}
	switch mode {
	case "short":
		items = items[:len(items)-1]
	case "reversed":
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
	case "dupe":
		items[len(items)-1].ID = 0
	}
	content, _ := json.Marshal(map[string]any{"translations": items})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "c1", "object": "chat.completion", "model": "gemma",
		"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": string(content)}}},
		"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 40, "total_tokens": 140},
	})
}

type promptPayloadForTest struct {
	TargetLanguage string `json:"target_language"`
	Items          []struct {
		ID   int    `json:"id"`
		Text string `json:"text"`
	} `json:"items"`
}

func contentText(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, p := range v {
			if m, ok := p.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

func (f *fakeTranslator) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func translateModel(id, endpoint string, source, target []string) *proxydb.Model {
	if source == nil {
		source = []string{"*"}
	}
	if target == nil {
		target = []string{"*"}
	}
	return &proxydb.Model{
		ID: id, Vendor: registry.VendorOpenAI, Endpoint: endpoint,
		Efforts: []string{registry.EffortLow}, Capabilities: []string{registry.CapabilityTranslate},
		LanguagesSource: source, LanguagesTarget: target, Enabled: true,
		PriceInPerMtok: 1, PriceOutPerMtok: 2, PricePerMillionChars: 20,
	}
}

func translateRule(use ...string) *proxydb.Rule {
	return &proxydb.Rule{
		Name: "translate-default", Priority: 170, Effort: registry.EffortLow,
		Attributes: map[string]string{"modality": "translate"}, Enabled: true, Use: use,
	}
}

func newTranslateServer(t *testing.T, models []*proxydb.Model, rules []*proxydb.Rule, tracker *fakeTracker) *ProxyServer {
	t.Helper()
	rules = append(rules, &proxydb.Rule{Name: "low-default", Priority: 10, Effort: registry.EffortLow, Enabled: true, Use: []string{models[0].ID}})
	return newJudgeServer(t, models, rules, tracker)
}

func fakeTranslatorServer(t *testing.T, f *fakeTranslator) string {
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestTranslateByPromptKeepsOrderAndBills(t *testing.T) {
	f := &fakeTranslator{mode: "reversed"}
	tracker := &fakeTracker{}
	s := newTranslateServer(t, []*proxydb.Model{translateModel("gemma", fakeTranslatorServer(t, f), nil, nil)},
		[]*proxydb.Rule{translateRule("gemma")}, tracker)

	resp, err := s.Translate(testIdentity(), &pb.TranslateRequest{
		Texts: []string{"good", "bad", "über"}, TargetLanguage: "de",
		Attributes: map[string]string{"user_id": "u1"},
	})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	want := []string{"DE:good", "DE:bad", "DE:über"}
	for i, tr := range resp.GetTranslations() {
		if tr.GetText() != want[i] || tr.GetDetectedSourceLanguage() != "en" {
			t.Errorf("translation %d = %q (%s), want %q (en)", i, tr.GetText(), tr.GetDetectedSourceLanguage(), want[i])
		}
	}
	if resp.GetResolvedModel() != "gemma" || resp.GetMatchedRule() != "translate-default" {
		t.Errorf("served by %s under %s", resp.GetResolvedModel(), resp.GetMatchedRule())
	}
	if got := resp.GetUsage().GetCharacters(); got != 11 {
		t.Errorf("characters = %d, want 11", got)
	}
	ev := tracker.list()
	if len(ev) != 1 {
		t.Fatalf("%d usage events, want 1", len(ev))
	}
	// 100 in at $1, 40 out at $2, 11 chars at $20 per million: 100k+80k+220k nano-USD.
	if ev[0].actionID != "translate" || ev[0].meta["characters"] != "11" || ev[0].cost != 400_000 {
		t.Errorf("usage event = %+v", ev[0])
	}
}

func TestTranslateMismatchRetriesThenFallsBack(t *testing.T) {
	bad := &fakeTranslator{mode: "short"}
	good := &fakeTranslator{}
	tracker := &fakeTracker{}
	s := newTranslateServer(t, []*proxydb.Model{
		translateModel("flaky", fakeTranslatorServer(t, bad), nil, nil),
		translateModel("steady", fakeTranslatorServer(t, good), nil, nil),
	}, []*proxydb.Rule{translateRule("flaky", "steady")}, tracker)

	resp, err := s.Translate(testIdentity(), &pb.TranslateRequest{Texts: []string{"a", "b"}, TargetLanguage: "de"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if resp.GetResolvedModel() != "steady" {
		t.Errorf("resolved %s, want the fallback", resp.GetResolvedModel())
	}
	if bad.count() != 2 {
		t.Errorf("mismatching model called %d times, want 2 (one retry)", bad.count())
	}
	// The failed model bills its tokens only, the serving one the characters.
	ev := tracker.list()
	if len(ev) != 2 || ev[0].meta["characters"] != "0" || ev[1].meta["characters"] != "2" || ev[0].cost != 360_000 {
		t.Errorf("usage events = %+v", ev)
	}
}

func TestTranslateMismatchRecoversOnRetry(t *testing.T) {
	f := &fakeTranslator{mode: "dupe", failOnce: true}
	s := newTranslateServer(t, []*proxydb.Model{translateModel("gemma", fakeTranslatorServer(t, f), nil, nil)},
		[]*proxydb.Rule{translateRule("gemma")}, nil)
	if _, err := s.Translate(testIdentity(), &pb.TranslateRequest{Texts: []string{"a", "b"}, TargetLanguage: "de"}); err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if f.count() != 2 {
		t.Errorf("calls = %d, want 2", f.count())
	}
}

func TestTranslateLanguagePairFilter(t *testing.T) {
	opus := &fakeTranslator{}
	llm := &fakeTranslator{}
	s := newTranslateServer(t, []*proxydb.Model{
		translateModel("opus-en-de", fakeTranslatorServer(t, opus), []string{"en"}, []string{"de"}),
		translateModel("gemma", fakeTranslatorServer(t, llm), []string{"*"}, []string{"de", "fr"}),
	}, []*proxydb.Rule{translateRule("opus-en-de", "gemma")}, nil)

	cases := []struct {
		from, to, want string
		code           codes.Code
		reason         string
	}{
		{from: "en", to: "de-DE", want: "opus-en-de"},
		{from: "", to: "de", want: "opus-en-de"},
		{from: "it", to: "de", want: "gemma"},
		{from: "en", to: "fr", want: "gemma"},
		{from: "en", to: "ja", code: codes.FailedPrecondition, reason: "unsupported_language_pair"},
	}
	for _, c := range cases {
		resp, err := s.Translate(testIdentity(), &pb.TranslateRequest{Texts: []string{"x"}, SourceLanguage: c.from, TargetLanguage: c.to})
		if c.reason != "" {
			if st, _ := status.FromError(err); st.Code() != c.code || !strings.HasPrefix(st.Message(), c.reason) {
				t.Errorf("%s->%s: err = %v, want %s", c.from, c.to, err, c.reason)
			}
			continue
		}
		if err != nil || resp.GetResolvedModel() != c.want {
			t.Errorf("%s->%s: model %s err %v, want %s", c.from, c.to, resp.GetResolvedModel(), err, c.want)
		}
	}
}

func TestTranslateFallsBackOnVendorError(t *testing.T) {
	down := &fakeTranslator{status: http.StatusServiceUnavailable}
	up := &fakeTranslator{}
	s := newTranslateServer(t, []*proxydb.Model{
		translateModel("local", fakeTranslatorServer(t, down), nil, nil),
		translateModel("cloud", fakeTranslatorServer(t, up), nil, nil),
	}, []*proxydb.Rule{translateRule("local", "cloud")}, nil)
	resp, err := s.Translate(testIdentity(), &pb.TranslateRequest{Texts: []string{"x"}, TargetLanguage: "de"})
	if err != nil || resp.GetResolvedModel() != "cloud" {
		t.Fatalf("model %s err %v, want cloud", resp.GetResolvedModel(), err)
	}
}

func TestTranslateNeedsCapabilityAndRule(t *testing.T) {
	f := &fakeTranslator{}
	chat := translateModel("chat", fakeTranslatorServer(t, f), nil, nil)
	chat.Capabilities = nil
	s := newTranslateServer(t, []*proxydb.Model{chat}, nil, nil)
	_, err := s.Translate(testIdentity(), &pb.TranslateRequest{Texts: []string{"x"}, TargetLanguage: "de"})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || !strings.Contains(st.Message(), "no translate rule") {
		t.Errorf("without a translate rule: %v", err)
	}
	s = newTranslateServer(t, []*proxydb.Model{chat}, []*proxydb.Rule{translateRule("chat")}, nil)
	_, err = s.Translate(testIdentity(), &pb.TranslateRequest{Texts: []string{"x"}, TargetLanguage: "de"})
	if st, _ := status.FromError(err); st.Code() != codes.FailedPrecondition || st.Message() != "no_capable_model" {
		t.Errorf("chain without the capability: %v", err)
	}
	if f.count() != 0 {
		t.Errorf("a model without translate was called")
	}
}

func TestTranslateValidation(t *testing.T) {
	s := newTranslateServer(t, []*proxydb.Model{translateModel("gemma", "http://127.0.0.1:1", nil, nil)},
		[]*proxydb.Rule{translateRule("gemma")}, nil)
	big := strings.Repeat("x", maxTranslateBytes)
	for name, req := range map[string]*pb.TranslateRequest{
		"no texts":    {TargetLanguage: "de"},
		"no target":   {Texts: []string{"x"}},
		"bad target":  {Texts: []string{"x"}, TargetLanguage: "german"},
		"star source": {Texts: []string{"x"}, SourceLanguage: "*", TargetLanguage: "de"},
		"bad format":  {Texts: []string{"x"}, TargetLanguage: "de", Format: "markdown"},
		"too many":    {Texts: make([]string, maxTranslateTexts+1), TargetLanguage: "de"},
		"too big":     {Texts: []string{big, "y"}, TargetLanguage: "de"},
	} {
		_, err := s.Translate(testIdentity(), req)
		if st, _ := status.FromError(err); st.Code() != codes.InvalidArgument || !strings.HasPrefix(st.Message(), "bad_request") {
			t.Errorf("%s: %v, want bad_request", name, err)
		}
	}
}

func TestTranslateSkipsBlankAndSameLanguage(t *testing.T) {
	f := &fakeTranslator{}
	s := newTranslateServer(t, []*proxydb.Model{translateModel("gemma", fakeTranslatorServer(t, f), nil, nil)},
		[]*proxydb.Rule{translateRule("gemma")}, nil)
	resp, err := s.Translate(testIdentity(), &pb.TranslateRequest{Texts: []string{"", "nice", "  "}, TargetLanguage: "de"})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{resp.Translations[0].GetText(), resp.Translations[1].GetText(), resp.Translations[2].GetText()}
	if strings.Join(got, "|") != "|DE:nice|  " {
		t.Errorf("translations = %q", got)
	}
	if !strings.Contains(f.prompts[0], `\"items\":[{\"id\":0,\"text\":\"nice\"}]`) {
		t.Errorf("blank texts reached the model: %s", f.prompts[0])
	}
	resp, err = s.Translate(testIdentity(), &pb.TranslateRequest{Texts: []string{"gut"}, SourceLanguage: "de", TargetLanguage: "DE"})
	if err != nil || resp.Translations[0].GetText() != "gut" || f.count() != 1 {
		t.Errorf("same language: %v %v, %d calls", resp, err, f.count())
	}
}

func TestTranslateHTMLPrompt(t *testing.T) {
	f := &fakeTranslator{}
	s := newTranslateServer(t, []*proxydb.Model{translateModel("gemma", fakeTranslatorServer(t, f), nil, nil)},
		[]*proxydb.Rule{translateRule("gemma")}, nil)
	if _, err := s.Translate(testIdentity(), &pb.TranslateRequest{Texts: []string{"<b>hi</b>"}, TargetLanguage: "de", Format: "HTML"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.prompts[0], "keep every tag") {
		t.Error("html format did not reach the prompt")
	}
}

func TestExplainRouteTranslate(t *testing.T) {
	s := newTranslateServer(t, []*proxydb.Model{
		translateModel("opus-en-de", "http://127.0.0.1:1", []string{"en"}, []string{"de"}),
		translateModel("gemma", "http://127.0.0.1:1", nil, nil),
	}, []*proxydb.Rule{translateRule("opus-en-de", "gemma")}, nil)
	out := s.ExplainRoute(context.Background(), apikeys.IdentityFrom(testIdentity()), &pb.ExplainRouteRequest{Modality: "translate", SourceLanguage: "fr", TargetLanguage: "de"})
	if out.GetError() != "" || out.GetRule() != "translate-default" {
		t.Fatalf("explain = %+v", out)
	}
	bad := s.ExplainRoute(context.Background(), apikeys.IdentityFrom(testIdentity()), &pb.ExplainRouteRequest{Modality: "translate", TargetLanguage: "*"})
	if !strings.HasPrefix(bad.GetError(), "INVALID_ARGUMENT: bad_request") {
		t.Errorf("explain with target * = %q", bad.GetError())
	}
	if len(out.GetCandidates()) != 2 || out.GetCandidates()[0].GetDropped() != dropLanguagePair || out.GetCandidates()[1].GetDropped() != "" {
		t.Errorf("candidates = %v", out.GetCandidates())
	}
	if strings.Join(out.GetModels(), ",") != "gemma" {
		t.Errorf("models = %v", out.GetModels())
	}
}
