package adapters

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ai-process/ktms/internal/llm"
)

func TestTypeSafeErrorRetryability(t *testing.T) {
	cases := map[int]bool{
		429: true,  // rate limited
		500: true,  // vendor fault
		529: true,  // the vendor's own "overloaded"
		401: false, // bad credential: every model of this vendor would fail alike
		422: false, // the request itself is wrong
		404: false,
	}
	for code, want := range cases {
		err := &TypeSafeError{Status: code, Body: "x"}
		if got := llm.IsRetryableError(err); got != want {
			t.Errorf("status %d retryable = %v, want %v", code, got, want)
		}
	}
}

// A 401 must never carry the vendor's body into a log line or an error string:
// it can echo the credential that was rejected.
func TestTypeSafeErrorHidesUnauthorizedBody(t *testing.T) {
	err := &TypeSafeError{Status: 401, Body: "invalid key ts-secret-value"}
	if strings.Contains(err.Error(), "ts-secret-value") {
		t.Errorf("401 message leaks the body: %s", err.Error())
	}
}

func TestTypeSafeRefusesTextGeneration(t *testing.T) {
	a := NewTypeSafeAdapter("https://api.typesafe.ai", "k", "jev-latest")
	if _, err := a.GenerateText(context.Background(), llm.NewChatContext()); err == nil {
		t.Fatal("a judgment-only vendor must refuse text generation")
	}
}

func TestToTypeSafeQuestionsRendersCriteriaPerType(t *testing.T) {
	got := toTypeSafeQuestions(map[string]*llm.JudgeQuestion{
		"noul_with":    {Type: llm.JudgeNoul, Instructions: "does it hold?", TrueCriteria: "yes means"},
		"noul_without": {Type: llm.JudgeNoul, Instructions: "does it hold?"},
		"pick":         {Type: llm.JudgeChoice, Instructions: "which?", Options: map[string]string{"a": "first", "b": "second"}},
		"rate":         {Type: llm.JudgeScore, Instructions: "how much?", Levels: []string{"low", "high"}},
	})

	if c, ok := got["noul_with"].Criteria.(map[string]string); !ok || c["true"] != "yes means" {
		t.Errorf("noul criteria = %#v, want a true/false map", got["noul_with"].Criteria)
	}
	// A noul that states no criteria must send none, not an empty pair.
	if got["noul_without"].Criteria != nil {
		t.Errorf("bare noul sent criteria %#v", got["noul_without"].Criteria)
	}
	if c, ok := got["pick"].Criteria.(map[string]string); !ok || c["b"] != "second" {
		t.Errorf("choice criteria = %#v, want the option map", got["pick"].Criteria)
	}
	if c, ok := got["rate"].Criteria.([]string); !ok || len(c) != 2 {
		t.Errorf("score criteria = %#v, want the ordered levels", got["rate"].Criteria)
	}
}

func TestTypeSafeJudgeSendsBearerAndPath(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.7}},"usage":{"input_tokens":10}}`))
	}))
	defer srv.Close()

	a := NewTypeSafeAdapter(srv.URL+"/", "ts-key", "jev-latest")
	res, err := a.Judge(context.Background(), &llm.JudgeRequest{
		StateJSON: `"hi"`,
		Questions: map[string]*llm.JudgeQuestion{"q": {Type: llm.JudgeNoul, Instructions: "does it hold?"}},
	}, "stud-1", nil)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if gotAuth != "Bearer ts-key" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if gotPath != "/v1/systemone" {
		t.Errorf("path = %q, want /v1/systemone (trailing slash in the endpoint must not double up)", gotPath)
	}
	if res.Answers["q"].Noul != 0.7 {
		t.Errorf("noul = %v", res.Answers["q"].Noul)
	}
	// The vendor's own model id wins: it resolves the alias we asked for.
	if res.Model != "jev-1.13.0" {
		t.Errorf("model = %q, want the vendor's resolved id", res.Model)
	}
	if res.Usage.Input != 10 {
		t.Errorf("input tokens = %d", res.Usage.Input)
	}
}

func TestTypeSafeJudgeRejectsMissingAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"other":{"type":"noul","noul":0.1}},"usage":{"input_tokens":5}}`))
	}))
	defer srv.Close()

	a := NewTypeSafeAdapter(srv.URL, "k", "jev-latest")
	_, err := a.Judge(context.Background(), &llm.JudgeRequest{
		StateJSON: `"hi"`,
		Questions: map[string]*llm.JudgeQuestion{"q": {Type: llm.JudgeNoul, Instructions: "does it hold?"}},
	}, "", nil)
	if err == nil {
		t.Fatal("a missing answer must be an error, not a zero-valued judgment")
	}
}

// The caller's deadline wins over the adapter's own ceiling: when a judgment's
// requester has given up there is no point holding the call open to 8s.
func TestTypeSafeJudgeHonoursTheCallerDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(1500 * time.Millisecond) // far past the caller's deadline
		_, _ = w.Write([]byte(`{"answers":{}}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	a := NewTypeSafeAdapter(srv.URL, "k", "jev-latest")
	started := time.Now()
	_, err := a.Judge(ctx, &llm.JudgeRequest{
		StateJSON: `"hi"`,
		Questions: map[string]*llm.JudgeQuestion{"q": {Type: llm.JudgeNoul, Instructions: "does it hold?"}},
	}, "", nil)
	if err == nil {
		t.Fatal("a judgment past its caller's deadline returned success")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("returned after %v, want the caller's 50ms deadline, not the 8s ceiling", elapsed)
	}
}

// System One reads JSON state only; judging without the media would answer
// about a different state, so the chain must move on without a vendor call.
func TestTypeSafeJudgeRefusesStateMedia(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called.Store(true) }))
	defer srv.Close()

	a := NewTypeSafeAdapter(srv.URL, "k", "jev-latest")
	_, err := a.Judge(context.Background(), &llm.JudgeRequest{
		StateMedia: []*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, URL: "https://x/a.png"}},
		Questions:  map[string]*llm.JudgeQuestion{"q": {Type: llm.JudgeNoul, Instructions: "does it hold?"}},
	}, "u", nil)
	if !errors.Is(err, llm.ErrJudgmentUnsupported) {
		t.Errorf("err = %v, want ErrJudgmentUnsupported", err)
	}
	if called.Load() {
		t.Error("vendor was called")
	}
}
