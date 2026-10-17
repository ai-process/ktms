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

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/guardrails"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
)

// fakeJudge is an httptest server speaking TypeSafe's /v1/systemone shape.
type fakeJudge struct {
	mu         sync.Mutex
	calls      int
	bodies     []string
	failFirstN int // respond failStatus to the first N calls
	failStatus int
	answers    map[string]any
}

func (f *fakeJudge) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.bodies = append(f.bodies, string(body))
	f.mu.Unlock()

	if call <= f.failFirstN {
		http.Error(w, `{"error":"boom"}`, f.failStatus)
		return
	}
	answers := f.answers
	if answers == nil {
		answers = map[string]any{
			"is_answer": map[string]any{"type": "noul", "noul": 0.93},
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model":   "jev-1.13.0",
		"answers": answers,
		"usage":   map[string]any{"input_tokens": 312, "output_tokens": 0},
	})
}

func (f *fakeJudge) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeJudge) lastBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return ""
	}
	return f.bodies[len(f.bodies)-1]
}

func judgeModel(id, endpoint string) *proxydb.Model {
	return &proxydb.Model{
		ID: id, Vendor: registry.VendorTypeSafe, Endpoint: endpoint,
		Efforts: []string{registry.EffortLow}, Capabilities: []string{registry.CapabilityJudge},
		Enabled: true,
	}
}

func judgeRule(use ...string) *proxydb.Rule {
	return &proxydb.Rule{
		Name: "judge-default", Priority: 160, Effort: registry.EffortLow,
		Attributes: map[string]string{"modality": "judge"}, Enabled: true, Use: use,
	}
}

// newJudgeServer wires a client key holding a typesafe credential, plus the low
// catch-all a text rule would occupy — judge must not be captured by it.
func newJudgeServer(t *testing.T, models []*proxydb.Model, rules []*proxydb.Rule, tracker *fakeTracker) *ProxyServer {
	t.Helper()
	keys := map[string]map[string]string{
		"key1": {registry.VendorTypeSafe: "ts-test", registry.VendorOpenAI: "sk-test", registry.VendorDeepSeek: "ds-test"},
	}
	if tracker != nil {
		return NewProxyServer(fixedSnapshot{registry.NewSnapshotForTest(models, rules, keys, tracker)}, nil)
	}
	return NewProxyServer(fixedSnapshot{registry.NewSnapshotForTest(models, rules, keys, nil)}, nil)
}

func nounQuestion() map[string]*pb.JudgeQuestion {
	return map[string]*pb.JudgeQuestion{
		"is_answer": {
			Type:          pb.JudgeType_JUDGE_TYPE_NOUL,
			Instructions:  "The student is attempting to answer the question",
			TrueCriteria:  "any attempt, even wrong",
			FalseCriteria: "asks for a hint, gives up, unrelated chat",
		},
	}
}

func TestJudgeHappyPath(t *testing.T) {
	vendor := &fakeJudge{}
	srv := httptest.NewServer(http.HandlerFunc(vendor.handler))
	defer srv.Close()

	tracker := &fakeTracker{}
	s := newJudgeServer(t,
		[]*proxydb.Model{judgeModel("jev-latest", srv.URL)},
		[]*proxydb.Rule{judgeRule("jev-latest")}, tracker)

	resp, err := s.Judge(testIdentity(), &pb.JudgeRequest{
		StateJson:  `{"student_message":"der Hund"}`,
		Questions:  nounQuestion(),
		Attributes: map[string]string{"user_id": "stud-1"},
		ActionId:   "app.judge.classifier",
	})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	answer := resp.GetAnswers()["is_answer"]
	if answer == nil {
		t.Fatal("no answer for the asked question")
	}
	if answer.GetNoul() != 0.93 {
		t.Errorf("noul = %v, want 0.93", answer.GetNoul())
	}
	if answer.GetType() != pb.JudgeType_JUDGE_TYPE_NOUL {
		t.Errorf("type = %v, want noul", answer.GetType())
	}
	if resp.GetResolvedVendor() != registry.VendorTypeSafe || resp.GetResolvedModel() != "jev-latest" {
		t.Errorf("resolved %s/%s, want typesafe/jev-latest", resp.GetResolvedVendor(), resp.GetResolvedModel())
	}
	if resp.GetMatchedRule() != "judge-default" {
		t.Errorf("matched rule %q, want judge-default", resp.GetMatchedRule())
	}
	if resp.GetUsage().GetInput() != 312 {
		t.Errorf("input tokens = %d, want 312", resp.GetUsage().GetInput())
	}

	// The vendor gets the state verbatim and the criteria pair as one map.
	var sent struct {
		State     map[string]string `json:"state"`
		Model     string            `json:"model"`
		Questions map[string]struct {
			Type     string            `json:"type"`
			Criteria map[string]string `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal([]byte(vendor.lastBody()), &sent); err != nil {
		t.Fatalf("vendor body: %v", err)
	}
	if sent.State["student_message"] != "der Hund" {
		t.Errorf("state reshaped: %s", vendor.lastBody())
	}
	if sent.Model != "jev-latest" {
		t.Errorf("model = %q, want the registry id", sent.Model)
	}
	if sent.Questions["is_answer"].Criteria["true"] != "any attempt, even wrong" {
		t.Errorf("criteria not sent: %s", vendor.lastBody())
	}

	events := tracker.list()
	if len(events) != 1 {
		t.Fatalf("recorded %d usage events, want 1", len(events))
	}
	if events[0].actionID != "app.judge.classifier" {
		t.Errorf("action id = %q, want the caller's", events[0].actionID)
	}
	if events[0].vendor != registry.VendorTypeSafe {
		t.Errorf("vendor = %q, want typesafe", events[0].vendor)
	}
	if events[0].meta["questions"] != "1" {
		t.Errorf("meta questions = %q, want 1", events[0].meta["questions"])
	}
}

// 529 is the vendor's "overloaded": retryable, so the chain gets to the next model.
func TestJudgeAdvancesChainOnOverload(t *testing.T) {
	down := &fakeJudge{failFirstN: 99, failStatus: 529}
	downSrv := httptest.NewServer(http.HandlerFunc(down.handler))
	defer downSrv.Close()
	up := &fakeJudge{}
	upSrv := httptest.NewServer(http.HandlerFunc(up.handler))
	defer upSrv.Close()

	s := newJudgeServer(t,
		[]*proxydb.Model{judgeModel("jev-a", downSrv.URL), judgeModel("jev-b", upSrv.URL)},
		[]*proxydb.Rule{judgeRule("jev-a", "jev-b")}, nil)

	resp, err := s.Judge(testIdentity(), &pb.JudgeRequest{
		StateJson: `"hello"`, Questions: nounQuestion(),
	})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if resp.GetResolvedModel() != "jev-b" {
		t.Errorf("served by %q, want the second model", resp.GetResolvedModel())
	}
	if up.count() != 1 {
		t.Errorf("second model called %d times, want 1", up.count())
	}
}

// 422 is the vendor rejecting the request itself: terminal, and it would fail
// identically on every other model.
func TestJudgeTerminalOnValidationError(t *testing.T) {
	vendor := &fakeJudge{failFirstN: 99, failStatus: 422}
	srv := httptest.NewServer(http.HandlerFunc(vendor.handler))
	defer srv.Close()

	s := newJudgeServer(t,
		[]*proxydb.Model{judgeModel("jev-a", srv.URL), judgeModel("jev-b", srv.URL)},
		[]*proxydb.Rule{judgeRule("jev-a", "jev-b")}, nil)

	_, err := s.Judge(testIdentity(), &pb.JudgeRequest{
		StateJson: `"hello"`, Questions: nounQuestion(),
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal vendor_error", status.Code(err))
	}
	if vendor.count() != 1 {
		t.Errorf("vendor called %d times, want 1 — terminal must not walk the chain", vendor.count())
	}
}

// An answer the caller never asked about is fine; a missing one is not.
func TestJudgeMissingAnswerIsAnError(t *testing.T) {
	vendor := &fakeJudge{answers: map[string]any{"other": map[string]any{"type": "noul", "noul": 0.5}}}
	srv := httptest.NewServer(http.HandlerFunc(vendor.handler))
	defer srv.Close()

	s := newJudgeServer(t,
		[]*proxydb.Model{judgeModel("jev-latest", srv.URL)},
		[]*proxydb.Rule{judgeRule("jev-latest")}, nil)

	_, err := s.Judge(testIdentity(), &pb.JudgeRequest{
		StateJson: `"hello"`, Questions: nounQuestion(),
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
}

// A text rule must never capture judgment traffic, even when its predicates hold.
func TestJudgeIgnoresTextRules(t *testing.T) {
	s := newJudgeServer(t,
		[]*proxydb.Model{testModel("gpt-5-nano", registry.VendorOpenAI, "", registry.EffortLow)},
		catchAllRules("gpt-5-nano"), nil)

	_, err := s.Judge(testIdentity(), &pb.JudgeRequest{
		StateJson: `"hello"`, Questions: nounQuestion(),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
	if got := status.Convert(err).Message(); got != "no_capable_model: no judge rule configured" {
		t.Errorf("message = %q", got)
	}
}

// The judge rule matches, but this client holds no credential for the vendor.
func TestJudgeNoVendorKey(t *testing.T) {
	snap := registry.NewSnapshotForTest(
		[]*proxydb.Model{judgeModel("jev-latest", "")},
		[]*proxydb.Rule{judgeRule("jev-latest")},
		map[string]map[string]string{"key1": {registry.VendorOpenAI: "sk-test"}}, nil)
	s := NewProxyServer(fixedSnapshot{snap}, nil)

	_, err := s.Judge(testIdentity(), &pb.JudgeRequest{
		StateJson: `"hello"`, Questions: nounQuestion(),
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
	if got := status.Convert(err).Message(); got != "no_vendor_key" {
		t.Errorf("message = %q, want no_vendor_key", got)
	}
}

func TestJudgeValidation(t *testing.T) {
	s := newJudgeServer(t,
		[]*proxydb.Model{judgeModel("jev-latest", "")},
		[]*proxydb.Rule{judgeRule("jev-latest")}, nil)

	cases := map[string]*pb.JudgeRequest{
		"no state":        {Questions: nounQuestion()},
		"state not json":  {StateJson: `{"broken"`, Questions: nounQuestion()},
		"no questions":    {StateJson: `"hi"`},
		"no instructions": {StateJson: `"hi"`, Questions: map[string]*pb.JudgeQuestion{"q": {Type: pb.JudgeType_JUDGE_TYPE_NOUL}}},
		"no type":         {StateJson: `"hi"`, Questions: map[string]*pb.JudgeQuestion{"q": {Instructions: "is it?"}}},
		"choice one option": {StateJson: `"hi"`, Questions: map[string]*pb.JudgeQuestion{"q": {
			Type: pb.JudgeType_JUDGE_TYPE_CHOICE, Instructions: "which?",
			Options: map[string]string{"only": "the only one"}}}},
		"score one level": {StateJson: `"hi"`, Questions: map[string]*pb.JudgeQuestion{"q": {
			Type: pb.JudgeType_JUDGE_TYPE_SCORE, Instructions: "how much?", Levels: []string{"low"}}}},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Judge(testIdentity(), req); status.Code(err) != codes.InvalidArgument {
				t.Errorf("code = %v, want InvalidArgument", status.Code(err))
			}
		})
	}
}

func TestJudgeUnauthenticated(t *testing.T) {
	s := newJudgeServer(t,
		[]*proxydb.Model{judgeModel("jev-latest", "")},
		[]*proxydb.Rule{judgeRule("jev-latest")}, nil)

	_, err := s.Judge(context.Background(), &pb.JudgeRequest{
		StateJson: `"hi"`, Questions: nounQuestion(),
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", status.Code(err))
	}
}

// A choice answer keeps its distribution and confidence, which is what callers
// route on.
func TestJudgeChoiceAnswer(t *testing.T) {
	vendor := &fakeJudge{answers: map[string]any{
		"intent": map[string]any{
			"type": "choice", "choice": "grammar_help", "confidence": 0.82,
			"probabilities": map[string]float64{"grammar_help": 0.82, "answer": 0.18},
		},
	}}
	srv := httptest.NewServer(http.HandlerFunc(vendor.handler))
	defer srv.Close()

	s := newJudgeServer(t,
		[]*proxydb.Model{judgeModel("jev-latest", srv.URL)},
		[]*proxydb.Rule{judgeRule("jev-latest")}, nil)

	resp, err := s.Judge(testIdentity(), &pb.JudgeRequest{
		StateJson: `"erkläre mir den Dativ"`,
		Questions: map[string]*pb.JudgeQuestion{"intent": {
			Type:         pb.JudgeType_JUDGE_TYPE_CHOICE,
			Instructions: "What does the student want?",
			Options:      map[string]string{"grammar_help": "asks for a rule", "answer": "answers the question"},
		}},
	})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	got := resp.GetAnswers()["intent"]
	if got.GetChoice() != "grammar_help" || got.GetConfidence() != 0.82 {
		t.Errorf("choice %q confidence %v", got.GetChoice(), got.GetConfidence())
	}
	if got.GetProbabilities()["answer"] != 0.18 {
		t.Errorf("probabilities lost: %v", got.GetProbabilities())
	}

	// Choice criteria travel as the option map, not a true/false pair.
	var sent struct {
		Questions map[string]struct {
			Criteria map[string]string `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal([]byte(vendor.lastBody()), &sent); err != nil {
		t.Fatalf("vendor body: %v", err)
	}
	if sent.Questions["intent"].Criteria["grammar_help"] != "asks for a rule" {
		t.Errorf("options not sent as criteria: %s", vendor.lastBody())
	}
}

// The estimate reserves the whole billed input: state plus every question's
// text, not just the state.
func TestJudgeEstimateCountsQuestions(t *testing.T) {
	state := `{"a":"` + strings.Repeat("x", 400) + `"}`
	stateOnly := judgeEstimate(&llm.JudgeRequest{StateJSON: state})
	withQuestions := judgeEstimate(&llm.JudgeRequest{
		StateJSON: state,
		Questions: map[string]*llm.JudgeQuestion{"q": {
			Type:         llm.JudgeNoul,
			Instructions: strings.Repeat("y", 200),
			TrueCriteria: strings.Repeat("z", 200),
		}},
	})
	if withQuestions <= stateOnly {
		t.Errorf("estimate %d did not grow with the questions (state alone %d)", withQuestions, stateOnly)
	}
}

func openAIJudgeModel(id, endpoint string) *proxydb.Model {
	return &proxydb.Model{
		ID: id, Vendor: registry.VendorOpenAI, Endpoint: endpoint,
		Efforts: []string{registry.EffortLow}, Capabilities: []string{registry.CapabilityJudge},
		Enabled: true,
	}
}

type fakeOpenAIDecisions struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeOpenAIDecisions) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()

	prob := 0.91
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model": "gpt-6-luna",
		"answers": []map[string]any{
			{"name": "is_answer", "type": "predicate", "probability": prob},
		},
		"usage": map[string]any{"input_tokens": 150, "output_tokens": 0},
	})
}

func TestJudgeFallsBackFromTypeSafeToOpenAI(t *testing.T) {
	tsFake := &fakeJudge{failFirstN: 1, failStatus: http.StatusInternalServerError}
	tsSrv := httptest.NewServer(http.HandlerFunc(tsFake.handler))
	defer tsSrv.Close()

	oaiFake := &fakeOpenAIDecisions{}
	oaiSrv := httptest.NewServer(http.HandlerFunc(oaiFake.handler))
	defer oaiSrv.Close()

	m1 := judgeModel("jev-latest", tsSrv.URL)
	m2 := openAIJudgeModel("gpt-6-luna", oaiSrv.URL)
	rule := judgeRule("jev-latest", "gpt-6-luna")

	s := newJudgeServer(t, []*proxydb.Model{m1, m2}, []*proxydb.Rule{rule}, nil)

	resp, err := s.Judge(testIdentity(), &pb.JudgeRequest{
		StateJson: `{"ctx":"hello"}`,
		Questions: map[string]*pb.JudgeQuestion{
			"is_answer": {Type: pb.JudgeType_JUDGE_TYPE_NOUL, Instructions: "is it an answer?"},
		},
	})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}

	if resp.GetResolvedVendor() != registry.VendorOpenAI || resp.GetResolvedModel() != "gpt-6-luna" {
		t.Errorf("resolved %s/%s, want openai/gpt-6-luna", resp.GetResolvedVendor(), resp.GetResolvedModel())
	}
	if tsFake.count() != 1 {
		t.Errorf("typesafe calls = %d, want 1", tsFake.count())
	}
	if oaiFake.calls != 1 {
		t.Errorf("openai calls = %d, want 1", oaiFake.calls)
	}
	if ans := resp.Answers["is_answer"]; ans.GetNoul() != 0.91 {
		t.Errorf("answer = %+v, want noul 0.91", ans)
	}
}

func TestJudgeFallsBackFromUnsupportedJudgeModel(t *testing.T) {
	oaiFake := &fakeOpenAIDecisions{}
	oaiSrv := httptest.NewServer(http.HandlerFunc(oaiFake.handler))
	defer oaiSrv.Close()

	// deepseek model has judge capability in DB, but its adapter cannot judge.
	m1 := &proxydb.Model{
		ID: "deepseek-chat", Vendor: registry.VendorDeepSeek, Endpoint: "http://example.com",
		Efforts: []string{registry.EffortLow}, Capabilities: []string{registry.CapabilityJudge},
		Enabled: true,
	}
	m2 := openAIJudgeModel("gpt-6-luna", oaiSrv.URL)
	rule := judgeRule("deepseek-chat", "gpt-6-luna")

	s := newJudgeServer(t, []*proxydb.Model{m1, m2}, []*proxydb.Rule{rule}, nil)

	resp, err := s.Judge(testIdentity(), &pb.JudgeRequest{
		StateJson: `{"ctx":"hello"}`,
		Questions: map[string]*pb.JudgeQuestion{
			"is_answer": {Type: pb.JudgeType_JUDGE_TYPE_NOUL, Instructions: "is it an answer?"},
		},
	})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}

	if resp.GetResolvedVendor() != registry.VendorOpenAI || resp.GetResolvedModel() != "gpt-6-luna" {
		t.Errorf("resolved %s/%s, want openai/gpt-6-luna", resp.GetResolvedVendor(), resp.GetResolvedModel())
	}
	if oaiFake.calls != 1 {
		t.Errorf("openai calls = %d, want 1", oaiFake.calls)
	}

	// Chain containing only the unsupported model returns no_capable_model.
	sOnlyUnsupported := newJudgeServer(t, []*proxydb.Model{m1}, []*proxydb.Rule{judgeRule("deepseek-chat")}, nil)
	_, errOnly := sOnlyUnsupported.Judge(testIdentity(), &pb.JudgeRequest{
		StateJson: `{"ctx":"hello"}`,
		Questions: map[string]*pb.JudgeQuestion{
			"is_answer": {Type: pb.JudgeType_JUDGE_TYPE_NOUL, Instructions: "is it an answer?"},
		},
	})
	if status.Code(errOnly) != codes.FailedPrecondition || !strings.Contains(errOnly.Error(), "no_capable_model") {
		t.Errorf("errOnly = %v, want FailedPrecondition no_capable_model", errOnly)
	}
}

func TestJudgeGuardrailsBlock(t *testing.T) {
	fake := &fakeJudge{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	model := judgeModel("typesafe-1", srv.URL)
	rule := judgeRule("typesafe-1")
	s := newJudgeServer(t, []*proxydb.Model{model}, []*proxydb.Rule{rule}, nil)
	s.WithGuardrails(guardrails.New(true, "block", "block"))

	_, err := s.Judge(testIdentity(), &pb.JudgeRequest{
		StateJson: `{"user_email":"leak@domain.com"}`,
		Questions: map[string]*pb.JudgeQuestion{
			"is_answer": {Type: pb.JudgeType_JUDGE_TYPE_NOUL, Instructions: "is user valid?"},
		},
	})
	if err == nil {
		t.Fatal("expected guardrail error, got nil")
	}
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "guardrail_violation") {
		t.Errorf("expected InvalidArgument with guardrail_violation, got %v", err)
	}
	if fake.count() != 0 {
		t.Errorf("vendor should not be called, got %d calls", fake.count())
	}
}

func TestJudgeGuardrailsRedact(t *testing.T) {
	fake := &fakeJudge{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	model := judgeModel("typesafe-1", srv.URL)
	rule := judgeRule("typesafe-1")
	s := newJudgeServer(t, []*proxydb.Model{model}, []*proxydb.Rule{rule}, nil)
	s.WithGuardrails(guardrails.New(true, "redact", "block"))

	resp, err := s.Judge(testIdentity(), &pb.JudgeRequest{
		StateJson: `{"user_email":"leak@domain.com"}`,
		Questions: map[string]*pb.JudgeQuestion{
			"is_answer": {Type: pb.JudgeType_JUDGE_TYPE_NOUL, Instructions: "is user valid?"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if fake.count() != 1 {
		t.Fatalf("vendor calls = %d, want 1", fake.count())
	}
	body := fake.bodies[0]
	if strings.Contains(body, "leak@domain.com") {
		t.Errorf("vendor received unredacted PII in state: %s", body)
	}
	if !strings.Contains(body, "[EMAIL_1]") {
		t.Errorf("vendor expected [EMAIL_1] in state: %s", body)
	}
}
