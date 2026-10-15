package router

import (
	"context"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
)

// spendThrottle refuses the models in deny with a spend reason and records
// the output ceiling each reservation was priced with.
type spendThrottle struct {
	NoopThrottle
	deny       map[string]string
	maxOutputs []int64
}

func (s *spendThrottle) Reserve(_ context.Context, m *proxydb.Model, _, _ string, _, maxOutput int64) (*Reservation, string) {
	s.maxOutputs = append(s.maxOutputs, maxOutput)
	return nil, s.deny[m.ID]
}

func TestExecuteFallsBackToCheaperModelOnSpendLimit(t *testing.T) {
	th := &spendThrottle{deny: map[string]string{"pricey": "spend_key_daily"}}
	cheap := &fakeAdapter{responses: []func() (*llm.Response, error){ok("hi")}}
	chat := llm.NewChatContext()
	chat.SetMaxOutputTokens(777)

	_, used, err := Execute(context.Background(), th, Request{
		Chain: []*proxydb.Model{
			model("pricey", registry.VendorOpenAI, registry.EffortLow),
			model("cheap", registry.VendorOpenAI, registry.EffortLow),
		},
		KeyName: "k", Chat: chat,
		Adapter: func(*proxydb.Model) (llm.ClientAdapter, error) { return cheap, nil },
	})
	if err != nil || used.ID != "cheap" {
		t.Fatalf("used=%v err=%v, want the cheaper model", used, err)
	}
	if len(th.maxOutputs) != 2 || th.maxOutputs[0] != 777 || th.maxOutputs[1] != 777 {
		t.Fatalf("reservations priced with max outputs %v, want the chat ceiling 777", th.maxOutputs)
	}
}

func TestExecuteAllSpendLimitedIsInsufficientQuota(t *testing.T) {
	th := &spendThrottle{deny: map[string]string{"a": "spend_user_monthly"}}
	_, _, err := Execute(context.Background(), th, Request{
		Chain:   []*proxydb.Model{model("a", registry.VendorOpenAI, registry.EffortLow)},
		KeyName: "k", Chat: llm.NewChatContext(),
		Adapter: func(*proxydb.Model) (llm.ClientAdapter, error) { return nil, nil },
	})
	st := status.Convert(ToStatus(err))
	if st.Code() != codes.ResourceExhausted || st.Message() != "throttled:spend_user_monthly" {
		t.Fatalf("status = %v %q", st.Code(), st.Message())
	}
}

func TestThrottledStatusSpendDetails(t *testing.T) {
	now := time.Date(2026, 10, 15, 22, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"spend_key_daily":    2 * time.Hour,
		"spend_user_monthly": time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Sub(now),
	}
	for kind, want := range cases {
		st := status.Convert(throttledStatus(kind, now))
		if st.Code() != codes.ResourceExhausted || st.Message() != "throttled:"+kind {
			t.Fatalf("%s: status = %v %q", kind, st.Code(), st.Message())
		}
		var gotRetry time.Duration
		var gotReason string
		for _, d := range st.Details() {
			switch v := d.(type) {
			case *errdetails.RetryInfo:
				gotRetry = v.GetRetryDelay().AsDuration()
			case *errdetails.ErrorInfo:
				gotReason = v.GetReason()
			}
		}
		if gotRetry != want || gotReason != InsufficientQuota {
			t.Errorf("%s: retry=%s reason=%q, want %s %q", kind, gotRetry, gotReason, want, InsufficientQuota)
		}
	}

	if st := status.Convert(throttledStatus("rpm", now)); len(st.Details()) != 0 {
		t.Errorf("rpm throttles carry no quota details, got %v", st.Details())
	}
	if code := status.Code(throttledStatus("spend_unavailable", now)); code != codes.Unavailable {
		t.Errorf("spend_unavailable = %v, want Unavailable", code)
	}
}
