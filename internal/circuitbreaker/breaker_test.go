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
		{"HTTP 429 rate limit", customHTTPStatusError{status: 429}, true},
		{"HTTP 500 internal error", customHTTPStatusError{status: 500}, true},
		{"HTTP 502 bad gateway", customHTTPStatusError{status: 502}, true},
		{"HTTP 503 unavailable", customHTTPStatusError{status: 503}, true},
		{"HTTP 504 gateway timeout", customHTTPStatusError{status: 504}, true},
		{"OpenAI Error 500", &openai.Error{StatusCode: 500}, true},
		{"OpenAI Error 400", &openai.Error{StatusCode: 400}, false},
		{"Network timeout", &net.DNSError{IsTimeout: true}, true},
		{"Connection refused syscall", syscall.ECONNREFUSED, true},
		{"Connection reset string", errors.New("read tcp 127.0.0.1: connection reset by peer"), true},
		{"Deadline exceeded string", errors.New("upstream deadline exceeded"), true},
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
