package grpcapi

import (
	"context"
	"slices"
	"strings"
	"testing"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/router"
)

func competentModel(id string, in float64, comp map[string]string) *proxydb.Model {
	m := testModel(id, registry.VendorOpenAI, "http://unused")
	m.Competencies, m.PriceInPerMtok, m.PriceOutPerMtok = comp, in, in
	return m
}

func selectionModels() []*proxydb.Model {
	return []*proxydb.Model{
		competentModel("small", 0.1, map[string]string{"general": "basic"}),
		competentModel("mid", 1, map[string]string{"general": "proficient", "medical": "basic"}),
		competentModel("doc", 5, map[string]string{"general": "proficient", "medical": "expert"}),
		competentModel("top", 10, map[string]string{"general": "expert", "medical": "proficient"}),
		competentModel("spare", 20, map[string]string{"general": "expert"}),
	}
}

func TestSelectionWithoutRules(t *testing.T) {
	s := newTestServer(t, selectionModels(), nil, nil)
	key := &apikeys.Identity{KeyID: "key1", Scopes: []string{apikeys.ScopeGenerate}}
	ex := s.ExplainRoute(context.Background(), key, &pb.ExplainRouteRequest{Needs: map[string]string{"medical": "basic"}})
	if ex.GetError() != "" || !ex.GetSelected() || ex.GetRule() != "" {
		t.Fatalf("selection: %v", ex)
	}
	if want := []string{"mid", "top", "doc"}; !slices.Equal(ex.GetModels(), want) {
		t.Errorf("chain = %v, want %v (excess, then price)", ex.GetModels(), want)
	}
	for _, c := range ex.GetCandidates() {
		switch c.GetModelId() {
		case "small", "spare":
			if c.GetDropped() != router.DropNeeds {
				t.Errorf("%s dropped %q", c.GetModelId(), c.GetDropped())
			}
		case "doc":
			if c.GetExcess() != 2 {
				t.Errorf("doc excess %d, want 2", c.GetExcess())
			}
		}
	}

	// Effort is general: high is general: expert.
	ex = s.ExplainRoute(context.Background(), key, &pb.ExplainRouteRequest{Effort: pb.Effort_EFFORT_HIGH})
	if !slices.Equal(ex.GetModels(), []string{"top", "spare"}) || ex.GetNeeds()["general"] != "expert" || ex.GetEffort() != registry.EffortHigh {
		t.Errorf("effort high: %v", ex)
	}

	ex = s.ExplainRoute(context.Background(), key, &pb.ExplainRouteRequest{Needs: map[string]string{"medical": "guru"}})
	if !strings.Contains(ex.GetError(), "INVALID_ARGUMENT") {
		t.Errorf("unknown level: %v", ex)
	}
	ex = s.ExplainRoute(context.Background(), key, &pb.ExplainRouteRequest{Needs: map[string]string{"legal": "basic"}})
	if !strings.Contains(ex.GetError(), "no_capable_model") {
		t.Errorf("unknown topic finds no model: %v", ex)
	}
}

// allowed_models narrows the pool before ranking: the key gets the best of
// what it may use, not an empty chain cut from the global top three.
func TestSelectionAppliesPolicyBeforeRanking(t *testing.T) {
	s := newTestServer(t, selectionModels(), nil, nil)
	key := &apikeys.Identity{KeyID: "key1", Scopes: []string{apikeys.ScopeGenerate}, KeyAllowedModels: []string{"spare"}}
	ex := s.ExplainRoute(context.Background(), key, &pb.ExplainRouteRequest{Needs: map[string]string{"general": "basic"}})
	if !slices.Equal(ex.GetModels(), []string{"spare"}) {
		t.Fatalf("policy: %v", ex)
	}
}

// A rule that matches replaces the selection with its own chain.
func TestRuleReplacesSelection(t *testing.T) {
	rules := []*proxydb.Rule{{Name: "medical", Priority: 1, Needs: map[string]string{"medical": "proficient"},
		Attributes: map[string]string{}, Enabled: true, Use: []string{"top", "doc"}}}
	s := newTestServer(t, selectionModels(), rules, nil)
	key := &apikeys.Identity{KeyID: "key1", Scopes: []string{apikeys.ScopeGenerate}}
	ex := s.ExplainRoute(context.Background(), key, &pb.ExplainRouteRequest{Needs: map[string]string{"medical": "expert"}})
	if ex.GetRule() != "medical" || ex.GetSelected() || !slices.Equal(ex.GetModels(), []string{"doc"}) {
		t.Fatalf("rule: %v", ex)
	}
	ex = s.ExplainRoute(context.Background(), key, &pb.ExplainRouteRequest{Needs: map[string]string{"medical": "basic"}})
	if ex.GetRule() != "" || !ex.GetSelected() {
		t.Fatalf("below the rule's level selection routes: %v", ex)
	}
}

func TestUsageMetaCarriesNeeds(t *testing.T) {
	meta := buildChat(&pb.GenerateTextRequest{}, &apikeys.Identity{Name: "k"}, "", map[string]string{"general": "proficient", "medical": "basic"}).GetMeta()
	if meta["effort"] != registry.EffortMedium || meta["needs"] != "general=proficient,medical=basic" {
		t.Fatalf("meta = %v", meta)
	}
}

// Without needs a modality call is filtered by its capability alone.
func TestMediaSelectionByModality(t *testing.T) {
	tts := testModel("voice", registry.VendorOpenAI, "http://unused", registry.EffortMedium)
	tts.Capabilities = []string{registry.CapabilityTTS}
	s := newTestServer(t, append(selectionModels(), tts), nil, nil)
	key := &apikeys.Identity{KeyID: "key1", Scopes: []string{apikeys.ScopeGenerate}}
	ex := s.ExplainRoute(context.Background(), key, &pb.ExplainRouteRequest{Modality: modalityTTS})
	if !ex.GetSelected() || !slices.Equal(ex.GetModels(), []string{"voice"}) {
		t.Fatalf("tts: %v", ex)
	}
	// And a text call never gets the voice.
	ex = s.ExplainRoute(context.Background(), key, &pb.ExplainRouteRequest{Effort: pb.Effort_EFFORT_MEDIUM})
	if slices.Contains(ex.GetModels(), "voice") {
		t.Fatalf("text chain holds a tts model: %v", ex)
	}
}

// Under nodes_first ExplainRoute lists the kept candidates as a call tries
// them: the node first, though a cloud model ranks above it on excess.
func TestExplainSelectionOrderFollowsPlacement(t *testing.T) {
	node := testModel("node", registry.VendorLocal, "http://unused")
	node.Competencies = map[string]string{"general": "expert"}
	s := newTestServer(t, append(selectionModels(), node), nil, nil)
	key := &apikeys.Identity{KeyID: "key1", Scopes: []string{apikeys.ScopeGenerate}}
	ex := s.ExplainRoute(context.Background(), key, &pb.ExplainRouteRequest{Needs: map[string]string{"general": "basic"}, Placement: "nodes_first"})
	if len(ex.GetModels()) == 0 || ex.GetModels()[0] != "node" || ex.GetCandidates()[0].GetModelId() != "node" {
		t.Fatalf("nodes_first: models %v, candidates %v", ex.GetModels(), ex.GetCandidates())
	}
}
