package router

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ai-process/llm-proxy/internal/circuitbreaker"
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
	"github.com/openai/openai-go/v3"
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

// Local models reach nodes with the node's own token, so a client needs no
// vendor key for them. Before this, a rule of only local models was emptied
// and every request failed with no_vendor_key.
func TestFilterChainLocalModelsNeedNoVendorKey(t *testing.T) {
	models := []*proxydb.Model{
		model("local-a", registry.VendorLocal, registry.EffortHigh),
		model("local-b", registry.VendorLocal, registry.EffortHigh),
		model("big", registry.VendorOpenAI, registry.EffortHigh),
	}
	// The client holds no vendor keys at all.
	snap := registry.NewSnapshotForTest(models, nil, map[string]map[string]string{"bare": {}}, nil)

	got, err := FilterChain(snap, []string{"local-a", "local-b"}, "bare", registry.EffortHigh, false, "")
	if err != nil || len(got) != 2 || got[0].ID != "local-a" || got[1].ID != "local-b" {
		t.Fatalf("local-only chain: got %v, err %v; want both local models", got, err)
	}

	// In a mixed chain the keyless vendor model drops out and the local one stays, in order.
	got, err = FilterChain(snap, []string{"big", "local-a"}, "bare", registry.EffortHigh, false, "")
	if err != nil || len(got) != 1 || got[0].ID != "local-a" {
		t.Fatalf("mixed chain: got %v, err %v; want only local-a", got, err)
	}

	// Vendors that do need a key still fail without one.
	if _, err = FilterChain(snap, []string{"big"}, "bare", registry.EffortHigh, false, ""); !errors.Is(err, ErrNoVendorKey) {
		t.Fatalf("err=%v, want ErrNoVendorKey for a vendor model without a key", err)
	}

	// An unknown client id (no entry at all) behaves the same.
	got, err = FilterChain(snap, []string{"local-a"}, "no-such-key", registry.EffortHigh, false, "")
	if err != nil || len(got) != 1 {
		t.Fatalf("client without any key row: got %v, err %v", got, err)
	}
}

// The production path: a rule whose models are all local, called by a client
// that has no vendor keys. It must route and be served, not end in no_vendor_key.
func TestLocalOnlyRuleServesKeylessClient(t *testing.T) {
	models := []*proxydb.Model{
		model("local-a", registry.VendorLocal, registry.EffortLow),
		model("local-b", registry.VendorLocal, registry.EffortLow),
	}
	rules := []*proxydb.Rule{rule("local-only", 1, registry.EffortLow, nil, "local-a", "local-b")}
	snap := registry.NewSnapshotForTest(models, rules, map[string]map[string]string{"bare": {}}, nil)

	r := Match(snap, registry.EffortLow, nil)
	if r == nil {
		t.Fatal("no rule matched")
	}
	chain, err := FilterChain(snap, r.Use, "bare", registry.EffortLow, false, "")
	if err != nil {
		t.Fatalf("FilterChain: %v", err)
	}
	served := &fakeAdapter{responses: []func() (*llm.Response, error){ok("from a node")}}
	resp, m, err := Execute(context.Background(), &recordingThrottle{}, Request{
		Chain: chain, KeyName: "bare-key", Chat: llm.NewChatContext(),
		Adapter: func(*proxydb.Model) (llm.ClientAdapter, error) { return served, nil },
	})
	if err != nil || m.ID != "local-a" || resp.Choices[0].Text != "from a node" {
		t.Fatalf("resolved %v, %v, err %v", m, resp, err)
	}
}

func TestFilterChainMultimodalCapabilities(t *testing.T) {
	snap := snapshot()
	snap.Models["big"].Capabilities = []string{registry.CapabilityVision, registry.CapabilityAudioInput}

	// Requesting vision capability: only "big" has vision, "deep" does not.
	models, err := FilterChain(snap, []string{"deep", "big"}, "key1", registry.EffortHigh, false, registry.CapabilityVision)
	if err != nil || len(models) != 1 || models[0].ID != "big" {
		t.Fatalf("expected only big to survive vision filter, got models=%v err=%v", models, err)
	}

	// Requesting vision and audio_input: "big" has both.
	models, err = FilterChain(snap, []string{"deep", "big"}, "key1", registry.EffortHigh, false, registry.CapabilityVision, registry.CapabilityAudioInput)
	if err != nil || len(models) != 1 || models[0].ID != "big" {
		t.Fatalf("expected big to survive vision+audio filter, got models=%v err=%v", models, err)
	}

	// Requesting pdf_input: neither has it, so ErrNoCapableModel is returned.
	if _, err = FilterChain(snap, []string{"deep", "big"}, "key1", registry.EffortHigh, false, registry.CapabilityPDFInput); !errors.Is(err, ErrNoCapableModel) {
		t.Fatalf("expected ErrNoCapableModel for missing pdf capability, got %v", err)
	}
}

// fakeAdapter scripts per-call outcomes.
type fakeAdapter struct {
	calls     int
	responses []func() (*llm.Response, error)
}

func (f *fakeAdapter) GenerateText(ctx context.Context, _ *llm.ChatContext) (*llm.Response, error) {
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
	mu       sync.Mutex
	rpmDeny  map[string]bool
	settled  []llm.TokensUsage
	reserves int
}

func (r *recordingThrottle) AllowRPM(_ context.Context, _, _ string, m *proxydb.Model) bool {
	return !r.rpmDeny[m.ID]
}

func (r *recordingThrottle) Reserve(context.Context, *proxydb.Model, string, string, int64, int64) (*Reservation, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reserves++
	return &Reservation{Estimate: 1}, ""
}

func (r *recordingThrottle) Settle(_ *Reservation, usage llm.TokensUsage) {
	r.mu.Lock()
	defer r.mu.Unlock()
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

func TestExecuteStreamDisconnectBeforeFirstChunkChargesBudget(t *testing.T) {
	thinkingStream := &fakeStreamAdapter{
		stream: func(ctx context.Context, _ *llm.ChatContext, _ func(llm.StreamChunk) error) error {
			return ctx.Err()
		},
	}
	th := &recordingThrottle{}
	chat := llm.NewChatContext()
	chat.AddUserMessage("ponder the universe for a minute")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ExecuteStream(ctx, th, Request{
		Chain:    []*proxydb.Model{model("m1", registry.VendorOpenAI, registry.EffortHigh)},
		KeyName:  "k",
		Chat:     chat,
		Estimate: 100,
		Adapter:  func(_ *proxydb.Model) (llm.ClientAdapter, error) { return thinkingStream, nil },
	}, func(_ *proxydb.Model, _ llm.StreamChunk) error {
		return nil
	})

	if err == nil {
		t.Fatal("expected error on canceled context")
	}
	if len(th.settled) != 1 {
		t.Fatalf("expected 1 settlement, got %d", len(th.settled))
	}
	if th.settled[0].Input <= 0 {
		t.Errorf("expected non-zero input tokens charged on pre-chunk disconnect, got %+v", th.settled[0])
	}
}

func TestExecuteStreamSoftSchemaErrorPartwayChargesBudget(t *testing.T) {
	partwayStream := &fakeStreamAdapter{
		stream: func(_ context.Context, _ *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
			if err := onChunk(llm.StreamChunk{Delta: "{\"partial\":"}); err != nil {
				return err
			}
			return errors.New("upstream connection reset")
		},
	}
	th := &recordingThrottle{}

	_, err := ExecuteStream(context.Background(), th, Request{
		Chain:      []*proxydb.Model{model("soft", registry.VendorDeepSeek, registry.EffortHigh)},
		KeyName:    "k",
		Chat:       chatWithSchema(),
		Adapter:    func(_ *proxydb.Model) (llm.ClientAdapter, error) { return partwayStream, nil },
		SoftSchema: func(_ *proxydb.Model) bool { return true },
	}, func(_ *proxydb.Model, _ llm.StreamChunk) error {
		return nil
	})

	if err == nil {
		t.Fatal("expected stream error")
	}
	if len(th.settled) != 1 {
		t.Fatalf("expected 1 settlement, got %d", len(th.settled))
	}
	if th.settled[0].Input <= 0 || th.settled[0].Output <= 0 {
		t.Errorf("expected input and output tokens charged for partway soft schema stream, got %+v", th.settled[0])
	}
}

type mockSkipError struct{}

func (mockSkipError) Error() string         { return "slot exhausted" }
func (mockSkipError) SkipToNextModel() bool { return true }

func TestExecute_SkipToNextModel_Fallback(t *testing.T) {
	m1 := model("node1", registry.VendorOpenAI, registry.EffortHigh)
	m2 := model("cloud2", registry.VendorOpenAI, registry.EffortHigh)

	ad1Calls := 0
	ad1 := &fakeAdapter{
		responses: []func() (*llm.Response, error){
			func() (*llm.Response, error) {
				ad1Calls++
				return nil, mockSkipError{}
			},
		},
	}
	ad2 := &fakeAdapter{
		responses: []func() (*llm.Response, error){
			func() (*llm.Response, error) {
				return &llm.Response{Choices: []*llm.Message{{Text: "hello from cloud"}}}, nil
			},
		},
	}

	resp, chosen, err := Execute(context.Background(), NoopThrottle{}, Request{
		Chain: []*proxydb.Model{m1, m2},
		Chat:  llm.NewChatContext(),
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) {
			if m.ID == "node1" {
				return ad1, nil
			}
			return ad2, nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chosen.ID != "cloud2" {
		t.Errorf("expected cloud2 to be chosen, got %s", chosen.ID)
	}
	if resp.Choices[0].Text != "hello from cloud" {
		t.Errorf("unexpected response text: %s", resp.Choices[0].Text)
	}
	if ad1Calls != 1 {
		t.Errorf("expected exactly 1 call to ad1 (no retries), got %d", ad1Calls)
	}
}

func TestExecuteStream_SkipToNextModel_Fallback(t *testing.T) {
	m1 := model("node1", registry.VendorOpenAI, registry.EffortHigh)
	m2 := model("cloud2", registry.VendorOpenAI, registry.EffortHigh)

	s1 := &fakeStreamAdapter{
		stream: func(_ context.Context, _ *llm.ChatContext, _ func(llm.StreamChunk) error) error {
			return mockSkipError{}
		},
	}
	s2 := &fakeStreamAdapter{
		stream: func(_ context.Context, _ *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
			return onChunk(llm.StreamChunk{Delta: "stream from cloud"})
		},
	}

	var chunks []string
	chosen, err := ExecuteStream(context.Background(), NoopThrottle{}, Request{
		Chain: []*proxydb.Model{m1, m2},
		Chat:  llm.NewChatContext(),
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) {
			if m.ID == "node1" {
				return s1, nil
			}
			return s2, nil
		},
	}, func(_ *proxydb.Model, c llm.StreamChunk) error {
		chunks = append(chunks, c.Delta)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chosen.ID != "cloud2" {
		t.Errorf("expected cloud2, got %s", chosen.ID)
	}
	if len(chunks) != 1 || chunks[0] != "stream from cloud" {
		t.Errorf("unexpected chunks: %v", chunks)
	}
}

func TestExecute_AzureContentFilter_Fallback(t *testing.T) {
	m1 := model("azure-gpt-4o", registry.VendorAzure, registry.EffortHigh)
	m2 := model("cloud-fallback", registry.VendorOpenAI, registry.EffortHigh)

	azureErr := &openai.Error{
		Code:     "content_filter",
		Message:  "The response was filtered due to the prompt triggering Azure OpenAI's content management policy.",
		Request:  &http.Request{Method: "POST", URL: &url.URL{Path: "/chat/completions"}},
		Response: &http.Response{StatusCode: 400},
	}

	ad1 := &fakeAdapter{
		responses: []func() (*llm.Response, error){
			func() (*llm.Response, error) {
				return nil, azureErr
			},
		},
	}
	ad2 := &fakeAdapter{
		responses: []func() (*llm.Response, error){
			func() (*llm.Response, error) {
				return &llm.Response{Choices: []*llm.Message{{Text: "safe reply from fallback"}}}, nil
			},
		},
	}

	resp, chosen, err := Execute(context.Background(), NoopThrottle{}, Request{
		Chain: []*proxydb.Model{m1, m2},
		Chat:  llm.NewChatContext(),
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) {
			if m.ID == "azure-gpt-4o" {
				return ad1, nil
			}
			return ad2, nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chosen.ID != "cloud-fallback" {
		t.Errorf("expected cloud-fallback to be chosen, got %s", chosen.ID)
	}
	if resp.Choices[0].Text != "safe reply from fallback" {
		t.Errorf("unexpected response text: %s", resp.Choices[0].Text)
	}
}

func TestExecuteStream_AzureContentFilter_Fallback(t *testing.T) {
	m1 := model("azure-gpt-4o", registry.VendorAzure, registry.EffortHigh)
	m2 := model("cloud-fallback", registry.VendorOpenAI, registry.EffortHigh)

	azureErr := &openai.Error{
		Code:     "ResponsibleAIPolicyViolation",
		Message:  "The response was filtered due to Azure OpenAI policy.",
		Request:  &http.Request{Method: "POST", URL: &url.URL{Path: "/chat/completions"}},
		Response: &http.Response{StatusCode: 400},
	}

	s1 := &fakeStreamAdapter{
		stream: func(_ context.Context, _ *llm.ChatContext, _ func(llm.StreamChunk) error) error {
			return azureErr
		},
	}
	s2 := &fakeStreamAdapter{
		stream: func(_ context.Context, _ *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
			return onChunk(llm.StreamChunk{Delta: "stream from fallback"})
		},
	}

	var chunks []string
	chosen, err := ExecuteStream(context.Background(), NoopThrottle{}, Request{
		Chain: []*proxydb.Model{m1, m2},
		Chat:  llm.NewChatContext(),
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) {
			if m.ID == "azure-gpt-4o" {
				return s1, nil
			}
			return s2, nil
		},
	}, func(_ *proxydb.Model, c llm.StreamChunk) error {
		chunks = append(chunks, c.Delta)
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chosen.ID != "cloud-fallback" {
		t.Errorf("expected cloud-fallback, got %s", chosen.ID)
	}
	if len(chunks) != 1 || chunks[0] != "stream from fallback" {
		t.Errorf("unexpected chunks: %v", chunks)
	}
}

func TestResolveChain_WeightedRouting(t *testing.T) {
	// 1. Nil rule, single model, or empty weights: return Use unchanged
	if got := ResolveChain(nil); got != nil {
		t.Errorf("expected nil for nil rule, got %v", got)
	}
	rSingle := &proxydb.Rule{Use: []string{"m1"}, Weights: []int32{100}}
	if got := ResolveChain(rSingle); len(got) != 1 || got[0] != "m1" {
		t.Errorf("expected [m1], got %v", got)
	}
	rNoWeights := &proxydb.Rule{Use: []string{"m1", "m2"}}
	if got := ResolveChain(rNoWeights); len(got) != 2 || got[0] != "m1" || got[1] != "m2" {
		t.Errorf("expected [m1, m2], got %v", got)
	}

	// 2. Mismatched weights length: fallback to Use unchanged
	rMismatch := &proxydb.Rule{Use: []string{"m1", "m2"}, Weights: []int32{50}}
	if got := ResolveChain(rMismatch); len(got) != 2 || got[0] != "m1" {
		t.Errorf("expected [m1, m2], got %v", got)
	}

	// 3. Fallback preservation test:
	// With weights [0, 100, 0], m2 MUST always be chosen as primary, and m1, m3 must follow in order
	rForcedM2 := &proxydb.Rule{Use: []string{"m1", "m2", "m3"}, Weights: []int32{0, 100, 0}}
	gotForced := ResolveChain(rForcedM2)
	if len(gotForced) != 3 || gotForced[0] != "m2" || gotForced[1] != "m1" || gotForced[2] != "m3" {
		t.Errorf("expected [m2, m1, m3], got %v", gotForced)
	}

	// 4. Monte-Carlo statistical test with weights [80, 20]
	rWeighted := &proxydb.Rule{Use: []string{"m1", "m2"}, Weights: []int32{80, 20}}
	const iterations = 10000
	m1Count := 0
	for i := 0; i < iterations; i++ {
		chain := ResolveChain(rWeighted)
		if len(chain) != 2 {
			t.Fatalf("unexpected chain length: %d", len(chain))
		}
		if chain[0] == "m1" {
			m1Count++
			if chain[1] != "m2" {
				t.Fatalf("expected fallback m2, got %s", chain[1])
			}
		} else if chain[0] == "m2" {
			if chain[1] != "m1" {
				t.Fatalf("expected fallback m1, got %s", chain[1])
			}
		} else {
			t.Fatalf("unexpected primary model: %s", chain[0])
		}
	}

	ratio := float64(m1Count) / float64(iterations)
	// Expected ~0.80, allow deviation up to 1.5% (i.e. [0.785, 0.815])
	if ratio < 0.785 || ratio > 0.815 {
		t.Errorf("expected ratio ~0.80 with +/- 1.5%% deviation, got %.4f (count=%d)", ratio, m1Count)
	}
}

func TestExecuteCircuitBreakerFailFast(t *testing.T) {
	currentTime := time.Now()
	nowMock := func() time.Time { return currentTime }

	cbMgr := circuitbreaker.NewManager(circuitbreaker.Config{
		Enabled:       true,
		FailThreshold: 2,
		Cooldown:      10 * time.Second,
	})
	cbMgr.SetNowFunc(nowMock)
	snap := snapshot()
	mDeep := snap.Models["deep"] // VendorDeepSeek
	mBig := snap.Models["big"]   // VendorOpenAI
	chain := []*proxydb.Model{mDeep, mBig}

	brokenCalls := 0
	broken := &fakeAdapter{responses: []func() (*llm.Response, error){
		func() (*llm.Response, error) {
			brokenCalls++
			return nil, retryableErr
		},
	}}
	healthyCalls := 0
	healthy := &fakeAdapter{responses: []func() (*llm.Response, error){
		func() (*llm.Response, error) {
			healthyCalls++
			return ok("from healthy fallback")()
		},
	}}
	adapters := map[string]llm.ClientAdapter{"deep": broken, "big": healthy}

	req := Request{
		Chain:          chain,
		KeyName:        "test-key",
		UserID:         "user-1",
		Chat:           llm.NewChatContext(),
		CircuitBreaker: cbMgr,
		Adapter:        func(m *proxydb.Model) (llm.ClientAdapter, error) { return adapters[m.ID], nil },
	}

	// 1st request: deep fails, falls back to big
	resp, m, err := Execute(context.Background(), NoopThrottle{}, req)
	if err != nil || m.ID != "big" || resp.Choices[0].Text != "from healthy fallback" {
		t.Fatalf("call 1 failed: resp=%v, m=%v, err=%v", resp, m, err)
	}
	if cbMgr.State(mDeep.Vendor, mDeep.ID) != circuitbreaker.StateClosed {
		t.Fatalf("expected deep to still be CLOSED, got %v", cbMgr.State(mDeep.Vendor, mDeep.ID))
	}

	// 2nd request: deep fails again -> trips to OPEN!
	resp, m, err = Execute(context.Background(), NoopThrottle{}, req)
	if err != nil || m.ID != "big" || resp.Choices[0].Text != "from healthy fallback" {
		t.Fatalf("call 2 failed: resp=%v, m=%v, err=%v", resp, m, err)
	}
	if cbMgr.State(mDeep.Vendor, mDeep.ID) != circuitbreaker.StateOpen {
		t.Fatalf("expected deep to trip to OPEN, got %v", cbMgr.State(mDeep.Vendor, mDeep.ID))
	}

	callsBefore := brokenCalls
	// 3rd request: deep is OPEN, must fail-fast without calling broken adapter
	resp, m, err = Execute(context.Background(), NoopThrottle{}, req)
	if err != nil || m.ID != "big" || resp.Choices[0].Text != "from healthy fallback" {
		t.Fatalf("call 3 failed: resp=%v, m=%v, err=%v", resp, m, err)
	}
	if brokenCalls != callsBefore {
		t.Fatalf("expected zero calls to broken adapter while OPEN, got %d calls", brokenCalls-callsBefore)
	}

	// 4th request: advance time past cooldown; deep recovers and answers successfully
	currentTime = currentTime.Add(11 * time.Second)
	adapters["deep"] = &fakeAdapter{responses: []func() (*llm.Response, error){ok("deep recovered")}}
	resp, m, err = Execute(context.Background(), NoopThrottle{}, req)
	if err != nil || m.ID != "deep" || resp.Choices[0].Text != "deep recovered" {
		t.Fatalf("call 4 failed: resp=%v, m=%v, err=%v", resp, m, err)
	}
	if cbMgr.State(mDeep.Vendor, mDeep.ID) != circuitbreaker.StateClosed {
		t.Fatalf("expected deep to recover to CLOSED, got %v", cbMgr.State(mDeep.Vendor, mDeep.ID))
	}
}

func TestExecuteCircuitBreakerAllBroken(t *testing.T) {
	cbMgr := circuitbreaker.NewManager(circuitbreaker.Config{
		Enabled:       true,
		FailThreshold: 1,
		Cooldown:      10 * time.Second,
	})
	snap := snapshot()
	mDeep := snap.Models["deep"]
	mBig := snap.Models["big"]
	chain := []*proxydb.Model{mDeep, mBig}

	// Trip both models
	cbMgr.Report(context.Background(), mDeep.Vendor, mDeep.ID, retryableErr)
	cbMgr.Report(context.Background(), mBig.Vendor, mBig.ID, retryableErr)

	if cbMgr.State(mDeep.Vendor, mDeep.ID) != circuitbreaker.StateOpen ||
		cbMgr.State(mBig.Vendor, mBig.ID) != circuitbreaker.StateOpen {
		t.Fatal("expected both models to be OPEN")
	}

	req := Request{
		Chain:          chain,
		KeyName:        "test-key",
		UserID:         "user-1",
		Chat:           llm.NewChatContext(),
		CircuitBreaker: cbMgr,
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) {
			t.Fatal("adapter should not be called when all models are circuit-broken")
			return nil, nil
		},
	}

	_, _, err := Execute(context.Background(), NoopThrottle{}, req)
	if err == nil {
		t.Fatal("expected error when all models are circuit-broken")
	}

	var exhausted *ChainExhausted
	if !errors.As(err, &exhausted) || !exhausted.AllCircuitBroken {
		t.Fatalf("expected ChainExhausted with AllCircuitBroken=true, got %v", err)
	}

	st := ToStatus(err)
	if status.Code(st) != codes.Unavailable {
		t.Fatalf("expected codes.Unavailable, got %v", status.Code(st))
	}
}

func TestExecuteCircuitBreakerAndThrottled(t *testing.T) {
	cbMgr := circuitbreaker.NewManager(circuitbreaker.Config{
		Enabled:       true,
		FailThreshold: 1,
		Cooldown:      10 * time.Second,
	})
	snap := snapshot()
	mDeep := snap.Models["deep"]
	mBig := snap.Models["big"]
	chain := []*proxydb.Model{mDeep, mBig}

	// Trip deep model's circuit breaker
	cbMgr.Report(context.Background(), mDeep.Vendor, mDeep.ID, retryableErr)

	// Throttler that refuses big model for RPM
	th := &recordingThrottle{rpmDeny: map[string]bool{"big": true}}

	req := Request{
		Chain:          chain,
		KeyName:        "test-key",
		UserID:         "user-1",
		Chat:           llm.NewChatContext(),
		CircuitBreaker: cbMgr,
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) {
			t.Fatal("adapter should not be called when candidates are broken or throttled")
			return nil, nil
		},
	}

	_, _, err := Execute(context.Background(), th, req)
	if err == nil {
		t.Fatal("expected error when all models are broken or throttled")
	}

	var exhausted *ChainExhausted
	if !errors.As(err, &exhausted) {
		t.Fatalf("expected ChainExhausted, got %v", err)
	}
	if !exhausted.AllThrottled || !exhausted.AnyCircuitBroken {
		t.Fatalf("expected AllThrottled=true and AnyCircuitBroken=true, got %+v", exhausted)
	}

	st := ToStatus(err)
	if status.Code(st) != codes.ResourceExhausted {
		t.Fatalf("expected codes.ResourceExhausted, got %v (%v)", status.Code(st), st)
	}
}

func TestExecuteStreamCircuitBreakerAndThrottled(t *testing.T) {
	cbMgr := circuitbreaker.NewManager(circuitbreaker.Config{
		Enabled:       true,
		FailThreshold: 1,
		Cooldown:      10 * time.Second,
	})
	snap := snapshot()
	mDeep := snap.Models["deep"]
	mBig := snap.Models["big"]
	chain := []*proxydb.Model{mDeep, mBig}

	cbMgr.Report(context.Background(), mDeep.Vendor, mDeep.ID, retryableErr)
	th := &recordingThrottle{rpmDeny: map[string]bool{"big": true}}

	req := Request{
		Chain:          chain,
		KeyName:        "test-key",
		UserID:         "user-1",
		Chat:           llm.NewChatContext(),
		CircuitBreaker: cbMgr,
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) {
			t.Fatal("adapter should not be called")
			return nil, nil
		},
	}

	_, err := ExecuteStream(context.Background(), th, req, func(_ *proxydb.Model, _ llm.StreamChunk) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error")
	}

	var exhausted *ChainExhausted
	if !errors.As(err, &exhausted) || !exhausted.AllThrottled || !exhausted.AnyCircuitBroken {
		t.Fatalf("expected AllThrottled=true and AnyCircuitBroken=true, got %+v", exhausted)
	}
	if st := ToStatus(err); status.Code(st) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted, got %v", status.Code(st))
	}
}

func TestExecuteMediaCircuitBreakerAndThrottled(t *testing.T) {
	cbMgr := circuitbreaker.NewManager(circuitbreaker.Config{
		Enabled:       true,
		FailThreshold: 1,
		Cooldown:      10 * time.Second,
	})
	snap := snapshot()
	mDeep := snap.Models["deep"]
	mBig := snap.Models["big"]
	chain := []*proxydb.Model{mDeep, mBig}

	cbMgr.Report(context.Background(), mDeep.Vendor, mDeep.ID, retryableErr)
	th := &recordingThrottle{rpmDeny: map[string]bool{"big": true}}

	req := MediaRequest{
		Chain:          chain,
		KeyName:        "test-key",
		UserID:         "user-1",
		CircuitBreaker: cbMgr,
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) {
			t.Fatal("adapter should not be called")
			return nil, nil
		},
		Call: func(m *proxydb.Model, adapter llm.ClientAdapter) (llm.TokensUsage, error) {
			t.Fatal("call should not be invoked")
			return llm.TokensUsage{}, nil
		},
	}

	_, err := ExecuteMedia(context.Background(), th, req)
	if err == nil {
		t.Fatal("expected error")
	}

	var exhausted *ChainExhausted
	if !errors.As(err, &exhausted) || !exhausted.AllThrottled || !exhausted.AnyCircuitBroken {
		t.Fatalf("expected AllThrottled=true and AnyCircuitBroken=true, got %+v", exhausted)
	}
	if st := ToStatus(err); status.Code(st) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted, got %v", status.Code(st))
	}
}

type shadowCaptureAdapter struct {
	fakeAdapter
	capturedChat *llm.ChatContext
	called       chan struct{}
	done         chan struct{}
}

func (s *shadowCaptureAdapter) GenerateText(ctx context.Context, chat *llm.ChatContext) (*llm.Response, error) {
	s.capturedChat = chat
	if s.called != nil {
		select {
		case s.called <- struct{}{}:
		default:
		}
	}
	defer func() {
		if s.done != nil {
			select {
			case s.done <- struct{}{}:
			default:
			}
		}
	}()
	return s.fakeAdapter.GenerateText(ctx, chat)
}

func TestSamplePercentage(t *testing.T) {
	if samplePercentage(0) {
		t.Error("samplePercentage(0) must be false")
	}
	if samplePercentage(-10) {
		t.Error("samplePercentage(-10) must be false")
	}
	if !samplePercentage(100) {
		t.Error("samplePercentage(100) must be true")
	}
	if !samplePercentage(150) {
		t.Error("samplePercentage(150) must be true")
	}
}

func TestExecute_ShadowRouting(t *testing.T) {
	mPrimary := model("primary-m", "openai")
	mShadow := model("shadow-m", "deepseek")

	primary := &fakeAdapter{responses: []func() (*llm.Response, error){ok("primary output")}}
	shadow := &shadowCaptureAdapter{
		fakeAdapter: fakeAdapter{responses: []func() (*llm.Response, error){ok("shadow output")}},
		called:      make(chan struct{}, 1),
		done:        make(chan struct{}, 1),
	}

	adapters := map[string]llm.ClientAdapter{
		"primary-m": primary,
		"shadow-m":  shadow,
	}

	chat := llm.NewChatContext()
	chat.SetMeta(map[string]string{"client": "test"})
	chat.AddUserMessage("hello")

	req := Request{
		Chain:            []*proxydb.Model{mPrimary},
		KeyName:          "test-key",
		UserID:           "user-1",
		Chat:             chat,
		ShadowModel:      mShadow,
		ShadowPercentage: 100,
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) {
			return adapters[m.ID], nil
		},
	}

	resp, chosen, err := Execute(context.Background(), NoopThrottle{}, req)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if chosen.ID != "primary-m" {
		t.Errorf("chosen model = %q, want primary-m", chosen.ID)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Text != "primary output" {
		t.Fatalf("unexpected primary response: %+v", resp)
	}

	select {
	case <-shadow.called:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for shadow execution")
	}

	select {
	case <-shadow.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for shadow completion")
	}

	if shadow.capturedChat == nil {
		t.Fatal("shadow did not receive chat context")
	}
	if shadow.capturedChat.GetMeta()["is_shadow"] != "true" {
		t.Errorf("shadow meta is_shadow = %q, want true", shadow.capturedChat.GetMeta()["is_shadow"])
	}
	if chat.GetMeta()["is_shadow"] != "" {
		t.Errorf("primary chat meta was contaminated with is_shadow: %v", chat.GetMeta())
	}
}

func TestExecute_ShadowFailureIsolated(t *testing.T) {
	mPrimary := model("primary-m", "openai")
	mShadow := model("shadow-m", "deepseek")

	primary := &fakeAdapter{responses: []func() (*llm.Response, error){ok("primary output")}}
	shadow := &shadowCaptureAdapter{
		fakeAdapter: fakeAdapter{responses: []func() (*llm.Response, error){fail(errors.New("shadow vendor failure"))}},
		called:      make(chan struct{}, 1),
		done:        make(chan struct{}, 1),
	}

	adapters := map[string]llm.ClientAdapter{
		"primary-m": primary,
		"shadow-m":  shadow,
	}

	req := Request{
		Chain:            []*proxydb.Model{mPrimary},
		KeyName:          "test-key",
		UserID:           "user-1",
		Chat:             llm.NewChatContext(),
		ShadowModel:      mShadow,
		ShadowPercentage: 100,
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) {
			return adapters[m.ID], nil
		},
	}

	resp, chosen, err := Execute(context.Background(), NoopThrottle{}, req)
	if err != nil {
		t.Fatalf("Execute should succeed even when shadow fails: %v", err)
	}
	if chosen.ID != "primary-m" || resp.Choices[0].Text != "primary output" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	select {
	case <-shadow.called:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for shadow execution")
	}

	select {
	case <-shadow.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for shadow completion")
	}
}

func TestExecute_ShadowPercentageZero(t *testing.T) {
	mPrimary := model("primary-m", "openai")
	mShadow := model("shadow-m", "deepseek")

	primary := &fakeAdapter{responses: []func() (*llm.Response, error){ok("primary output")}}
	shadow := &shadowCaptureAdapter{
		fakeAdapter: fakeAdapter{responses: []func() (*llm.Response, error){ok("shadow output")}},
		called:      make(chan struct{}, 1),
	}

	req := Request{
		Chain:            []*proxydb.Model{mPrimary},
		KeyName:          "test-key",
		UserID:           "user-1",
		Chat:             llm.NewChatContext(),
		ShadowModel:      mShadow,
		ShadowPercentage: 0,
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) {
			if m.ID == "shadow-m" {
				return shadow, nil
			}
			return primary, nil
		},
	}

	_, _, err := Execute(context.Background(), NoopThrottle{}, req)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	select {
	case <-shadow.called:
		t.Fatal("shadow should not be called when percentage is 0")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestExecute_ShadowDroppedOnCapacity(t *testing.T) {
	for i := 0; i < maxConcurrentShadows; i++ {
		shadowSem <- struct{}{}
	}
	defer func() {
		for i := 0; i < maxConcurrentShadows; i++ {
			<-shadowSem
		}
	}()

	mPrimary := model("primary-m", "openai")
	mShadow := model("shadow-m", "deepseek")

	primary := &fakeAdapter{responses: []func() (*llm.Response, error){ok("primary output")}}
	shadow := &shadowCaptureAdapter{
		fakeAdapter: fakeAdapter{responses: []func() (*llm.Response, error){ok("shadow output")}},
		called:      make(chan struct{}, 1),
	}

	req := Request{
		Chain:            []*proxydb.Model{mPrimary},
		KeyName:          "test-key",
		UserID:           "user-1",
		Chat:             llm.NewChatContext(),
		ShadowModel:      mShadow,
		ShadowPercentage: 100,
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) {
			if m.ID == "shadow-m" {
				return shadow, nil
			}
			return primary, nil
		},
	}

	_, _, err := Execute(context.Background(), NoopThrottle{}, req)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	select {
	case <-shadow.called:
		t.Fatal("shadow should be dropped when semaphore is full")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestExecute_ShadowThrottledRPM(t *testing.T) {
	mPrimary := model("primary-m", "openai")
	mShadow := model("shadow-m", "deepseek")

	primary := &fakeAdapter{responses: []func() (*llm.Response, error){ok("primary output")}}
	shadow := &shadowCaptureAdapter{
		fakeAdapter: fakeAdapter{responses: []func() (*llm.Response, error){ok("shadow output")}},
		called:      make(chan struct{}, 1),
	}

	th := &recordingThrottle{rpmDeny: map[string]bool{"shadow-m": true}}

	req := Request{
		Chain:            []*proxydb.Model{mPrimary},
		KeyName:          "test-key",
		UserID:           "user-1",
		Chat:             llm.NewChatContext(),
		ShadowModel:      mShadow,
		ShadowPercentage: 100,
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) {
			if m.ID == "shadow-m" {
				return shadow, nil
			}
			return primary, nil
		},
	}

	_, _, err := Execute(context.Background(), th, req)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	select {
	case <-shadow.called:
		t.Fatal("shadow should be skipped when shadow model is rpm saturated")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestExecuteStream_ShadowRouting(t *testing.T) {
	mPrimary := model("primary-stream", "openai")
	mShadow := model("shadow-m", "deepseek")

	primaryStream := &fakeStreamAdapter{
		fakeAdapter: fakeAdapter{responses: []func() (*llm.Response, error){ok("stream text")}},
		stream: func(ctx context.Context, chat *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
			if err := onChunk(llm.StreamChunk{Delta: "hello "}); err != nil {
				return err
			}
			return onChunk(llm.StreamChunk{Delta: "world"})
		},
	}
	shadow := &shadowCaptureAdapter{
		fakeAdapter: fakeAdapter{responses: []func() (*llm.Response, error){ok("shadow text")}},
		called:      make(chan struct{}, 1),
		done:        make(chan struct{}, 1),
	}

	adapters := map[string]llm.ClientAdapter{
		"primary-stream": primaryStream,
		"shadow-m":       shadow,
	}

	req := Request{
		Chain:            []*proxydb.Model{mPrimary},
		KeyName:          "test-key",
		UserID:           "user-1",
		Chat:             llm.NewChatContext(),
		ShadowModel:      mShadow,
		ShadowPercentage: 100,
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) {
			return adapters[m.ID], nil
		},
	}

	var chunks []string
	chosen, err := ExecuteStream(context.Background(), NoopThrottle{}, req, func(_ *proxydb.Model, c llm.StreamChunk) error {
		chunks = append(chunks, c.Delta)
		return nil
	})
	if err != nil {
		t.Fatalf("ExecuteStream failed: %v", err)
	}
	if chosen.ID != "primary-stream" {
		t.Errorf("chosen = %q, want primary-stream", chosen.ID)
	}
	if len(chunks) != 2 {
		t.Errorf("chunks len = %d, want 2", len(chunks))
	}

	select {
	case <-shadow.called:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for shadow execution")
	}

	select {
	case <-shadow.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for shadow completion")
	}

	if shadow.capturedChat.GetMeta()["is_shadow"] != "true" {
		t.Errorf("shadow meta is_shadow = %q, want true", shadow.capturedChat.GetMeta()["is_shadow"])
	}
}

func TestExecute_ShadowCircuitOpenSkipped(t *testing.T) {
	mPrimary := model("primary-m", "openai")
	mShadow := model("shadow-m", "deepseek")

	primary := &fakeAdapter{responses: []func() (*llm.Response, error){ok("primary output")}}
	shadow := &shadowCaptureAdapter{
		fakeAdapter: fakeAdapter{responses: []func() (*llm.Response, error){ok("shadow output")}},
		called:      make(chan struct{}, 1),
	}

	cbMgr := circuitbreaker.NewManager(circuitbreaker.Config{Enabled: true, FailThreshold: 1, Cooldown: 10 * time.Minute})
	cbMgr.Report(context.Background(), mShadow.Vendor, mShadow.ID, retryableErr)

	if cbMgr.IsClosed(mShadow.Vendor, mShadow.ID) {
		t.Fatal("expected shadow breaker to be open")
	}

	req := Request{
		Chain:            []*proxydb.Model{mPrimary},
		KeyName:          "test-key",
		UserID:           "user-1",
		Chat:             llm.NewChatContext(),
		ShadowModel:      mShadow,
		ShadowPercentage: 100,
		CircuitBreaker:   cbMgr,
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) {
			if m.ID == "shadow-m" {
				return shadow, nil
			}
			return primary, nil
		},
	}

	_, _, err := Execute(context.Background(), NoopThrottle{}, req)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	select {
	case <-shadow.called:
		t.Fatal("shadow should not be called when circuit breaker is open")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestExecute_ShadowSpendSettled(t *testing.T) {
	mPrimary := model("primary-m", "openai")
	mShadow := model("shadow-m", "deepseek")

	primaryResp := &llm.Response{
		Choices: []*llm.Message{{Text: "primary", Type: llm.MessageTypeBot}},
		Usage:   llm.TokensUsage{Input: 10, Output: 20},
	}
	shadowResp := &llm.Response{
		Choices: []*llm.Message{{Text: "shadow", Type: llm.MessageTypeBot}},
		Usage:   llm.TokensUsage{Input: 15, Output: 25},
	}

	primary := &fakeAdapter{responses: []func() (*llm.Response, error){func() (*llm.Response, error) { return primaryResp, nil }}}
	shadow := &shadowCaptureAdapter{
		fakeAdapter: fakeAdapter{responses: []func() (*llm.Response, error){func() (*llm.Response, error) { return shadowResp, nil }}},
		called:      make(chan struct{}, 1),
		done:        make(chan struct{}, 1),
	}

	th := &recordingThrottle{}

	req := Request{
		Chain:            []*proxydb.Model{mPrimary},
		KeyName:          "test-key",
		UserID:           "user-1",
		Chat:             llm.NewChatContext(),
		ShadowModel:      mShadow,
		ShadowPercentage: 100,
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) {
			if m.ID == "shadow-m" {
				return shadow, nil
			}
			return primary, nil
		},
	}

	_, _, err := Execute(context.Background(), th, req)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	select {
	case <-shadow.done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for shadow completion")
	}

	th.mu.Lock()
	reserves := th.reserves
	settledCount := len(th.settled)
	settled := append([]llm.TokensUsage{}, th.settled...)
	th.mu.Unlock()

	if reserves != 2 {
		t.Fatalf("reserves = %d, want 2", reserves)
	}
	if settledCount != 2 {
		t.Fatalf("settled count = %d, want 2", settledCount)
	}
	foundShadowUsage := false
	for _, u := range settled {
		if u.Input == 15 && u.Output == 25 {
			foundShadowUsage = true
		}
	}
	if !foundShadowUsage {
		t.Fatalf("shadow usage (15, 25) not found in settled: %+v", settled)
	}
}
