package grpcapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/guardrails"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

type fakeRolloutServer struct {
	mu      sync.Mutex
	calls   int
	bodies  []string
	handler func(w http.ResponseWriter, r *http.Request, callCount int, body []byte)
}

func (f *fakeRolloutServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls++
	callCount := f.calls
	f.bodies = append(f.bodies, string(body))
	f.mu.Unlock()

	if f.handler != nil {
		f.handler(w, r, callCount, body)
		return
	}

	score := 0.5
	if strings.Contains(string(body), "good_move") {
		score = 0.95
	} else if strings.Contains(string(body), "bad_blunder") {
		score = 0.1
	}

	var parsed struct {
		Questions map[string]any `json:"questions"`
	}
	_ = json.Unmarshal(body, &parsed)
	answers := make(map[string]any, len(parsed.Questions))
	for qName := range parsed.Questions {
		answers[qName] = map[string]any{
			"type":  "score",
			"score": score,
		}
	}
	if len(answers) == 0 {
		answers["eval"] = map[string]any{"type": "score", "score": score}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model":   "jev-rollout",
		"answers": answers,
		"usage":   map[string]any{"input_tokens": 150, "output_tokens": 0},
	})
}

func TestEvaluateRollouts_HappyPath(t *testing.T) {
	fake := &fakeRolloutServer{}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	s := newJudgeServer(t,
		[]*proxydb.Model{judgeModel("jev-rollout", srv.URL)},
		[]*proxydb.Rule{judgeRule("jev-rollout")},
		nil,
	)

	req := &pb.EvaluateRolloutsRequest{
		StateJson: `{"board":"rnbqkbnr/pppppppp/8/8/8/8/PPPPPPPP/RNBQKBNR w KQkq - 0 1"}`,
		Candidates: []*pb.RolloutCandidate{
			{Id: "c1", ActionJson: `{"move":"e4","type":"good_move"}`},
			{Id: "c2", ActionJson: `{"move":"f3","type":"bad_blunder"}`},
			{Id: "c3", ActionJson: `{"move":"d4","type":"normal"}`},
		},
		Criteria: map[string]*pb.JudgeQuestion{
			"eval": {
				Type:         pb.JudgeType_JUDGE_TYPE_SCORE,
				Instructions: "Rate the move quality",
				Levels:       []string{"blunder", "mistake", "inaccuracy", "good", "best"},
			},
		},
	}

	resp, err := s.EvaluateRollouts(testIdentity(), req)
	if err != nil {
		t.Fatalf("EvaluateRollouts failed: %v", err)
	}

	if len(resp.Evaluations) != 3 {
		t.Fatalf("got %d evaluations, want 3", len(resp.Evaluations))
	}
	if resp.BestCandidateId != "c1" {
		t.Errorf("best candidate = %q, want c1", resp.BestCandidateId)
	}
	if resp.Evaluations[0].Id != "c1" || resp.Evaluations[0].Rank != 1 {
		t.Errorf("evaluation[0] = %+v, want c1 with rank 1", resp.Evaluations[0])
	}
	if resp.Evaluations[2].Id != "c2" || resp.Evaluations[2].Rank != 3 {
		t.Errorf("evaluation[2] = %+v, want c2 with rank 3", resp.Evaluations[2])
	}
	if resp.Usage.Input <= 0 {
		t.Errorf("input usage = %d, want > 0", resp.Usage.Input)
	}
}

func TestEvaluateRollouts_DefaultCriteria(t *testing.T) {
	fake := &fakeRolloutServer{}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	s := newJudgeServer(t,
		[]*proxydb.Model{judgeModel("jev-rollout", srv.URL)},
		[]*proxydb.Rule{judgeRule("jev-rollout")},
		nil,
	)

	req := &pb.EvaluateRolloutsRequest{
		StateJson: `{"step":1}`,
		Candidates: []*pb.RolloutCandidate{
			{Id: "cand1", ActionJson: `{"action":"turn_left"}`},
		},
		// Criteria omitted to trigger default
	}

	resp, err := s.EvaluateRollouts(testIdentity(), req)
	if err != nil {
		t.Fatalf("EvaluateRollouts with default criteria failed: %v", err)
	}
	if len(resp.Evaluations) != 1 {
		t.Fatalf("got %d evaluations, want 1", len(resp.Evaluations))
	}
	if resp.BestCandidateId != "cand1" {
		t.Errorf("best candidate = %q, want cand1", resp.BestCandidateId)
	}
}

func TestEvaluateRollouts_Validation(t *testing.T) {
	s := newJudgeServer(t, nil, nil, nil)

	// No candidates
	_, err := s.EvaluateRollouts(testIdentity(), &pb.EvaluateRolloutsRequest{
		StateJson: `{"x":1}`,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty candidates err = %v, want InvalidArgument", err)
	}

	// Duplicate candidate id
	_, err = s.EvaluateRollouts(testIdentity(), &pb.EvaluateRolloutsRequest{
		StateJson: `{"x":1}`,
		Candidates: []*pb.RolloutCandidate{
			{Id: "same", ActionJson: `{"a":1}`},
			{Id: "same", ActionJson: `{"a":2}`},
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("duplicate candidate id err = %v, want InvalidArgument", err)
	}

	// Empty candidate id
	_, err = s.EvaluateRollouts(testIdentity(), &pb.EvaluateRolloutsRequest{
		StateJson: `{"x":1}`,
		Candidates: []*pb.RolloutCandidate{
			{Id: "", ActionJson: `{"a":1}`},
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty candidate id err = %v, want InvalidArgument", err)
	}

	// Invalid candidate JSON
	_, err = s.EvaluateRollouts(testIdentity(), &pb.EvaluateRolloutsRequest{
		StateJson: `{"x":1}`,
		Candidates: []*pb.RolloutCandidate{
			{Id: "c1", ActionJson: `not-json`},
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("invalid action_json err = %v, want InvalidArgument", err)
	}

	// Too many candidates (> 64)
	tooMany := make([]*pb.RolloutCandidate, 65)
	for i := range tooMany {
		tooMany[i] = &pb.RolloutCandidate{Id: fmt.Sprintf("c%d", i), ActionJson: `{}`}
	}
	_, err = s.EvaluateRollouts(testIdentity(), &pb.EvaluateRolloutsRequest{
		StateJson:  `{"x":1}`,
		Candidates: tooMany,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("too many candidates err = %v, want InvalidArgument", err)
	}
}

func TestEvaluateRollouts_Multimodal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/decisions/rollouts" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model":             "multimodal-judge",
				"best_candidate_id": "c1",
				"evaluations": []map[string]any{
					{
						"id":              "c1",
						"rank":            1,
						"composite_score": 0.85,
						"answers": []map[string]any{
							{"name": "check", "type": "predicate", "probability": 0.85},
						},
					},
				},
				"usage": map[string]any{"input_tokens": 1200, "output_tokens": 0},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "multimodal-judge",
			"answers": []map[string]any{
				{"name": "check", "type": "predicate", "probability": 0.85},
			},
			"usage": map[string]any{"input_tokens": 1200, "output_tokens": 0},
		})
	}))
	defer srv.Close()

	model := &proxydb.Model{
		ID: "multimodal-judge", Vendor: registry.VendorOpenAI, Endpoint: srv.URL,
		Efforts:      []string{registry.EffortLow},
		Capabilities: []string{registry.CapabilityJudge, registry.CapabilityVision},
		Enabled:      true,
	}
	s := newJudgeServer(t,
		[]*proxydb.Model{model},
		[]*proxydb.Rule{judgeRule("multimodal-judge")},
		nil,
	)

	req := &pb.EvaluateRolloutsRequest{
		StateMedia: []*pb.MediaPart{
			{Part: &pb.MediaPart_Image{Image: &pb.ImagePart{Url: "https://example.com/state.jpg"}}},
		},
		Candidates: []*pb.RolloutCandidate{
			{
				Id: "c1",
				ActionMedia: []*pb.MediaPart{
					{Part: &pb.MediaPart_Image{Image: &pb.ImagePart{Url: "https://example.com/next.jpg"}}},
				},
			},
		},
		Criteria: map[string]*pb.JudgeQuestion{
			"check": {
				Type:         pb.JudgeType_JUDGE_TYPE_NOUL,
				Instructions: "Does next state improve visibility?",
			},
		},
	}

	resp, err := s.EvaluateRollouts(testIdentity(), req)
	if err != nil {
		t.Fatalf("Multimodal EvaluateRollouts failed: %v", err)
	}
	if len(resp.Evaluations) != 1 {
		t.Fatalf("got %d evaluations, want 1", len(resp.Evaluations))
	}
}

func TestEvaluateRollouts_Guardrails(t *testing.T) {
	fake := &fakeRolloutServer{}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	s := newJudgeServer(t,
		[]*proxydb.Model{judgeModel("jev-rollout", srv.URL)},
		[]*proxydb.Rule{judgeRule("jev-rollout")},
		nil,
	).WithGuardrails(&guardrails.Guardrails{
		Enabled:   true,
		PIIAction: guardrails.ActionBlock,
	})

	// Contains phone number in candidate action text -> block
	req := &pb.EvaluateRolloutsRequest{
		StateJson: `{"user":"agent"}`,
		Candidates: []*pb.RolloutCandidate{
			{Id: "c1", ActionText: "Contact client at 555-123-4567 for confirmation."},
		},
	}

	_, err := s.EvaluateRollouts(testIdentity(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Guardrail block err = %v, want InvalidArgument", err)
	}
}

type mockBidiStreamServer struct {
	ctx    context.Context
	in     chan *pb.JudgeRequest
	out    chan *pb.JudgeResponse
	closed bool
	mu     sync.Mutex
}

func newMockBidiStream(ctx context.Context) *mockBidiStreamServer {
	return &mockBidiStreamServer{
		ctx: ctx,
		in:  make(chan *pb.JudgeRequest, 10),
		out: make(chan *pb.JudgeResponse, 10),
	}
}

func (m *mockBidiStreamServer) Context() context.Context {
	return m.ctx
}

func (m *mockBidiStreamServer) Recv() (*pb.JudgeRequest, error) {
	req, ok := <-m.in
	if !ok {
		return nil, io.EOF
	}
	return req, nil
}

func (m *mockBidiStreamServer) Send(resp *pb.JudgeResponse) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("stream closed")
	}
	m.out <- resp
	return nil
}

func (m *mockBidiStreamServer) SetHeader(metadata.MD) error  { return nil }
func (m *mockBidiStreamServer) SendHeader(metadata.MD) error { return nil }
func (m *mockBidiStreamServer) SetTrailer(metadata.MD)       {}
func (m *mockBidiStreamServer) SendMsg(any) error            { return nil }
func (m *mockBidiStreamServer) RecvMsg(any) error            { return nil }

func TestJudgeStream_HappyPath(t *testing.T) {
	fake := &fakeJudge{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	defer srv.Close()

	s := newJudgeServer(t,
		[]*proxydb.Model{judgeModel("jev-stream", srv.URL)},
		[]*proxydb.Rule{judgeRule("jev-stream")},
		nil,
	)

	bidi := newMockBidiStream(testIdentity())

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.JudgeStream(bidi)
	}()

	// Send 3 streaming decisions
	for i := 0; i < 3; i++ {
		bidi.in <- &pb.JudgeRequest{
			StateJson: fmt.Sprintf(`{"step":%d}`, i),
			Questions: nounQuestion(),
		}
		resp := <-bidi.out
		if resp == nil || len(resp.Answers) == 0 {
			t.Fatalf("step %d: got empty answer in stream", i)
		}
	}

	// Client closes stream
	close(bidi.in)
	if err := <-errCh; err != nil {
		t.Fatalf("JudgeStream returned error: %v", err)
	}
}

type benchJudge struct{}

func (b *benchJudge) Judge(ctx context.Context, req *llm.JudgeRequest, _ string, _ map[string]string) (*llm.JudgeResult, error) {
	answers := make(map[string]*llm.JudgeAnswer, len(req.Questions))
	for q := range req.Questions {
		answers[q] = &llm.JudgeAnswer{Type: llm.JudgeScore, Score: 0.8}
	}
	return &llm.JudgeResult{Model: "bench", Answers: answers, Usage: llm.TokensUsage{Input: 50}}, nil
}

func BenchmarkEvaluateRollouts(b *testing.B) {
	rolloutReq := &llm.EvaluateRolloutsRequest{
		StateJSON: `{"board":"state"}`,
		Candidates: []*llm.RolloutCandidate{
			{ID: "c1", ActionJSON: `{"a":1}`},
			{ID: "c2", ActionJSON: `{"a":2}`},
			{ID: "c3", ActionJSON: `{"a":3}`},
			{ID: "c4", ActionJSON: `{"a":4}`},
			{ID: "c5", ActionJSON: `{"a":5}`},
			{ID: "c6", ActionJSON: `{"a":6}`},
			{ID: "c7", ActionJSON: `{"a":7}`},
			{ID: "c8", ActionJSON: `{"a":8}`},
		},
		Criteria: map[string]*llm.JudgeQuestion{
			"eval": {
				Type:         llm.JudgeScore,
				Instructions: "Rate candidate",
				Levels:       []string{"low", "med", "high"},
			},
		},
	}

	judge := &benchJudge{}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := executeParallelCandidateRollouts(ctx, judge, rolloutReq, "user", nil)
		if err != nil || len(res.Evaluations) != 8 {
			b.Fatalf("EvaluateRollouts: %v", err)
		}
	}
}

func TestCalculateCompositeScore_MonotonicScoreLevels(t *testing.T) {
	criteria := map[string]*llm.JudgeQuestion{
		"quality": {
			Type:   llm.JudgeScore,
			Levels: []string{"bad", "suboptimal", "ok", "good", "best"}, // 5 levels: 0..4
		},
	}

	scoreBad := calculateCompositeScore(map[string]*llm.JudgeAnswer{
		"quality": {Type: llm.JudgeScore, Score: 0.0},
	}, criteria)
	scoreSub := calculateCompositeScore(map[string]*llm.JudgeAnswer{
		"quality": {Type: llm.JudgeScore, Score: 1.0},
	}, criteria)
	scoreOk := calculateCompositeScore(map[string]*llm.JudgeAnswer{
		"quality": {Type: llm.JudgeScore, Score: 2.0},
	}, criteria)
	scoreGood := calculateCompositeScore(map[string]*llm.JudgeAnswer{
		"quality": {Type: llm.JudgeScore, Score: 3.0},
	}, criteria)
	scoreBest := calculateCompositeScore(map[string]*llm.JudgeAnswer{
		"quality": {Type: llm.JudgeScore, Score: 4.0},
	}, criteria)

	if !(scoreBad < scoreSub && scoreSub < scoreOk && scoreOk < scoreGood && scoreGood < scoreBest) {
		t.Errorf("scores not strictly monotonic: bad=%f, sub=%f, ok=%f, good=%f, best=%f",
			scoreBad, scoreSub, scoreOk, scoreGood, scoreBest)
	}
	if scoreBest != 1.0 {
		t.Errorf("best score = %f, want 1.0", scoreBest)
	}
	if scoreSub != 0.25 {
		t.Errorf("suboptimal score = %f, want 0.25", scoreSub)
	}
}

type mutatingJudge struct{}

func (m *mutatingJudge) Judge(_ context.Context, req *llm.JudgeRequest, _ string, meta map[string]string) (*llm.JudgeResult, error) {
	// Mutate meta to verify concurrent safety across candidate goroutines
	meta["node_id"] = "node-123"
	meta["visited"] = "true"
	return &llm.JudgeResult{
		Answers: map[string]*llm.JudgeAnswer{"q": {Type: llm.JudgeNoul, Noul: 0.8}},
		Usage:   llm.TokensUsage{Input: 10, Output: 2},
	}, nil
}

func TestParallelRollouts_ConcurrentMetaSafety(t *testing.T) {
	rolloutReq := &llm.EvaluateRolloutsRequest{
		Candidates: []*llm.RolloutCandidate{
			{ID: "c1", ActionText: "a1"},
			{ID: "c2", ActionText: "a2"},
			{ID: "c3", ActionText: "a3"},
			{ID: "c4", ActionText: "a4"},
		},
		Criteria: map[string]*llm.JudgeQuestion{"q": {Type: llm.JudgeNoul}},
	}
	sharedMeta := map[string]string{"initial": "key"}
	res, err := executeParallelCandidateRollouts(context.Background(), &mutatingJudge{}, rolloutReq, "user", sharedMeta)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Evaluations) != 4 {
		t.Errorf("got %d evals, want 4", len(res.Evaluations))
	}
}
