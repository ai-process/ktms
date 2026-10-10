package router

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/genai"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
)

func model(id, vendor string, efforts ...string) *proxydb.Model {
	return &proxydb.Model{ID: id, Vendor: vendor, Efforts: efforts, Enabled: true}
}

func rule(name string, priority int32, effort string, attrs map[string]string, use ...string) *proxydb.Rule {
	if attrs == nil {
		attrs = map[string]string{}
	}
	return &proxydb.Rule{Name: name, Priority: priority, Effort: effort,
		Attributes: attrs, Enabled: true, Use: use}
}

func snapshot() *registry.Snapshot {
	models := []*proxydb.Model{
		model("cheap", registry.VendorGoogle, registry.EffortLow),
		model("deep", registry.VendorDeepSeek, registry.EffortHigh),
		model("big", registry.VendorOpenAI, registry.EffortHigh),
	}
	models[0].Capabilities = []string{registry.CapabilityGoogleSearch}
	// Priority order in the snapshot is descending, as the DB query returns.
	rules := []*proxydb.Rule{
		rule("chinese-high", 100, registry.EffortHigh, map[string]string{"subject": "chinese"}, "deep", "big"),
		rule("high-default", 2, registry.EffortHigh, nil, "big"),
		rule("any-default", 0, "", nil, "cheap", "big"),
	}
	keys := map[string]map[string]string{
		"key1": {registry.VendorGoogle: "g", registry.VendorOpenAI: "o", registry.VendorDeepSeek: "d"},
		"key2": {registry.VendorOpenAI: "o"},
	}
	return registry.NewSnapshotForTest(models, rules, keys, nil)
}

func TestMatchPrecedenceAndPredicates(t *testing.T) {
	snap := snapshot()

	if r := Match(snap, registry.EffortHigh, map[string]string{"subject": "chinese"}); r.Name != "chinese-high" {
		t.Fatalf("matched %s, want chinese-high", r.Name)
	}
	// Attribute mismatch falls through to the effort default.
	if r := Match(snap, registry.EffortHigh, map[string]string{"subject": "english"}); r.Name != "high-default" {
		t.Fatalf("matched %s, want high-default", r.Name)
	}
	// No high rule matched attributes-wise? any-default still catches low.
	if r := Match(snap, registry.EffortLow, nil); r.Name != "any-default" {
		t.Fatalf("matched %s, want any-default", r.Name)
	}
	// Extra request attributes don't block a rule with fewer predicates.
	if r := Match(snap, registry.EffortHigh, map[string]string{"task": "x"}); r.Name != "high-default" {
		t.Fatalf("matched %s, want high-default", r.Name)
	}
}

func TestFilterChain(t *testing.T) {
	snap := snapshot()

	// key1 holds all vendor keys: full chain survives.
	models, err := FilterChain(snap, []string{"deep", "big"}, "key1", registry.EffortHigh, false, "")
	if err != nil || len(models) != 2 {
		t.Fatalf("models=%v err=%v", models, err)
	}
	// key2 has only openai: deepseek drops out.
	models, err = FilterChain(snap, []string{"deep", "big"}, "key2", registry.EffortHigh, false, "")
	if err != nil || len(models) != 1 || models[0].ID != "big" {
		t.Fatalf("models=%v err=%v", models, err)
	}
	// Effort filter: cheap serves low only.
	if _, err = FilterChain(snap, []string{"cheap"}, "key1", registry.EffortHigh, false, ""); !errors.Is(err, ErrNoCapableModel) {
		t.Fatalf("err=%v, want ErrNoCapableModel", err)
	}
	// Capability filter: only cheap has google_search.
	if _, err = FilterChain(snap, []string{"big"}, "key1", registry.EffortHigh, true, ""); !errors.Is(err, ErrNoCapableModel) {
		t.Fatalf("err=%v, want ErrNoCapableModel", err)
	}
	// Capable model exists but the client lacks the vendor key.
	if _, err = FilterChain(snap, []string{"deep"}, "key2", registry.EffortHigh, false, ""); !errors.Is(err, ErrNoVendorKey) {
		t.Fatalf("err=%v, want ErrNoVendorKey", err)
	}
}

// fakeAdapter scripts per-call outcomes.
type fakeAdapter struct {
	calls     int
	responses []func() (*llm.Response, error)
}

func (f *fakeAdapter) GenerateText(*llm.ChatContext) (*llm.Response, error) {
	i := f.calls
	f.calls++
	if i >= len(f.responses) {
		i = len(f.responses) - 1
	}
	return f.responses[i]()
}

func ok(text string) func() (*llm.Response, error) {
	return func() (*llm.Response, error) {
		r := &llm.Response{Usage: llm.TokensUsage{Input: 10, Output: 5}}
		r.AddMessage(&llm.Message{Text: text, Type: llm.MessageTypeBot})
		return r, nil
	}
}

func fail(err error) func() (*llm.Response, error) {
	return func() (*llm.Response, error) { return nil, err }
}

// recordingThrottle logs reserve/settle pairs.
type recordingThrottle struct {
	NoopThrottle
	rpmDeny  map[string]bool
	settled  []llm.TokensUsage
	reserves int
}

func (r *recordingThrottle) AllowRPM(_ context.Context, _ string, m *proxydb.Model) bool {
	return !r.rpmDeny[m.ID]
}

func (r *recordingThrottle) Reserve(context.Context, *proxydb.Model, string, string, int64) (*Reservation, string) {
	r.reserves++
	return &Reservation{Estimate: 1}, ""
}

func (r *recordingThrottle) Settle(_ *Reservation, usage llm.TokensUsage) {
	r.settled = append(r.settled, usage)
}

// retryableErr mimics a vendor 500 via the genai error type the classifier knows.
var retryableErr = vendor500()

func TestExecuteFallsThroughChainOnRetryable(t *testing.T) {
	broken := &fakeAdapter{responses: []func() (*llm.Response, error){fail(retryableErr)}}
	healthy := &fakeAdapter{responses: []func() (*llm.Response, error){ok("hello")}}
	adapters := map[string]llm.ClientAdapter{"deep": broken, "big": healthy}
	th := &recordingThrottle{}

	resp, m, err := Execute(context.Background(), th, Request{
		Chain:   []*proxydb.Model{model("deep", registry.VendorDeepSeek, registry.EffortHigh), model("big", registry.VendorOpenAI, registry.EffortHigh)},
		KeyName: "k", Chat: llm.NewChatContext(),
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) { return adapters[m.ID], nil },
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if m.ID != "big" || resp.Choices[0].Text != "hello" {
		t.Fatalf("resolved %s, resp %v", m.ID, resp.Choices)
	}
	if broken.calls != perModelAttempts {
		t.Fatalf("broken model called %d times, want %d", broken.calls, perModelAttempts)
	}
	// Two reservations (one per admitted model); failed one settled to zero.
	if th.reserves != 2 || len(th.settled) != 2 {
		t.Fatalf("reserves=%d settled=%v", th.reserves, th.settled)
	}
	if th.settled[0] != (llm.TokensUsage{}) || th.settled[1] != (llm.TokensUsage{Input: 10, Output: 5}) {
		t.Fatalf("settled = %v", th.settled)
	}
}

func TestExecuteStopsChainOnTerminalError(t *testing.T) {
	terminal := errors.New("schema rejected")
	first := &fakeAdapter{responses: []func() (*llm.Response, error){fail(terminal)}}
	second := &fakeAdapter{responses: []func() (*llm.Response, error){ok("never")}}
	adapters := map[string]llm.ClientAdapter{"a": first, "b": second}

	_, _, err := Execute(context.Background(), &recordingThrottle{}, Request{
		Chain:   []*proxydb.Model{model("a", registry.VendorOpenAI, registry.EffortLow), model("b", registry.VendorOpenAI, registry.EffortLow)},
		KeyName: "k", Chat: llm.NewChatContext(),
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) { return adapters[m.ID], nil },
	})
	if !errors.Is(err, terminal) {
		t.Fatalf("err=%v, want the terminal error", err)
	}
	if second.calls != 0 {
		t.Fatal("terminal error must stop the chain")
	}
	if first.calls != 1 {
		t.Fatalf("terminal error retried: %d calls", first.calls)
	}
}

func TestExecuteAllThrottled(t *testing.T) {
	th := &recordingThrottle{rpmDeny: map[string]bool{"a": true, "b": true}}
	_, _, err := Execute(context.Background(), th, Request{
		Chain:   []*proxydb.Model{model("a", registry.VendorOpenAI, registry.EffortLow), model("b", registry.VendorOpenAI, registry.EffortLow)},
		KeyName: "k", Chat: llm.NewChatContext(),
		Adapter: func(*proxydb.Model) (llm.ClientAdapter, error) { return nil, errors.New("unreachable") },
	})
	st := ToStatus(err)
	if status.Code(st) != codes.ResourceExhausted {
		t.Fatalf("code=%v, want ResourceExhausted", status.Code(st))
	}
	if status.Convert(st).Message() != "throttled:rpm" {
		t.Fatalf("message=%q", status.Convert(st).Message())
	}
}

func TestToStatusMapping(t *testing.T) {
	cases := map[error]codes.Code{
		ErrNotConfigured:                       codes.FailedPrecondition,
		ErrNoCapableModel:                      codes.FailedPrecondition,
		ErrNoVendorKey:                         codes.FailedPrecondition,
		llm.ErrOutputTruncated:                 codes.FailedPrecondition,
		&ChainExhausted{Last: retryableErr}:    codes.Unavailable,
		&ChainExhausted{Last: errors.New("x")}: codes.Internal,
	}
	for err, want := range cases {
		if got := status.Code(ToStatus(err)); got != want {
			t.Errorf("ToStatus(%v) = %v, want %v", err, got, want)
		}
	}
	if ToStatus(nil) != nil {
		t.Error("ToStatus(nil) != nil")
	}
}

// vendor500 builds an error the retryable classifier recognizes as a 5xx.
func vendor500() error {
	return genai.APIError{Code: 500, Message: "upstream boom"}
}

// jsonSchema is the shape the soft-schema path has to police.
func jsonSchema() *llm.ResponseSchema {
	return &llm.ResponseSchema{
		Type:       llm.SchemaPropertyTypeObject,
		Properties: map[string]*llm.SchemaProperty{"answer": {Type: llm.SchemaPropertyTypeString}},
		Required:   []string{"answer"},
	}
}

func chatWithSchema() *llm.ChatContext {
	chat := llm.NewChatContext()
	chat.SetResponseSchema(jsonSchema())
	return chat
}

// A soft-schema model that answers in the wrong shape gets exactly one more
// try, and then the chain moves on rather than failing the request.
func TestExecuteRetriesSoftSchemaThenAdvances(t *testing.T) {
	bad := &fakeAdapter{responses: []func() (*llm.Response, error){ok("sorry, here is prose")}}
	good := &fakeAdapter{responses: []func() (*llm.Response, error){ok(`{"answer":"fine"}`)}}
	adapters := map[string]llm.ClientAdapter{"soft": bad, "strict": good}
	th := &recordingThrottle{}

	resp, m, err := Execute(context.Background(), th, Request{
		Chain: []*proxydb.Model{
			model("soft", registry.VendorDeepSeek, registry.EffortHigh),
			model("strict", registry.VendorGoogle, registry.EffortHigh),
		},
		KeyName: "k", Chat: chatWithSchema(),
		Adapter:    func(m *proxydb.Model) (llm.ClientAdapter, error) { return adapters[m.ID], nil },
		SoftSchema: func(m *proxydb.Model) bool { return m.ID == "soft" },
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if m.ID != "strict" || resp.Choices[0].Text != `{"answer":"fine"}` {
		t.Fatalf("resolved %s: %v", m.ID, resp.Choices)
	}
	if bad.calls != schemaAttempts {
		t.Fatalf("soft model called %d times, want %d (one retry)", bad.calls, schemaAttempts)
	}
	// Both discarded attempts were billed, so both must reach the budget.
	if th.settled[0] != (llm.TokensUsage{Input: 20, Output: 10}) {
		t.Fatalf("discarded attempts were not billed: %v", th.settled[0])
	}
}

// A soft-schema model that complies on the retry keeps the request.
func TestExecuteAcceptsSoftSchemaOnRetry(t *testing.T) {
	flaky := &fakeAdapter{responses: []func() (*llm.Response, error){
		ok("prose first"), ok(`{"answer":"second time"}`),
	}}
	resp, m, err := Execute(context.Background(), &recordingThrottle{}, Request{
		Chain:   []*proxydb.Model{model("soft", registry.VendorDeepSeek, registry.EffortHigh)},
		KeyName: "k", Chat: chatWithSchema(),
		Adapter:    func(*proxydb.Model) (llm.ClientAdapter, error) { return flaky, nil },
		SoftSchema: func(*proxydb.Model) bool { return true },
	})
	if err != nil || m.ID != "soft" || resp.Choices[0].Text != `{"answer":"second time"}` {
		t.Fatalf("resp=%v m=%v err=%v", resp, m, err)
	}
}

// With no model left to try, the schema failure surfaces as vendor_error
// rather than pretending success.
func TestExecuteSchemaFailureExhaustsChain(t *testing.T) {
	bad := &fakeAdapter{responses: []func() (*llm.Response, error){ok("never json")}}
	_, _, err := Execute(context.Background(), &recordingThrottle{}, Request{
		Chain:   []*proxydb.Model{model("soft", registry.VendorDeepSeek, registry.EffortHigh)},
		KeyName: "k", Chat: chatWithSchema(),
		Adapter:    func(*proxydb.Model) (llm.ClientAdapter, error) { return bad, nil },
		SoftSchema: func(*proxydb.Model) bool { return true },
	})
	var schemaErr *SchemaFailure
	if !errors.As(err, &schemaErr) {
		t.Fatalf("err = %v, want a SchemaFailure", err)
	}
	if status.Code(ToStatus(err)) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(ToStatus(err)))
	}
}

// A strict model's reply is never second-guessed: its API already enforced it.
func TestExecuteSkipsCheckForStrictModels(t *testing.T) {
	strict := &fakeAdapter{responses: []func() (*llm.Response, error){ok("not json at all")}}
	resp, _, err := Execute(context.Background(), &recordingThrottle{}, Request{
		Chain:   []*proxydb.Model{model("strict", registry.VendorGoogle, registry.EffortHigh)},
		KeyName: "k", Chat: chatWithSchema(),
		Adapter:    func(*proxydb.Model) (llm.ClientAdapter, error) { return strict, nil },
		SoftSchema: func(*proxydb.Model) bool { return false },
	})
	if err != nil || resp.Choices[0].Text != "not json at all" {
		t.Fatalf("strict reply was second-guessed: resp=%v err=%v", resp, err)
	}
	if strict.calls != 1 {
		t.Fatalf("strict model called %d times, want 1", strict.calls)
	}
}

type fakeStreamAdapter struct {
	fakeAdapter
	stream      func(ctx context.Context, chat *llm.ChatContext, onChunk func(llm.StreamChunk) error) error
	unsupported bool
}

func (f *fakeStreamAdapter) SupportsStreaming() bool {
	return !f.unsupported
}

func (f *fakeStreamAdapter) GenerateTextStream(ctx context.Context, chat *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
	if f.unsupported {
		return llm.ErrStreamingNotSupported
	}
	if f.stream != nil {
		return f.stream(ctx, chat, onChunk)
	}
	return nil
}

func TestExecuteStreamSoftSchemaFailsAndAdvances(t *testing.T) {
	badStream := &fakeStreamAdapter{
		stream: func(_ context.Context, _ *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
			return onChunk(llm.StreamChunk{Delta: "Here is prose"})
		},
	}
	goodStream := &fakeStreamAdapter{
		stream: func(_ context.Context, _ *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
			return onChunk(llm.StreamChunk{Delta: `{"answer":"ok"}`, Usage: &llm.TokensUsage{Input: 5, Output: 3}})
		},
	}
	adapters := map[string]llm.ClientAdapter{"soft": badStream, "good": goodStream}
	th := &recordingThrottle{}

	var receivedChunks []string
	m, err := ExecuteStream(context.Background(), th, Request{
		Chain: []*proxydb.Model{
			model("soft", registry.VendorDeepSeek, registry.EffortHigh),
			model("good", registry.VendorGoogle, registry.EffortHigh),
		},
		KeyName: "k", Chat: chatWithSchema(),
		Adapter:    func(m *proxydb.Model) (llm.ClientAdapter, error) { return adapters[m.ID], nil },
		SoftSchema: func(m *proxydb.Model) bool { return m.ID == "soft" },
	}, func(_ *proxydb.Model, chunk llm.StreamChunk) error {
		receivedChunks = append(receivedChunks, chunk.Delta)
		return nil
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.ID != "good" {
		t.Fatalf("expected resolved model 'good', got %s", m.ID)
	}
	if len(receivedChunks) != 1 || receivedChunks[0] != `{"answer":"ok"}` {
		t.Fatalf("unexpected received chunks: %v", receivedChunks)
	}
	if len(th.settled) != 2 {
		t.Fatalf("expected 2 settlements, got %d", len(th.settled))
	}
	if th.settled[0].Input == 0 || th.settled[0].Output == 0 {
		t.Errorf("expected non-zero fallback settlement for failed soft schema attempt, got %+v", th.settled[0])
	}
}

func TestExecuteStreamAbortedMidStreamChargesBudget(t *testing.T) {
	abortedStream := &fakeStreamAdapter{
		stream: func(_ context.Context, _ *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
			if err := onChunk(llm.StreamChunk{Delta: "partially generated output text"}); err != nil {
				return err
			}
			return errors.New("connection reset by peer")
		},
	}
	th := &recordingThrottle{}
	chat := llm.NewChatContext()
	chat.AddUserMessage("tell me a long story")

	m, err := ExecuteStream(context.Background(), th, Request{
		Chain:    []*proxydb.Model{model("m1", registry.VendorOpenAI, registry.EffortHigh)},
		KeyName:  "k",
		Chat:     chat,
		Estimate: 100,
		Adapter:  func(_ *proxydb.Model) (llm.ClientAdapter, error) { return abortedStream, nil },
	}, func(_ *proxydb.Model, _ llm.StreamChunk) error {
		return nil
	})

	if err == nil {
		t.Fatal("expected stream error")
	}
	if m == nil || m.ID != "m1" {
		t.Fatalf("expected model m1, got %v", m)
	}
	if len(th.settled) != 1 {
		t.Fatalf("expected 1 settlement, got %d", len(th.settled))
	}
	if th.settled[0].Input <= 0 || th.settled[0].Output <= 0 {
		t.Errorf("budget bypass: expected non-zero usage settled on aborted stream, got %+v", th.settled[0])
	}
}

func TestExecuteStreamSkipsUnsupportedModel(t *testing.T) {
	unsupported := &fakeStreamAdapter{unsupported: true}
	supported := &fakeStreamAdapter{
		stream: func(_ context.Context, _ *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
			return onChunk(llm.StreamChunk{Delta: "hi"})
		},
	}
	adapters := map[string]llm.ClientAdapter{"no_stream": unsupported, "stream": supported}
	th := &recordingThrottle{}

	m, err := ExecuteStream(context.Background(), th, Request{
		Chain: []*proxydb.Model{
			model("no_stream", registry.VendorOpenAI, registry.EffortHigh),
			model("stream", registry.VendorGoogle, registry.EffortHigh),
		},
		KeyName: "k",
		Chat:    llm.NewChatContext(),
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) { return adapters[m.ID], nil },
	}, func(_ *proxydb.Model, _ llm.StreamChunk) error {
		return nil
	})

	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if m.ID != "stream" {
		t.Fatalf("expected model 'stream', got %s", m.ID)
	}
}
