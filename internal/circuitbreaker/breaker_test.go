package circuitbreaker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
)

type customHTTPStatusError struct {
	status int
}

func (e customHTTPStatusError) Error() string {
	return fmt.Sprintf("http status %d", e.status)
}

func (e customHTTPStatusError) HTTPStatus() int {
	return e.status
}

type mockBedrockError struct {
	code   string
	status int
}

func (m mockBedrockError) Error() string       { return m.code }
func (m mockBedrockError) ErrorCode() string   { return m.code }
func (m mockBedrockError) HTTPStatusCode() int { return m.status }

func TestCircuitBreakerStateTransitions(t *testing.T) {
	currentTime := time.Now()
	nowMock := func() time.Time { return currentTime }

	cfg := Config{
		Enabled:       true,
		FailThreshold: 3,
		Cooldown:      10 * time.Second,
	}

	transitions := make([]string, 0)
	mgr := NewManager(cfg)
	mgr.nowFunc = nowMock
	mgr.onStateChange = func(vendor, model string, from, to State) {
		if from != to {
			transitions = append(transitions, fmt.Sprintf("%s->%s", from, to))
		}
	}

	vendor := "openai"
	model := "gpt-4o"
	ctx := context.Background()

	// Initially CLOSED
	if !mgr.Allow(vendor, model) {
		t.Fatal("expected Allow to be true initially")
	}
	if mgr.State(vendor, model) != StateClosed {
		t.Fatalf("expected StateClosed, got %v", mgr.State(vendor, model))
	}

	// 2 failures should remain CLOSED
	mgr.Report(ctx, vendor, model, customHTTPStatusError{status: 500})
	mgr.Report(ctx, vendor, model, customHTTPStatusError{status: 502})
	if mgr.State(vendor, model) != StateClosed {
		t.Fatalf("expected StateClosed after 2 failures, got %v", mgr.State(vendor, model))
	}
	if !mgr.Allow(vendor, model) {
		t.Fatal("expected Allow to be true after 2 failures")
	}

	// 3rd failure trips to OPEN
	mgr.Report(ctx, vendor, model, customHTTPStatusError{status: 503})
	if mgr.State(vendor, model) != StateOpen {
		t.Fatalf("expected StateOpen after 3 failures, got %v", mgr.State(vendor, model))
	}
	if mgr.Allow(vendor, model) {
		t.Fatal("expected Allow to be false when OPEN")
	}

	// Before cooldown: still OPEN and disallowed
	currentTime = currentTime.Add(5 * time.Second)
	if mgr.Allow(vendor, model) {
		t.Fatal("expected Allow to remain false before cooldown expires")
	}

	// After cooldown: first request transitions to HALF_OPEN and is allowed as probe
	currentTime = currentTime.Add(6 * time.Second) // total 11s elapsed
	if !mgr.Allow(vendor, model) {
		t.Fatal("expected probe request to be allowed after cooldown")
	}
	if mgr.State(vendor, model) != StateHalfOpen {
		t.Fatalf("expected StateHalfOpen during probe, got %v", mgr.State(vendor, model))
	}

	// While probe is in progress, subsequent requests are rejected
	if mgr.Allow(vendor, model) {
		t.Fatal("expected subsequent requests to be rejected while probe is active")
	}

	// Probe succeeds: transitions to CLOSED
	mgr.Report(ctx, vendor, model, nil)
	if mgr.State(vendor, model) != StateClosed {
		t.Fatalf("expected StateClosed after probe success, got %v", mgr.State(vendor, model))
	}
	if !mgr.Allow(vendor, model) {
		t.Fatal("expected Allow to be true after recovering to CLOSED")
	}

	expectedTransitions := []string{"CLOSED->OPEN", "OPEN->HALF_OPEN", "HALF_OPEN->CLOSED"}
	if len(transitions) != len(expectedTransitions) {
		t.Fatalf("unexpected transitions count: got %v, want %v", transitions, expectedTransitions)
	}
	for i, want := range expectedTransitions {
		if transitions[i] != want {
			t.Errorf("transition %d: got %s, want %s", i, transitions[i], want)
		}
	}
}

func TestProbeFailureReturnsToOpen(t *testing.T) {
	currentTime := time.Now()
	nowMock := func() time.Time { return currentTime }

	cfg := Config{
		Enabled:       true,
		FailThreshold: 2,
		Cooldown:      10 * time.Second,
	}

	mgr := NewManager(cfg)
	mgr.nowFunc = nowMock
	mgr.onStateChange = nil

	vendor := "anthropic"
	model := "claude-3-opus"
	ctx := context.Background()

	mgr.Report(ctx, vendor, model, customHTTPStatusError{status: 500})
	mgr.Report(ctx, vendor, model, customHTTPStatusError{status: 500})
	if mgr.State(vendor, model) != StateOpen {
		t.Fatalf("expected OPEN state, got %v", mgr.State(vendor, model))
	}

	currentTime = currentTime.Add(11 * time.Second)
	if !mgr.Allow(vendor, model) {
		t.Fatal("expected probe to be admitted")
	}
	if mgr.State(vendor, model) != StateHalfOpen {
		t.Fatalf("expected HALF_OPEN state, got %v", mgr.State(vendor, model))
	}

	// Probe encounters timeout failure
	mgr.Report(ctx, vendor, model, &net.DNSError{IsTimeout: true})
	if mgr.State(vendor, model) != StateOpen {
		t.Fatalf("expected state to return to OPEN after failed probe, got %v", mgr.State(vendor, model))
	}
	if mgr.Allow(vendor, model) {
		t.Fatal("expected requests to be blocked after probe failed")
	}
}

func TestClientCancellationDoesNotTripBreaker(t *testing.T) {
	cfg := Config{Enabled: true, FailThreshold: 2, Cooldown: 10 * time.Second}
	mgr := NewManager(cfg)
	mgr.onStateChange = nil

	vendor := "openai"
	model := "gpt-4o"

	// Canceled context
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	for i := 0; i < 5; i++ {
		mgr.Report(canceledCtx, vendor, model, context.Canceled)
	}

	if mgr.State(vendor, model) != StateClosed {
		t.Fatalf("client cancellations should not trip breaker, got state %v", mgr.State(vendor, model))
	}
	if !mgr.Allow(vendor, model) {
		t.Fatal("expected Allow to remain true despite client cancellations")
	}
}

func Test400BadRequestDoesNotTripBreaker(t *testing.T) {
	cfg := Config{Enabled: true, FailThreshold: 2, Cooldown: 10 * time.Second}
	mgr := NewManager(cfg)
	mgr.onStateChange = nil

	vendor := "openai"
	model := "gpt-4o"
	ctx := context.Background()

	// 400 Bad Request error
	badReqErr := customHTTPStatusError{status: 400}
	for i := 0; i < 5; i++ {
		mgr.Report(ctx, vendor, model, badReqErr)
	}

	if mgr.State(vendor, model) != StateClosed {
		t.Fatalf("400 Bad Request should not trip breaker, got state %v", mgr.State(vendor, model))
	}

	// 401 Unauthorized error
	unauthErr := customHTTPStatusError{status: 401}
	for i := 0; i < 5; i++ {
		mgr.Report(ctx, vendor, model, unauthErr)
	}
	if mgr.State(vendor, model) != StateClosed {
		t.Fatalf("401 Unauthorized should not trip breaker, got state %v", mgr.State(vendor, model))
	}
}

func TestProbeCancellationKeepsHalfOpen(t *testing.T) {
	currentTime := time.Now()
	nowMock := func() time.Time { return currentTime }

	cfg := Config{Enabled: true, FailThreshold: 1, Cooldown: 5 * time.Second}
	mgr := NewManager(cfg)
	mgr.nowFunc = nowMock
	mgr.onStateChange = nil

	vendor := "google"
	model := "gemini-1.5-pro"
	ctx := context.Background()

	mgr.Report(ctx, vendor, model, customHTTPStatusError{status: 503})
	if mgr.State(vendor, model) != StateOpen {
		t.Fatalf("expected OPEN state, got %v", mgr.State(vendor, model))
	}

	currentTime = currentTime.Add(6 * time.Second)
	if !mgr.Allow(vendor, model) {
		t.Fatal("expected probe to be admitted")
	}
	if mgr.State(vendor, model) != StateHalfOpen {
		t.Fatalf("expected HALF_OPEN state, got %v", mgr.State(vendor, model))
	}

	// Probe request cancelled by client
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	mgr.Report(canceledCtx, vendor, model, context.Canceled)

	// Breaker should remain HALF_OPEN and allow a new probe
	if mgr.State(vendor, model) != StateHalfOpen {
		t.Fatalf("expected state to stay HALF_OPEN, got %v", mgr.State(vendor, model))
	}
	if !mgr.Allow(vendor, model) {
		t.Fatal("expected new probe to be admitted after previous probe cancelled")
	}
}

func TestNonTrippingErrorDoesNotCloseHalfOpen(t *testing.T) {
	currentTime := time.Now()
	nowMock := func() time.Time { return currentTime }

	cfg := Config{Enabled: true, FailThreshold: 1, Cooldown: 5 * time.Second}
	mgr := NewManager(cfg)
	mgr.nowFunc = nowMock

	vendor := "google"
	model := "gemini-1.5-pro"
	ctx := context.Background()

	mgr.Report(ctx, vendor, model, customHTTPStatusError{status: 503})
	if mgr.State(vendor, model) != StateOpen {
		t.Fatalf("expected OPEN state, got %v", mgr.State(vendor, model))
	}

	currentTime = currentTime.Add(6 * time.Second)
	if !mgr.Allow(vendor, model) {
		t.Fatal("expected probe to be admitted")
	}
	if mgr.State(vendor, model) != StateHalfOpen {
		t.Fatalf("expected HALF_OPEN state, got %v", mgr.State(vendor, model))
	}

	// 400 Bad Request or capability mismatch (e.g. ErrJudgmentUnsupported) during probe
	mgr.Report(ctx, vendor, model, customHTTPStatusError{status: 400})

	// Must remain HALF_OPEN (not prematurely marked healthy/CLOSED)
	if mgr.State(vendor, model) != StateHalfOpen {
		t.Fatalf("expected state to remain HALF_OPEN after non-tripping error, got %v", mgr.State(vendor, model))
	}
	// Probe slot must be released so a subsequent valid request can probe
	if !mgr.Allow(vendor, model) {
		t.Fatal("expected new probe to be admitted after non-tripping error released slot")
	}
}

func TestTrippingErrorClassification(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name     string
		err      error
		expected bool
	}{
		{"nil error", nil, false},
		{"context.Canceled", context.Canceled, false},
		{"HTTP 400", customHTTPStatusError{status: 400}, false},
		{"HTTP 401", customHTTPStatusError{status: 401}, false},
		{"HTTP 403", customHTTPStatusError{status: 403}, false},
		{"HTTP 404", customHTTPStatusError{status: 404}, false},
		{"HTTP 422", customHTTPStatusError{status: 422}, false},
		{"HTTP 429 rate limit (isolated to tenant)", customHTTPStatusError{status: 429}, false},
		{"HTTP 500 internal error", customHTTPStatusError{status: 500}, true},
		{"HTTP 502 bad gateway", customHTTPStatusError{status: 502}, true},
		{"HTTP 503 unavailable", customHTTPStatusError{status: 503}, true},
		{"HTTP 504 gateway timeout", customHTTPStatusError{status: 504}, true},
		{"OpenAI Error 500", &openai.Error{StatusCode: 500}, true},
		{"OpenAI Error 429", &openai.Error{StatusCode: 429}, false},
		{"OpenAI Error 400", &openai.Error{StatusCode: 400}, false},
		{"Network timeout", &net.DNSError{IsTimeout: true}, true},
		{"Connection refused syscall", syscall.ECONNREFUSED, true},
		{"Connection reset string", errors.New("read tcp 127.0.0.1: connection reset by peer"), true},
		{"TLS handshake timeout", errors.New("net/http: TLS handshake timeout"), true},
		{"i/o timeout", errors.New("read tcp 127.0.0.1:443: i/o timeout"), true},
		{"Broken pipe", errors.New("write: broken pipe"), true},
		{"Bedrock ModelTimeoutException 408", mockBedrockError{code: "ModelTimeoutException", status: 408}, true},
		{"Bedrock ModelErrorException 424", mockBedrockError{code: "ModelErrorException", status: 424}, true},
		{"Bedrock ThrottlingException 429", mockBedrockError{code: "ThrottlingException", status: 429}, false},
		{"Bedrock ValidationException 400", mockBedrockError{code: "ValidationException", status: 400}, false},
		{"ErrUpstreamTimeout sentinel", ErrUpstreamTimeout, true},
		{"UpstreamTimeoutError struct", NewUpstreamTimeoutError(5*time.Second, context.DeadlineExceeded), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsTrippingError(ctx, tc.err)
			if got != tc.expected {
				t.Errorf("IsTrippingError(%v) = %v, want %v", tc.err, got, tc.expected)
			}
		})
	}
}

func TestReleaseAllowsNextProbe(t *testing.T) {
	currentTime := time.Now()
	nowMock := func() time.Time { return currentTime }

	cfg := Config{Enabled: true, FailThreshold: 1, Cooldown: 5 * time.Second}
	mgr := NewManager(cfg)
	mgr.nowFunc = nowMock

	vendor := "openai"
	model := "gpt-4o"
	ctx := context.Background()

	// Trip to OPEN
	mgr.Report(ctx, vendor, model, customHTTPStatusError{status: 500})
	if mgr.State(vendor, model) != StateOpen {
		t.Fatalf("expected OPEN state, got %v", mgr.State(vendor, model))
	}

	currentTime = currentTime.Add(6 * time.Second)
	// Allow transitions to HALF_OPEN and acquires probe lock
	if !mgr.Allow(vendor, model) {
		t.Fatal("expected probe to be admitted")
	}
	if mgr.State(vendor, model) != StateHalfOpen {
		t.Fatalf("expected HALF_OPEN, got %v", mgr.State(vendor, model))
	}

	// While probe is in progress, next Allow fails
	if mgr.Allow(vendor, model) {
		t.Fatal("expected second probe to be rejected while first is in progress")
	}

	// Request is canceled or skipped before upstream call -> Release called
	mgr.Release(vendor, model)

	// Now probe lock is freed, next Allow must succeed
	if !mgr.Allow(vendor, model) {
		t.Fatal("expected probe to be admitted after Release")
	}
}

func TestCallerDeadlineDoesNotTripBreaker(t *testing.T) {
	cfg := Config{Enabled: true, FailThreshold: 2, Cooldown: 10 * time.Second}
	mgr := NewManager(cfg)

	vendor := "openai"
	model := "gpt-4o"

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-1*time.Second))
	defer cancel()

	for i := 0; i < 5; i++ {
		mgr.Report(ctx, vendor, model, context.DeadlineExceeded)
	}

	if mgr.State(vendor, model) != StateClosed {
		t.Fatalf("caller deadline exceeded should not trip breaker, got state %v", mgr.State(vendor, model))
	}
	if !mgr.Allow(vendor, model) {
		t.Fatal("expected Allow to remain true despite caller deadlines")
	}
}

func TestUpstreamTimeoutTripsBreaker(t *testing.T) {
	cfg := Config{Enabled: true, FailThreshold: 2, Cooldown: 10 * time.Second}
	mgr := NewManager(cfg)

	vendor := "anthropic"
	model := "claude-3-7-sonnet"
	ctx := context.Background()

	timeoutErr := NewUpstreamTimeoutError(5*time.Second, context.DeadlineExceeded)

	mgr.Report(ctx, vendor, model, timeoutErr)
	if mgr.State(vendor, model) != StateClosed {
		t.Fatalf("expected CLOSED after 1 failure, got %v", mgr.State(vendor, model))
	}

	mgr.Report(ctx, vendor, model, timeoutErr)
	if mgr.State(vendor, model) != StateOpen {
		t.Fatalf("expected OPEN after 2 upstream timeouts, got %v", mgr.State(vendor, model))
	}
	if mgr.Allow(vendor, model) {
		t.Fatal("expected Allow to be false after breaker tripped to OPEN")
	}
}

func TestUpstreamTimeoutWithExpiredCallerContextTripsBreaker(t *testing.T) {
	cfg := Config{Enabled: true, FailThreshold: 2, Cooldown: 10 * time.Second}
	mgr := NewManager(cfg)

	vendor := "google"
	model := "gemini-2.5-pro"

	// Caller context deadline is expired, but failure is caused by upstream timeout watchdog.
	expiredCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-1*time.Second))
	defer cancel()

	timeoutErr := NewUpstreamTimeoutError(10*time.Second, context.DeadlineExceeded)

	mgr.Report(expiredCtx, vendor, model, timeoutErr)
	mgr.Report(expiredCtx, vendor, model, timeoutErr)

	if mgr.State(vendor, model) != StateOpen {
		t.Fatalf("upstream timeout with expired caller context should trip breaker, got state %v", mgr.State(vendor, model))
	}
	if mgr.Allow(vendor, model) {
		t.Fatal("expected Allow to be false after breaker tripped to OPEN")
	}
}

func TestConcurrentBreakerAccess(t *testing.T) {
	cfg := Config{Enabled: true, FailThreshold: 10, Cooldown: 100 * time.Millisecond}
	mgr := NewManager(cfg)

	vendor := "openai"
	model := "gpt-4o"
	ctx := context.Background()

	var wg sync.WaitGroup
	workers := 20
	iterations := 200

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				allowed := mgr.Allow(vendor, model)
				if allowed {
					if (workerID+j)%7 == 0 {
						mgr.Report(ctx, vendor, model, customHTTPStatusError{status: 500})
					} else {
						mgr.Report(ctx, vendor, model, nil)
					}
				}
				_ = mgr.State(vendor, model)
			}
		}(i)
	}

	wg.Wait()
}

func TestManagerIsClosed(t *testing.T) {
	cfg := Config{Enabled: true, FailThreshold: 1, Cooldown: 10 * time.Second}
	mgr := NewManager(cfg)
	if !mgr.IsClosed("openai", "gpt-4o") {
		t.Fatal("expected IsClosed to be true initially")
	}
	mgr.Report(context.Background(), "openai", "gpt-4o", customHTTPStatusError{status: 500})
	if mgr.IsClosed("openai", "gpt-4o") {
		t.Fatal("expected IsClosed to be false after error")
	}
}
