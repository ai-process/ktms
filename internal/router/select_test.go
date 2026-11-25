package router

import (
	"math"
	"slices"
	"testing"

	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

func comp(id, vendor string, in, out float64, c map[string]string, caps ...string) *proxydb.Model {
	return &proxydb.Model{ID: id, Vendor: vendor, Competencies: c, Capabilities: caps,
		PriceInPerMtok: in, PriceOutPerMtok: out, Enabled: true}
}

func kept(cands []Candidate) []string {
	var out []string
	for _, c := range cands {
		if c.Dropped == "" {
			out = append(out, c.ID)
		}
	}
	return out
}

// Excess before price: the expert is not spent on a basic task, and with two
// topics the sum makes the order total.
func TestSelectRanksByExcessThenPrice(t *testing.T) {
	models := []*proxydb.Model{
		comp("expert", registry.VendorOpenAI, 0.1, 0.1, map[string]string{"general": "expert", "medical": "expert"}),
		comp("fit-dear", registry.VendorOpenAI, 3, 15, map[string]string{"general": "basic", "medical": "basic"}),
		comp("fit-cheap", registry.VendorOpenAI, 0.2, 0.8, map[string]string{"general": "basic", "medical": "basic"}),
		comp("one-up", registry.VendorOpenAI, 0.1, 0.1, map[string]string{"general": "proficient", "medical": "basic"}),
		comp("no-medical", registry.VendorOpenAI, 0.01, 0.01, map[string]string{"general": "expert"}),
		comp("mt", registry.VendorOpenAI, 0, 0, map[string]string{"general": "expert", "medical": "expert"}, registry.CapabilityTranslate),
	}
	snap := registry.NewSnapshotForTest(models, nil, map[string]map[string]string{"key1": {registry.VendorOpenAI: "sk"}}, nil)
	cands := Select(snap, "key1", map[string]string{"general": "basic", "medical": "basic"}, false, true)
	if got, want := kept(cands), []string{"fit-cheap", "fit-dear", "one-up", "expert"}; !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for _, c := range cands {
		if c.ID == "no-medical" && c.Dropped != DropNeeds {
			t.Errorf("no-medical dropped %q, want %q", c.Dropped, DropNeeds)
		}
		if c.ID == "mt" {
			t.Error("a text selection must leave out translate models")
		}
		if c.ID == "fit-cheap" && (c.Excess != 0 || math.Abs(c.Price-0.35) > 1e-9) {
			t.Errorf("fit-cheap ranked by excess %d, price %v", c.Excess, c.Price)
		}
	}
}

// Price ranks within a placement class only: a free node does not jump ahead
// of the cloud under cloud_first.
func TestSelectedChainKeepsPlacementFirst(t *testing.T) {
	node := comp("node", registry.VendorLocal, 0, 0, map[string]string{"general": "basic"})
	cloudA := comp("cloud-a", registry.VendorOpenAI, 1, 1, map[string]string{"general": "basic"})
	cloudB := comp("cloud-b", registry.VendorOpenAI, 2, 2, map[string]string{"general": "basic"})
	cloudC := comp("cloud-c", registry.VendorOpenAI, 3, 3, map[string]string{"general": "basic"})
	ranked := []*proxydb.Model{node, cloudA, cloudB, cloudC}
	got, err := SelectedChain(ranked, PlacementCloudFirst)
	if err != nil {
		t.Fatal(err)
	}
	if ids := modelIDs(got); !slices.Equal(ids, []string{"cloud-a", "cloud-b", "cloud-c"}) {
		t.Errorf("cloud_first = %v", ids)
	}
	got, _ = SelectedChain(ranked, PlacementNodesFirst)
	if ids := modelIDs(got); !slices.Equal(ids, []string{"node", "cloud-a", "cloud-b"}) {
		t.Errorf("nodes_first = %v", ids)
	}
}

func modelIDs(ms []*proxydb.Model) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func TestMatchByNeeds(t *testing.T) {
	medical := &proxydb.Rule{Name: "medical", Priority: 2, Needs: map[string]string{"medical": "proficient"}, Enabled: true, Use: []string{"x"}}
	legacy := &proxydb.Rule{Name: "low", Priority: 1, Effort: registry.EffortLow, Enabled: true, Use: []string{"x"}}
	snap := registry.NewSnapshotForTest(nil, []*proxydb.Rule{medical, legacy}, nil, nil)
	if r := Match(snap, map[string]string{"medical": "expert"}, nil); r != medical {
		t.Errorf("medical: expert matched %v", r)
	}
	if r := Match(snap, map[string]string{"medical": "basic", "general": "basic"}, nil); r != legacy {
		t.Errorf("medical: basic must fall to the effort rule, got %v", r)
	}
	if r := Match(snap, map[string]string{"general": "proficient"}, nil); r != nil {
		t.Errorf("an effort rule matches its level only, got %v", r)
	}
}

// A 1.6 modality rule at effort low still takes a call that names only
// another topic.
func TestMatchModalityWithTopicNeeds(t *testing.T) {
	r := &proxydb.Rule{Name: "speech", Effort: registry.EffortLow, Attributes: map[string]string{"modality": "tts"}, Enabled: true, Use: []string{"x"}}
	snap := registry.NewSnapshotForTest(nil, []*proxydb.Rule{r}, nil, nil)
	if got := MatchModality(snap, map[string]string{"voice": "basic"}, "modality", "tts", map[string]string{"modality": "tts"}); got != r {
		t.Fatalf("got %v", got)
	}
}

// Translation is billed by characters: selection ranks it by that price.
func TestSelectRanksTranslateByChars(t *testing.T) {
	a := comp("mt-a", registry.VendorOpenAI, 0, 0, map[string]string{"general": "basic"}, registry.CapabilityTranslate)
	b := comp("llm-b", registry.VendorOpenAI, 10, 4, map[string]string{"general": "basic"}, registry.CapabilityTranslate)
	a.PricePerMillionChars, b.PricePerMillionChars = 20, 5
	snap := registry.NewSnapshotForTest([]*proxydb.Model{a, b}, nil, map[string]map[string]string{"k": {registry.VendorOpenAI: "sk"}}, nil)
	if got := kept(Select(snap, "k", nil, false, false, registry.CapabilityTranslate)); !slices.Equal(got, []string{"llm-b", "mt-a"}) {
		t.Fatalf("order = %v, want the cheaper per character first", got)
	}
}
