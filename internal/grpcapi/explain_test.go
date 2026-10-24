package grpcapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/circuitbreaker"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/router"
)

// breakerOpenFor reports one model's circuit open, as after its failures.
type breakerOpenFor string

func (b breakerOpenFor) Allow(_, model string) bool                  { return model != string(b) }
func (breakerOpenFor) Report(context.Context, string, string, error) {}
func (breakerOpenFor) Release(string, string)                        {}
func (b breakerOpenFor) IsClosed(_, model string) bool               { return model != string(b) }

// ExplainRoute must name the rule and the model a real call ends up with, or
// the error it gets, across the filters planRoute and Execute apply.
func TestExplainRouteMatchesGenerateText(t *testing.T) {
	vendor := &fakeVendor{reply: "ok"}
	srv := httptest.NewServer(http.HandlerFunc(vendor.handler))
	defer srv.Close()
	all := []string{registry.EffortLow, registry.EffortMedium, registry.EffortHigh}
	models := []*proxydb.Model{
		testModel("cheap", registry.VendorOpenAI, srv.URL, all...),
		testModel("premium", registry.VendorDeepSeek, srv.URL, registry.EffortHigh),
		testModel("claude", registry.VendorAnthropic, srv.URL, all...), // key1 holds no anthropic key
		testModel("node", registry.VendorOllama, srv.URL, all...),
		{ID: "searcher", Vendor: registry.VendorOpenAI, Endpoint: srv.URL, Efforts: all, Capabilities: []string{registry.CapabilityGoogleSearch}, Enabled: true},
	}
	rules := []*proxydb.Rule{
		{Name: "pro", Priority: 30, Attributes: map[string]string{"tier": "pro"}, Enabled: true, Use: []string{"premium", "cheap"}},
		{Name: "high", Priority: 20, Effort: registry.EffortHigh, Attributes: map[string]string{}, Enabled: true, Use: []string{"claude", "premium", "cheap"}},
		{Name: "search", Priority: 15, Attributes: map[string]string{"task": "search"}, Enabled: true, Use: []string{"searcher", "cheap"}},
		{Name: "local", Priority: 10, Attributes: map[string]string{"where": "local"}, Enabled: true, Use: []string{"cheap", "node"}},
		{Name: "default", Priority: 1, Attributes: map[string]string{}, Enabled: true, Use: []string{"cheap", "premium"}},
	}
	keys := map[string]map[string]string{"key1": {
		registry.VendorOpenAI: "sk-test", registry.VendorDeepSeek: "sk-deep", registry.VendorOllama: "none",
	}}
	plain := &apikeys.Identity{KeyID: "key1", Name: "app", Scopes: []string{apikeys.ScopeGenerate}}
	gen := []string{apikeys.ScopeGenerate}
	onlyCheap := &apikeys.Identity{KeyID: "key1", Name: "app", Scopes: gen, KeyAllowedModels: []string{"cheap"}}
	team := &apikeys.Identity{KeyID: "key1", Name: "app", Scopes: gen, TeamID: "t1", TeamAllowedModels: []string{"nothing"}}

	cases := []struct {
		name    string
		id      *apikeys.Identity
		req     *pb.ExplainRouteRequest
		breaker router.CircuitBreaker
		dropped map[string]string
	}{
		{name: "default", id: plain, req: &pb.ExplainRouteRequest{Effort: pb.Effort_EFFORT_LOW}, dropped: map[string]string{"premium": router.DropEffort}},
		{name: "attribute", id: plain, req: &pb.ExplainRouteRequest{Effort: pb.Effort_EFFORT_HIGH, Attributes: map[string]string{"tier": "pro"}}},
		{name: "no vendor key", id: plain, req: &pb.ExplainRouteRequest{Effort: pb.Effort_EFFORT_HIGH}, dropped: map[string]string{"claude": router.DropNoVendorKey}},
		{name: "key policy", id: onlyCheap, req: &pb.ExplainRouteRequest{Effort: pb.Effort_EFFORT_HIGH}, dropped: map[string]string{"premium": dropKeyPolicy}},
		{name: "team policy", id: team, req: &pb.ExplainRouteRequest{Effort: pb.Effort_EFFORT_LOW}},
		{name: "capability", id: plain, req: &pb.ExplainRouteRequest{Effort: pb.Effort_EFFORT_LOW, Attributes: map[string]string{"task": "search"}, Capabilities: []string{registry.CapabilityGoogleSearch}},
			dropped: map[string]string{"cheap": router.DropCapability}},
		{name: "no capable model", id: plain, req: &pb.ExplainRouteRequest{Effort: pb.Effort_EFFORT_LOW, Capabilities: []string{registry.CapabilityGoogleSearch}}},
		{name: "placement", id: plain, req: &pb.ExplainRouteRequest{Effort: pb.Effort_EFFORT_LOW, Attributes: map[string]string{"where": "local"}, Placement: "nodes_only"},
			dropped: map[string]string{"cheap": dropPlacement}},
		{name: "override", id: plain, req: &pb.ExplainRouteRequest{ModelOverride: "premium"}},
		{name: "override denied", id: onlyCheap, req: &pb.ExplainRouteRequest{ModelOverride: "premium"}, dropped: map[string]string{"premium": dropKeyPolicy}},
		{name: "circuit open", id: plain, req: &pb.ExplainRouteRequest{Effort: pb.Effort_EFFORT_HIGH, Attributes: map[string]string{"tier": "pro"}},
			breaker: breakerOpenFor("premium"), dropped: map[string]string{"premium": dropCircuitOpen}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewProxyServer(fixedSnapshot{registry.NewSnapshotForTest(models, rules, keys, nil)}, nil)
			if tc.breaker != nil {
				s.WithCircuitBreaker(tc.breaker)
			}
			before := vendor.count()
			ex := s.ExplainRoute(context.Background(), tc.id, tc.req)
			if vendor.count() != before {
				t.Fatal("ExplainRoute called a vendor")
			}
			seen := map[string]bool{}
			for _, c := range ex.GetCandidates() {
				if want, ok := tc.dropped[c.GetModelId()]; ok {
					seen[c.GetModelId()] = true
					if c.GetDropped() != want {
						t.Errorf("%s dropped %q, want %q", c.GetModelId(), c.GetDropped(), want)
					}
				}
			}
			for id := range tc.dropped {
				if !seen[id] {
					t.Errorf("%s is not among the candidates %v", id, ex.GetCandidates())
				}
			}

			resp, err := s.GenerateText(apikeys.WithIdentity(context.Background(), tc.id), &pb.GenerateTextRequest{
				Effort: tc.req.GetEffort(), EffortSpecified: tc.req.GetEffort() != pb.Effort_EFFORT_UNSPECIFIED,
				Attributes: tc.req.GetAttributes(), Placement: tc.req.GetPlacement(), ModelOverride: tc.req.GetModelOverride(),
				EnableGoogleSearch: len(tc.req.GetCapabilities()) > 0,
				Messages:           []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "x"}},
			})
			if err != nil {
				if want := statusText(err); ex.GetError() != want {
					t.Fatalf("GenerateText failed with %q, ExplainRoute says %q", want, ex.GetError())
				}
				return
			}
			if ex.GetError() != "" || len(ex.GetModels()) == 0 {
				t.Fatalf("GenerateText served %s, ExplainRoute says %q %v", resp.GetResolvedModel(), ex.GetError(), ex.GetModels())
			}
			if ex.GetModels()[0] != resp.GetResolvedModel() || ex.GetRule() != resp.GetMatchedRule() || ex.GetRouteMode() != resp.GetRouteMode() {
				t.Fatalf("GenerateText: %s by %q (%s); ExplainRoute: %s by %q (%s)", resp.GetResolvedModel(), resp.GetMatchedRule(), resp.GetRouteMode(),
					ex.GetModels()[0], ex.GetRule(), ex.GetRouteMode())
			}
		})
	}
}

func TestExplainRouteSkipsTheJudge(t *testing.T) {
	models := []*proxydb.Model{testModel("m", registry.VendorOpenAI, "http://unused", registry.EffortMedium)}
	s := newTestServer(t, models, catchAllRules("m"), nil)
	key := &apikeys.Identity{KeyID: "key1", Scopes: []string{apikeys.ScopeGenerate}}
	ex := s.ExplainRoute(context.Background(), key, &pb.ExplainRouteRequest{AutoRoute: true})
	if !ex.GetJudgeSkipped() || ex.GetRouteMode() != router.RouteModeAutoDefault || ex.GetEffort() != registry.EffortMedium || ex.GetModels()[0] != "m" {
		t.Fatalf("auto route without effort: %v", ex)
	}
	ex = s.ExplainRoute(context.Background(), key, &pb.ExplainRouteRequest{AutoRoute: true, Effort: pb.Effort_EFFORT_MEDIUM})
	if ex.GetJudgeSkipped() || ex.GetRouteMode() != router.RouteModeAutoEffort {
		t.Fatalf("auto route with effort: %v", ex)
	}
	if ex := s.ExplainRoute(context.Background(), key, &pb.ExplainRouteRequest{}); !strings.Contains(ex.GetError(), "effort is required") {
		t.Fatalf("no effort: %v", ex)
	}
}

// A due probe lets a real call through an open breaker; the dry run must say
// so without taking the probe, and must not mirror to a broken shadow.
func TestExplainRouteBreakerAndShadow(t *testing.T) {
	now := time.Now()
	cb := circuitbreaker.NewManager(circuitbreaker.Config{Enabled: true, FailThreshold: 1, Cooldown: time.Minute})
	cb.SetNowFunc(func() time.Time { return now })
	cb.Report(context.Background(), registry.VendorOpenAI, "a", circuitbreaker.ErrUpstreamTimeout)
	cb.Report(context.Background(), registry.VendorOpenAI, "shadow", circuitbreaker.ErrUpstreamTimeout)
	models := []*proxydb.Model{
		testModel("a", registry.VendorOpenAI, "http://unused", registry.EffortLow),
		testModel("b", registry.VendorOpenAI, "http://unused", registry.EffortLow),
		testModel("shadow", registry.VendorOpenAI, "http://unused", registry.EffortLow),
	}
	rules := []*proxydb.Rule{{Name: "r", Attributes: map[string]string{}, Enabled: true, Use: []string{"a", "b"}, ShadowModel: "shadow", ShadowPercentage: 50}}
	s := newTestServer(t, models, rules, nil).WithCircuitBreaker(cb)
	id := &apikeys.Identity{KeyID: "key1", Scopes: []string{apikeys.ScopeGenerate}}
	req := &pb.ExplainRouteRequest{Effort: pb.Effort_EFFORT_LOW}

	if ex := s.ExplainRoute(context.Background(), id, req); ex.GetCandidates()[0].GetDropped() != dropCircuitOpen || ex.GetModels()[0] != "b" || ex.GetShadowModel() != "" {
		t.Fatalf("open breaker: %v", ex)
	}
	now = now.Add(2 * time.Minute)
	for i := 0; i < 2; i++ { // asking twice: the dry run takes no probe slot
		if ex := s.ExplainRoute(context.Background(), id, req); ex.GetCandidates()[0].GetDropped() != "" || ex.GetModels()[0] != "a" {
			t.Fatalf("due probe, ask %d: %v", i, ex)
		}
	}
	if !cb.Allow(registry.VendorOpenAI, "a") {
		t.Fatal("the dry run used the probe slot")
	}

	reader := &apikeys.Identity{KeyID: "key1", Scopes: []string{apikeys.ScopeAdminRead}}
	if ex := s.ExplainRoute(context.Background(), reader, req); !strings.HasPrefix(ex.GetError(), "PERMISSION_DENIED") {
		t.Fatalf("a key without generate: %v", ex)
	}
}
