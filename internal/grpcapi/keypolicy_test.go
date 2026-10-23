package grpcapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

func TestKeyAllowedModels(t *testing.T) {
	models := []*proxydb.Model{
		{ID: "m1", Vendor: registry.VendorOpenAI, Efforts: []string{registry.EffortMedium}, Enabled: true},
		{ID: "m2", Vendor: registry.VendorOpenAI, Efforts: []string{registry.EffortMedium}, Enabled: true},
	}
	snap := registry.NewSnapshotForTest(models, catchAllRules("m1", "m2"),
		map[string]map[string]string{"key-1": {registry.VendorOpenAI: "sk"}}, nil)
	server := NewProxyServer(fixedSnapshot{snap}, nil)
	req := &pb.GenerateTextRequest{Effort: pb.Effort_EFFORT_MEDIUM,
		Messages: []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "hi"}}}

	key := &apikeys.Identity{KeyID: "key-1", Name: "k", KeyAllowedModels: []string{"m2"}}
	plan, err := server.planRoute(context.Background(), snap, key, req)
	if err != nil || len(plan.models) != 1 || plan.models[0].ID != "m2" {
		t.Fatalf("chain = %v, %v; want only m2", plan, err)
	}

	// Both lists apply: the key allows m2, the team only m1, so nothing is left.
	both := &apikeys.Identity{KeyID: "key-1", Name: "k", KeyAllowedModels: []string{"m2"}, TeamAllowedModels: []string{"m1"}}
	if _, err := server.planRoute(context.Background(), snap, both, req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("key and team lists disjoint: %v, want PermissionDenied", err)
	}

	ctx := apikeys.WithIdentity(context.Background(), key)
	_, err = server.GenerateText(ctx, &pb.GenerateTextRequest{ModelOverride: "m1", Messages: req.Messages})
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), `"m1" not allowed for key`) {
		t.Fatalf("override outside the key's list: %v", err)
	}
	team := &apikeys.Identity{KeyID: "key-1", Name: "k", TeamAllowedModels: []string{"m2"}}
	_, err = server.GenerateText(apikeys.WithIdentity(context.Background(), team), &pb.GenerateTextRequest{ModelOverride: "m1", Messages: req.Messages})
	if !strings.Contains(err.Error(), `"m1" not allowed for team`) {
		t.Fatalf("override outside the team's list: %v", err)
	}
}

type fakeKeyLimiter struct {
	kind     string
	retry    time.Duration
	estimate int64
	settled  []int64
}

func (f *fakeKeyLimiter) AdmitKey(_ context.Context, _ string, _ int32, _, estimate int64) (func(int64), string, time.Duration) {
	f.estimate = estimate
	if f.kind != "" {
		return nil, f.kind, f.retry
	}
	return func(actual int64) { f.settled = append(f.settled, actual) }, "", 0
}

func TestKeyRatesRefuseAndSettle(t *testing.T) {
	vendor := &fakeVendor{reply: "ok"}
	srv := httptest.NewServer(http.HandlerFunc(vendor.handler))
	defer srv.Close()
	models := []*proxydb.Model{testModel("m1", registry.VendorOpenAI, srv.URL, registry.EffortLow)}
	req := &pb.GenerateTextRequest{Effort: pb.Effort_EFFORT_LOW,
		Messages: []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "say hi"}}}
	limited := apikeys.WithIdentity(context.Background(),
		&apikeys.Identity{KeyID: "key1", Name: "generation", Scopes: []string{apikeys.ScopeGenerate}, RPMLimit: 10, TPMLimit: 1000})

	lim := &fakeKeyLimiter{}
	s := newTestServer(t, models, catchAllRules("m1"), nil).WithKeyLimiter(lim)
	if _, err := s.GenerateText(limited, req); err != nil {
		t.Fatal(err)
	}
	if lim.estimate <= 0 || len(lim.settled) != 1 || lim.settled[0] != 18 {
		t.Fatalf("estimate %d, settled %v; want an estimate, then the call's 11+7 tokens", lim.estimate, lim.settled)
	}

	// A key without rates is never sent to the limiter.
	lim.settled = nil
	if _, err := s.GenerateText(testIdentity(), req); err != nil || lim.settled != nil {
		t.Fatalf("unlimited key: %v, settled %v", err, lim.settled)
	}

	refusing := &fakeKeyLimiter{kind: "key_tpm", retry: 1500 * time.Millisecond}
	s = newTestServer(t, models, catchAllRules("m1"), nil).WithKeyLimiter(refusing)
	_, err := s.GenerateText(limited, req)
	st, _ := status.FromError(err)
	if st.Code() != codes.ResourceExhausted || st.Message() != "throttled:key_tpm" {
		t.Fatalf("refused call: %v", err)
	}
	var retry time.Duration
	for _, d := range st.Details() {
		if ri, ok := d.(*errdetails.RetryInfo); ok {
			retry = ri.GetRetryDelay().AsDuration()
		}
	}
	if retry != 1500*time.Millisecond {
		t.Fatalf("RetryInfo = %v, want 1.5s", retry)
	}
	never := &fakeKeyLimiter{kind: "key_tpm"}
	s = newTestServer(t, models, catchAllRules("m1"), nil).WithKeyLimiter(never)
	if _, err := s.GenerateText(limited, req); status.Code(err) != codes.FailedPrecondition ||
		status.Convert(err).Message() != "request_too_large" {
		t.Fatalf("a call that never fits: %v, want FailedPrecondition request_too_large", err)
	}
	if vendor.count() != 2 {
		t.Fatalf("vendor called %d times, want only the two admitted calls", vendor.count())
	}
}

func TestKeyPolicyValidation(t *testing.T) {
	for _, bad := range []*pb.APIKeyPolicy{{RpmLimit: -1}, {TpmLimit: -1}, {AllowedModels: []string{"m1", " "}}} {
		if _, _, _, err := keyPolicy(bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("keyPolicy(%v) = %v, want InvalidArgument", bad, err)
		}
	}
	allowed, rpm, tpm, err := keyPolicy(&pb.APIKeyPolicy{AllowedModels: []string{"m1", " m1 ", "m2"}, RpmLimit: 5, TpmLimit: 9})
	if err != nil || len(allowed) != 2 || rpm != 5 || tpm != 9 {
		t.Fatalf("keyPolicy = %v %d %d %v", allowed, rpm, tpm, err)
	}
	if allowed, _, _, err := keyPolicy(nil); err != nil || allowed == nil || len(allowed) != 0 {
		t.Fatalf("nil policy = %v %v, want no limits", allowed, err)
	}
}
