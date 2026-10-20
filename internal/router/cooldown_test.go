package router

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

// fakeCooldown is a clock-driven pause table keyed by model id.
type fakeCooldown struct {
	now   time.Time
	until map[string]time.Time
}

func newFakeCooldown() *fakeCooldown {
	return &fakeCooldown{now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), until: map[string]time.Time{}}
}

func (f *fakeCooldown) Remaining(m *proxydb.Model) time.Duration {
	if d := f.until[m.ID].Sub(f.now); d > 0 {
		return d
	}
	return 0
}

func (f *fakeCooldown) Pause(m *proxydb.Model, d time.Duration) time.Duration {
	f.until[m.ID] = f.now.Add(d)
	return d
}

func vendor429(retryAfter string) error {
	h := http.Header{}
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	return &openai.Error{
		StatusCode: http.StatusTooManyRequests,
		Request:    httptest.NewRequest(http.MethodPost, "https://api.example.com/v1/chat/completions", nil),
		Response:   &http.Response{StatusCode: http.StatusTooManyRequests, Header: h},
	}
}

func cooldownChain() []*proxydb.Model {
	return []*proxydb.Model{model("primary", registry.VendorOpenAI), model("backup", registry.VendorAnthropic)}
}

func TestExecuteRetryAfterPausesAndSkips(t *testing.T) {
	limited := &fakeAdapter{responses: []func() (*llm.Response, error){fail(vendor429("5"))}}
	backup := &fakeAdapter{responses: []func() (*llm.Response, error){ok("from backup")}}
	adapters := map[string]llm.ClientAdapter{"primary": limited, "backup": backup}
	cd := newFakeCooldown()
	req := Request{
		Chain:    cooldownChain(),
		Chat:     llm.NewChatContext(),
		Cooldown: cd,
		Adapter:  func(m *proxydb.Model) (llm.ClientAdapter, error) { return adapters[m.ID], nil },
	}

	_, m, err := Execute(context.Background(), NoopThrottle{}, req)
	if err != nil || m.ID != "backup" {
		t.Fatalf("first call: m=%v err=%v, want backup", m, err)
	}
	if limited.calls != 1 {
		t.Fatalf("429 with Retry-After was retried: %d calls, want 1", limited.calls)
	}
	if got := cd.Remaining(req.Chain[0]); got != 5*time.Second {
		t.Fatalf("pause = %v, want 5s", got)
	}

	// Inside the window the paused model is not called at all.
	if _, m, err = Execute(context.Background(), NoopThrottle{}, req); err != nil || m.ID != "backup" {
		t.Fatalf("second call: m=%v err=%v", m, err)
	}
	if limited.calls != 1 {
		t.Fatalf("paused model was called: %d calls", limited.calls)
	}

	// Once the window passes the model is tried again.
	cd.now = cd.now.Add(5 * time.Second)
	adapters["primary"] = &fakeAdapter{responses: []func() (*llm.Response, error){ok("primary back")}}
	if _, m, err = Execute(context.Background(), NoopThrottle{}, req); err != nil || m.ID != "primary" {
		t.Fatalf("after pause: m=%v err=%v, want primary", m, err)
	}
}

func TestExecute429WithoutRetryAfterKeepsSameModelRetry(t *testing.T) {
	limited := &fakeAdapter{responses: []func() (*llm.Response, error){fail(vendor429("")), ok("second try")}}
	cd := newFakeCooldown()
	_, m, err := Execute(context.Background(), NoopThrottle{}, Request{
		Chain:    cooldownChain()[:1],
		Chat:     llm.NewChatContext(),
		Cooldown: cd,
		Adapter:  func(*proxydb.Model) (llm.ClientAdapter, error) { return limited, nil },
	})
	if err != nil || m.ID != "primary" || limited.calls != 2 {
		t.Fatalf("m=%v err=%v calls=%d, want a same-model retry", m, err, limited.calls)
	}
	if len(cd.until) != 0 {
		t.Fatalf("pause started without Retry-After: %v", cd.until)
	}
}

func TestExecuteAllPausedReturnsRetryInfo(t *testing.T) {
	cd := newFakeCooldown()
	chain := cooldownChain()
	cd.Pause(chain[0], 3*time.Second)
	cd.Pause(chain[1], 7*time.Second)
	called := false
	_, _, err := Execute(context.Background(), NoopThrottle{}, Request{
		Chain:    chain,
		Chat:     llm.NewChatContext(),
		Cooldown: cd,
		Adapter: func(*proxydb.Model) (llm.ClientAdapter, error) {
			called = true
			return nil, errors.New("must not resolve an adapter")
		},
	})
	if called {
		t.Fatal("adapter resolved for a paused model")
	}
	st := status.Convert(ToStatus(err))
	if st.Code() != codes.Unavailable || st.Message() != "vendors_unavailable" {
		t.Fatalf("status = %v %q, want Unavailable vendors_unavailable", st.Code(), st.Message())
	}
	var delay time.Duration
	for _, d := range st.Details() {
		if ri, ok := d.(*errdetails.RetryInfo); ok {
			delay = ri.GetRetryDelay().AsDuration()
		}
	}
	if delay != 3*time.Second {
		t.Fatalf("RetryInfo delay = %v, want the soonest pause, 3s", delay)
	}
}

func TestExecutePauseWithOpenCircuitPromisesNoDelay(t *testing.T) {
	cd := newFakeCooldown()
	chain := cooldownChain()
	cd.Pause(chain[0], time.Minute)
	_, _, err := Execute(context.Background(), NoopThrottle{}, Request{
		Chain:          chain,
		Chat:           llm.NewChatContext(),
		Cooldown:       cd,
		CircuitBreaker: openBreaker{},
		Adapter:        func(*proxydb.Model) (llm.ClientAdapter, error) { return nil, errors.New("unused") },
	})
	st := status.Convert(ToStatus(err))
	if st.Code() != codes.Unavailable {
		t.Fatalf("status = %v, want Unavailable", st.Code())
	}
	for _, d := range st.Details() {
		if _, ok := d.(*errdetails.RetryInfo); ok {
			t.Fatal("an open circuit may recover before the pause ends; no RetryInfo expected")
		}
	}
}

// openBreaker rejects every model.
type openBreaker struct{}

func (openBreaker) Allow(string, string) bool                     { return false }
func (openBreaker) Report(context.Context, string, string, error) {}
func (openBreaker) Release(string, string)                        {}
func (openBreaker) IsClosed(string, string) bool                  { return false }

func vendor429Ms(ms string) error {
	err := vendor429("").(*openai.Error)
	err.Response.Header.Set("Retry-After-Ms", ms)
	return err
}

func TestExecuteShortRetryAfterIsWaitedInPlace(t *testing.T) {
	limited := &fakeAdapter{responses: []func() (*llm.Response, error){fail(vendor429Ms("20")), ok("after the wait")}}
	cd := newFakeCooldown()
	start := time.Now()
	_, m, err := Execute(context.Background(), NoopThrottle{}, Request{
		Chain:    cooldownChain()[:1],
		Chat:     llm.NewChatContext(),
		Cooldown: cd,
		Adapter:  func(*proxydb.Model) (llm.ClientAdapter, error) { return limited, nil },
	})
	if err != nil || m.ID != "primary" || limited.calls != 2 {
		t.Fatalf("m=%v err=%v calls=%d, want the same model retried", m, err, limited.calls)
	}
	if waited := time.Since(start); waited < 20*time.Millisecond || waited >= attemptBackoff {
		t.Fatalf("waited %v, want the vendor's 20ms instead of the fixed backoff", waited)
	}
	if len(cd.until) != 0 {
		t.Fatalf("a short wait that was honoured in place must not pause: %v", cd.until)
	}
}

type resetErr struct{}

func (resetErr) Error() string   { return "read: connection reset by peer" }
func (resetErr) Timeout() bool   { return false }
func (resetErr) Temporary() bool { return true }

func TestExecuteTransportFailureFallsBack(t *testing.T) {
	broken := &fakeAdapter{responses: []func() (*llm.Response, error){fail(resetErr{})}}
	backup := &fakeAdapter{responses: []func() (*llm.Response, error){ok("from backup")}}
	adapters := map[string]llm.ClientAdapter{"primary": broken, "backup": backup}
	_, m, err := Execute(context.Background(), NoopThrottle{}, Request{
		Chain:   cooldownChain(),
		Chat:    llm.NewChatContext(),
		Adapter: func(m *proxydb.Model) (llm.ClientAdapter, error) { return adapters[m.ID], nil },
	})
	if err != nil || m.ID != "backup" {
		t.Fatalf("m=%v err=%v: a reset connection must fall back, not end the chain", m, err)
	}
	if broken.calls != 2 {
		t.Fatalf("reset retried %d times on the same model, want 2 attempts", broken.calls)
	}
}

func TestExecuteStreamRetriesBeforeFirstChunk(t *testing.T) {
	calls := 0
	flaky := &fakeStreamAdapter{stream: func(_ context.Context, _ *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
		calls++
		if calls == 1 {
			return vendor500()
		}
		return onChunk(llm.StreamChunk{Delta: "ok", Usage: &llm.TokensUsage{Input: 1, Output: 1}})
	}}
	m, err := ExecuteStream(context.Background(), NoopThrottle{}, Request{
		Chain:   cooldownChain()[:1],
		Chat:    llm.NewChatContext(),
		Adapter: func(*proxydb.Model) (llm.ClientAdapter, error) { return flaky, nil },
	}, func(*proxydb.Model, llm.StreamChunk) error { return nil })
	if err != nil || m.ID != "primary" || calls != 2 {
		t.Fatalf("m=%v err=%v calls=%d, want one same-model retry", m, err, calls)
	}
}

func TestExecuteStreamDoesNotRetryAfterFirstChunk(t *testing.T) {
	calls := 0
	dropping := &fakeStreamAdapter{stream: func(_ context.Context, _ *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
		calls++
		if err := onChunk(llm.StreamChunk{Delta: "partial"}); err != nil {
			return err
		}
		return vendor500()
	}}
	_, err := ExecuteStream(context.Background(), NoopThrottle{}, Request{
		Chain:   cooldownChain()[:1],
		Chat:    llm.NewChatContext(),
		Adapter: func(*proxydb.Model) (llm.ClientAdapter, error) { return dropping, nil },
	}, func(*proxydb.Model, llm.StreamChunk) error { return nil })
	if err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d: a stream that already sent text must not be replayed", err, calls)
	}
}

func TestExecuteMediaRetriesSameModel(t *testing.T) {
	calls := 0
	m, err := ExecuteMedia(context.Background(), NoopThrottle{}, MediaRequest{
		Chain:   cooldownChain()[:1],
		Adapter: func(*proxydb.Model) (llm.ClientAdapter, error) { return &fakeAdapter{}, nil },
		Call: func(context.Context, *proxydb.Model, llm.ClientAdapter) (llm.TokensUsage, error) {
			calls++
			if calls == 1 {
				return llm.TokensUsage{}, vendor500()
			}
			return llm.TokensUsage{Input: 1}, nil
		},
	})
	if err != nil || m.ID != "primary" || calls != 2 {
		t.Fatalf("m=%v err=%v calls=%d, want one same-model retry", m, err, calls)
	}
}

func TestExecuteStreamRetryAfterFallsBack(t *testing.T) {
	limited := &fakeStreamAdapter{stream: func(context.Context, *llm.ChatContext, func(llm.StreamChunk) error) error {
		return vendor429("2")
	}}
	backup := &fakeStreamAdapter{stream: func(_ context.Context, _ *llm.ChatContext, onChunk func(llm.StreamChunk) error) error {
		return onChunk(llm.StreamChunk{Delta: "ok", Usage: &llm.TokensUsage{Input: 1, Output: 1}})
	}}
	adapters := map[string]llm.ClientAdapter{"primary": limited, "backup": backup}
	cd := newFakeCooldown()
	m, err := ExecuteStream(context.Background(), NoopThrottle{}, Request{
		Chain:    cooldownChain(),
		Chat:     llm.NewChatContext(),
		Cooldown: cd,
		Adapter:  func(m *proxydb.Model) (llm.ClientAdapter, error) { return adapters[m.ID], nil },
	}, func(*proxydb.Model, llm.StreamChunk) error { return nil })
	if err != nil || m.ID != "backup" {
		t.Fatalf("m=%v err=%v, want backup", m, err)
	}
	if got := cd.Remaining(cooldownChain()[0]); got != 2*time.Second {
		t.Fatalf("pause = %v, want 2s", got)
	}
}

func TestExecuteMediaRetryAfterPauses(t *testing.T) {
	cd := newFakeCooldown()
	calls := map[string]int{}
	m, err := ExecuteMedia(context.Background(), NoopThrottle{}, MediaRequest{
		Chain:    cooldownChain(),
		Cooldown: cd,
		Adapter:  func(*proxydb.Model) (llm.ClientAdapter, error) { return &fakeAdapter{}, nil },
		Call: func(_ context.Context, m *proxydb.Model, _ llm.ClientAdapter) (llm.TokensUsage, error) {
			calls[m.ID]++
			if m.ID == "primary" {
				return llm.TokensUsage{}, vendor429("4")
			}
			return llm.TokensUsage{Input: 1}, nil
		},
	})
	if err != nil || m.ID != "backup" || calls["primary"] != 1 {
		t.Fatalf("m=%v err=%v calls=%v", m, err, calls)
	}
	if got := cd.Remaining(cooldownChain()[0]); got != 4*time.Second {
		t.Fatalf("pause = %v, want 4s", got)
	}
}
