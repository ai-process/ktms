package adapters

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ai-process/llm-proxy/internal/llm"
)

func TestOpenAIDecisionErrorRetryability(t *testing.T) {
	cases := map[int]bool{
		429: true,
		500: true,
		502: true,
		503: true,
		401: false,
		422: false,
		400: false,
		404: false,
	}
	for code, want := range cases {
		err := &OpenAIDecisionError{Status: code, Body: "x"}
		if got := llm.IsRetryableError(err); got != want {
			t.Errorf("status %d retryable = %v, want %v", code, got, want)
		}
	}
}

func TestOpenAIDecisionErrorHidesUnauthorizedBody(t *testing.T) {
	err := &OpenAIDecisionError{Status: 401, Body: "invalid key sk-secret-token"}
	if strings.Contains(err.Error(), "sk-secret-token") {
		t.Errorf("401 error message leaks key body: %s", err.Error())
	}
}

func TestToOpenAIDecisionQuestions(t *testing.T) {
	qs, err := toOpenAIDecisionQuestions(map[string]*llm.JudgeQuestion{
		"noul_bare": {Type: llm.JudgeNoul, Instructions: "is it valid?"},
		"noul_crit": {Type: llm.JudgeNoul, Instructions: "is it valid?", TrueCriteria: "yes", FalseCriteria: "no"},
		"pick":      {Type: llm.JudgeChoice, Instructions: "which one?", Options: map[string]string{"b": "opt B", "a": "opt A"}},
		"rate":      {Type: llm.JudgeScore, Instructions: "rate severity", Levels: []string{"low", "medium", "high"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(qs) != 4 {
		t.Fatalf("got %d questions, want 4", len(qs))
	}

	byName := make(map[string]openAIDecisionQuestion)
	for _, q := range qs {
		byName[q.Name] = q
	}

	if q := byName["noul_bare"]; q.Type != "predicate" || q.Instructions != "is it valid?" {
		t.Errorf("noul_bare = %+v", q)
	}
	if q := byName["noul_crit"]; q.Type != "predicate" || !strings.Contains(q.Instructions, "True criteria: yes") || !strings.Contains(q.Instructions, "False criteria: no") {
		t.Errorf("noul_crit instructions = %q", q.Instructions)
	}

	pick := byName["pick"]
	if pick.Type != "choice" || len(pick.Choices) != 2 {
		t.Fatalf("pick = %+v", pick)
	}
	if pick.Choices[0].Value != "a" || pick.Choices[1].Value != "b" {
		t.Errorf("choices not sorted by key: %+v", pick.Choices)
	}

	rate := byName["rate"]
	if rate.Type != "score" || len(rate.Levels) != 3 {
		t.Fatalf("rate = %+v", rate)
	}
	if rate.Levels[0].Label != "0" || rate.Levels[0].Description != "low" {
		t.Errorf("level 0 = %+v", rate.Levels[0])
	}
}

func TestOpenAIJudgeSendsBearerAndPath(t *testing.T) {
	var gotAuth, gotPath string
	var reqBody openAIDecisionRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&reqBody)

		prob := 0.85
		confChoice := 0.92
		score := 1.75
		confScore := 0.88

		_ = json.NewEncoder(w).Encode(openAIDecisionResponse{
			Model: "gpt-6-luna-2026-09-01",
			Answers: []openAIDecisionAnswer{
				{
					Name:        "is_valid",
					Type:        "predicate",
					Probability: &prob,
				},
				{
					Name:          "department",
					Type:          "choice",
					Choice:        "billing",
					Probabilities: map[string]float64{"billing": 0.95, "tech": 0.05},
					Confidence:    &confChoice,
				},
				{
					Name:          "severity",
					Type:          "score",
					Score:         &score,
					Probabilities: map[string]float64{"0": 0.05, "1": 0.15, "2": 0.80},
					Confidence:    &confScore,
				},
			},
			Usage: struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
				TotalTokens  int `json:"total_tokens"`
			}{InputTokens: 45, OutputTokens: 0, TotalTokens: 45},
		})
	}))
	defer srv.Close()

	adapter := NewOpenAIAdapter(nil, "gpt-6-luna", nil)
	adapter.SetDecisionsConfig(srv.URL, "sk-test-key")

	res, err := adapter.Judge(context.Background(), &llm.JudgeRequest{
		StateJSON: `{"ticket_id": 1234, "text": "Charged twice"}`,
		Questions: map[string]*llm.JudgeQuestion{
			"is_valid":   {Type: llm.JudgeNoul, Instructions: "is it valid?"},
			"department": {Type: llm.JudgeChoice, Instructions: "route team", Options: map[string]string{"billing": "Billing", "tech": "Tech"}},
			"severity":   {Type: llm.JudgeScore, Instructions: "rate severity", Levels: []string{"low", "med", "high"}},
		},
	}, "user-1", nil)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}

	if gotAuth != "Bearer sk-test-key" {
		t.Errorf("Authorization header = %q, want Bearer sk-test-key", gotAuth)
	}
	if gotPath != "/v1/decisions" {
		t.Errorf("URL path = %q, want /v1/decisions", gotPath)
	}
	if res.Model != "gpt-6-luna-2026-09-01" {
		t.Errorf("Model = %q, want gpt-6-luna-2026-09-01", res.Model)
	}
	if res.Usage.Input != 45 || res.Usage.Output != 0 {
		t.Errorf("Usage = %+v", res.Usage)
	}

	ansNoul := res.Answers["is_valid"]
	if ansNoul.Type != llm.JudgeNoul || ansNoul.Noul != 0.85 {
		t.Errorf("is_valid = %+v", ansNoul)
	}

	ansChoice := res.Answers["department"]
	if ansChoice.Type != llm.JudgeChoice || ansChoice.Choice != "billing" || ansChoice.Confidence != 0.92 {
		t.Errorf("department = %+v", ansChoice)
	}

	ansScore := res.Answers["severity"]
	if ansScore.Type != llm.JudgeScore || ansScore.Score != 1.75 || ansScore.Legend["0"] != "low" {
		t.Errorf("severity = %+v", ansScore)
	}
}

func TestOpenAIJudgeRejectsMissingAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prob := 0.5
		_ = json.NewEncoder(w).Encode(openAIDecisionResponse{
			Model: "gpt-6-luna",
			Answers: []openAIDecisionAnswer{
				{Name: "q1", Type: "predicate", Probability: &prob},
			},
		})
	}))
	defer srv.Close()

	adapter := NewOpenAIAdapter(nil, "gpt-6-luna", nil)
	adapter.SetDecisionsConfig(srv.URL, "sk-test")

	_, err := adapter.Judge(context.Background(), &llm.JudgeRequest{
		StateJSON: `"state"`,
		Questions: map[string]*llm.JudgeQuestion{
			"q1": {Type: llm.JudgeNoul, Instructions: "first?"},
			"q2": {Type: llm.JudgeNoul, Instructions: "second?"},
		},
	}, "", nil)
	if err == nil {
		t.Fatal("expected error when response misses an answer")
	}
	if !strings.Contains(err.Error(), `no answer for question "q2"`) {
		t.Errorf("error message = %q", err.Error())
	}
}

func TestOpenAIJudgeWithoutAPIKeyRefuses(t *testing.T) {
	adapter := NewOpenAIAdapter(nil, "gpt-6-luna", nil)
	_, err := adapter.Judge(context.Background(), &llm.JudgeRequest{
		StateJSON: `"state"`,
		Questions: map[string]*llm.JudgeQuestion{
			"q": {Type: llm.JudgeNoul, Instructions: "test"},
		},
	}, "", nil)
	if err != llm.ErrJudgmentUnsupported {
		t.Errorf("err = %v, want ErrJudgmentUnsupported", err)
	}
}

func TestOpenAIJudgeHonoursCallerDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
	}))
	defer srv.Close()

	adapter := NewOpenAIAdapter(nil, "gpt-6-luna", nil)
	adapter.SetDecisionsConfig(srv.URL, "sk-test")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	_, err := adapter.Judge(ctx, &llm.JudgeRequest{
		StateJSON: `"state"`,
		Questions: map[string]*llm.JudgeQuestion{"q": {Type: llm.JudgeNoul, Instructions: "test"}},
	}, "", nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("err = %v, want context deadline exceeded", err)
	}
}

func TestOpenAIJudgePreservesStateVerbatim(t *testing.T) {
	var capturedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		capturedBody = string(bodyBytes)
		prob := 0.7
		_ = json.NewEncoder(w).Encode(openAIDecisionResponse{
			Model: "gpt-6-luna",
			Answers: []openAIDecisionAnswer{
				{Name: "q", Type: "predicate", Probability: &prob},
			},
		})
	}))
	defer srv.Close()

	adapter := NewOpenAIAdapter(nil, "gpt-6-luna", nil)
	adapter.SetDecisionsConfig(srv.URL, "sk-test")

	// State with large int that would lose precision if parsed as float64, plus specific key order
	rawState := `{"large_id":9007199254740993,"zeta":"last","alpha":"first"}`
	_, err := adapter.Judge(context.Background(), &llm.JudgeRequest{
		StateJSON: rawState,
		Questions: map[string]*llm.JudgeQuestion{"q": {Type: llm.JudgeNoul, Instructions: "test"}},
	}, "", nil)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}

	if !strings.Contains(capturedBody, `"input":{"large_id":9007199254740993,"zeta":"last","alpha":"first"}`) {
		t.Errorf("state not preserved verbatim in body: %s", capturedBody)
	}
}

func TestOpenAIJudgeCustomEndpointWithV1(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		prob := 0.5
		_ = json.NewEncoder(w).Encode(openAIDecisionResponse{
			Model: "gpt-6-luna",
			Answers: []openAIDecisionAnswer{
				{Name: "q", Type: "predicate", Probability: &prob},
			},
		})
	}))
	defer srv.Close()

	// Test endpoints configured with /v1 and trailing slash
	endpoints := []string{srv.URL + "/v1", srv.URL + "/v1/"}
	for _, ep := range endpoints {
		gotPath = ""
		adapter := NewOpenAIAdapter(nil, "gpt-6-luna", nil)
		adapter.SetDecisionsConfig(ep, "sk-test")

		_, err := adapter.Judge(context.Background(), &llm.JudgeRequest{
			StateJSON: `{}`,
			Questions: map[string]*llm.JudgeQuestion{"q": {Type: llm.JudgeNoul, Instructions: "test"}},
		}, "", nil)
		if err != nil {
			t.Fatalf("endpoint %s Judge: %v", ep, err)
		}
		if gotPath != "/v1/decisions" {
			t.Errorf("endpoint %s called path %q, want /v1/decisions", ep, gotPath)
		}
	}
}
