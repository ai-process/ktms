package registry

import (
	"strings"
	"testing"

	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
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

// baseConfig is a minimal valid config: one model per effort, three catch-alls.
func baseConfig() ([]*proxydb.Model, []*proxydb.Rule) {
	models := []*proxydb.Model{
		model("cheap", VendorGoogle, EffortLow),
		model("mid", VendorGoogle, EffortMedium),
		model("big", VendorOpenAI, EffortHigh),
	}
	rules := []*proxydb.Rule{
		rule("low-default", 0, EffortLow, nil, "cheap"),
		rule("medium-default", 1, EffortMedium, nil, "mid"),
		rule("high-default", 2, EffortHigh, nil, "big"),
	}
	return models, rules
}

func TestValidateAcceptsBaseConfig(t *testing.T) {
	models, rules := baseConfig()
	if err := Validate(models, rules); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidateRejections(t *testing.T) {
	cases := map[string]struct {
		mutate  func(models []*proxydb.Model, rules []*proxydb.Rule) ([]*proxydb.Model, []*proxydb.Rule)
		wantSub string
	}{
		"unknown vendor": {
			func(m []*proxydb.Model, r []*proxydb.Rule) ([]*proxydb.Model, []*proxydb.Rule) {
				m[0].Vendor = "anthropic"
				return m, r
			}, "unknown vendor"},
		"ollama without endpoint": {
			func(m []*proxydb.Model, r []*proxydb.Rule) ([]*proxydb.Model, []*proxydb.Rule) {
				m[0].Vendor, m[0].Endpoint = "ollama", ""
				return m, r
			}, "needs its endpoint"},
		"empty efforts": {
			func(m []*proxydb.Model, r []*proxydb.Rule) ([]*proxydb.Model, []*proxydb.Rule) {
				m[0].Efforts = nil
				return m, r
			}, "at least one effort"},
		"invalid effort": {
			func(m []*proxydb.Model, r []*proxydb.Rule) ([]*proxydb.Model, []*proxydb.Rule) {
				m[0].Efforts = []string{"extreme"}
				return m, r
			}, "unknown effort"},
		"negative rpm": {
			func(m []*proxydb.Model, r []*proxydb.Rule) ([]*proxydb.Model, []*proxydb.Rule) {
				m[0].RPM = -1
				return m, r
			}, "negative"},
		"negative peak price": {
			func(m []*proxydb.Model, r []*proxydb.Rule) ([]*proxydb.Model, []*proxydb.Rule) {
				m[0].PriceOutPeakPerMtok = -1
				return m, r
			}, "negative"},
		"duplicate model id": {
			func(m []*proxydb.Model, r []*proxydb.Rule) ([]*proxydb.Model, []*proxydb.Rule) {
				return append(m, model("cheap", VendorOpenAI, EffortLow)), r
			}, "duplicate model id"},
		"unknown model in chain": {
			func(m []*proxydb.Model, r []*proxydb.Rule) ([]*proxydb.Model, []*proxydb.Rule) {
				r[0].Use = []string{"cheap", "gpt-99"}
				return m, r
			}, "unknown model"},
		"duplicate model in chain": {
			func(m []*proxydb.Model, r []*proxydb.Rule) ([]*proxydb.Model, []*proxydb.Rule) {
				r[0].Use = []string{"cheap", "cheap"}
				return m, r
			}, "duplicate model"},
		"empty chain": {
			func(m []*proxydb.Model, r []*proxydb.Rule) ([]*proxydb.Model, []*proxydb.Rule) {
				r[0].Use = nil
				return m, r
			}, "empty model chain"},
		"duplicate rule name": {
			func(m []*proxydb.Model, r []*proxydb.Rule) ([]*proxydb.Model, []*proxydb.Rule) {
				r[1].Name = r[0].Name
				return m, r
			}, "duplicate rule name"},
		"duplicate priority": {
			func(m []*proxydb.Model, r []*proxydb.Rule) ([]*proxydb.Model, []*proxydb.Rule) {
				r[1].Priority = r[0].Priority
				return m, r
			}, "duplicate priority"},
		"missing catch-all after disable": {
			func(m []*proxydb.Model, r []*proxydb.Rule) ([]*proxydb.Model, []*proxydb.Rule) {
				r[2].Enabled = false
				return m, r
			}, "no catch-all"},
		"chain cannot serve matchable effort": {
			// An any-effort catch-all must reach a model for every effort.
			func(m []*proxydb.Model, r []*proxydb.Rule) ([]*proxydb.Model, []*proxydb.Rule) {
				rules := []*proxydb.Rule{rule("all", 0, "", nil, "cheap")}
				return m, rules
			}, "serves effort"},
		"all chain models disabled": {
			func(m []*proxydb.Model, r []*proxydb.Rule) ([]*proxydb.Model, []*proxydb.Rule) {
				m[0].Enabled = false
				return m, r
			}, "disabled"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			models, rules := baseConfig()
			models, rules = tc.mutate(models, rules)
			err := Validate(models, rules)
			if err == nil {
				t.Fatal("Validate accepted an invalid config")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %q, want substring %q", err, tc.wantSub)
			}
		})
	}
}

// A disabled rule referencing a disabled model is dead config, not an error.
func TestValidateIgnoresDisabledRules(t *testing.T) {
	models, rules := baseConfig()
	dead := rule("experiment", 100, EffortHigh, map[string]string{"subject": "chinese"}, "cheap")
	dead.Enabled = false
	// Disabled rule with a chain that could not serve its effort: still fine.
	if err := Validate(models, append(rules, dead)); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// An any-effort catch-all whose chain covers all three efforts satisfies
// every catch-all requirement at once.
func TestValidateAnyEffortCatchAll(t *testing.T) {
	models := []*proxydb.Model{
		model("all-rounder", VendorGoogle, EffortLow, EffortMedium, EffortHigh),
	}
	rules := []*proxydb.Rule{rule("default", 0, "", nil, "all-rounder")}
	if err := Validate(models, rules); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestSnapshotClientModels(t *testing.T) {
	snap := &Snapshot{
		Models: map[string]*proxydb.Model{
			"a-google": model("a-google", VendorGoogle, EffortLow),
			"b-openai": model("b-openai", VendorOpenAI, EffortLow),
			"c-deep":   model("c-deep", VendorDeepSeek, EffortLow),
		},
		vendorKeys: map[string]map[string]string{
			"key1": {VendorGoogle: "g-secret", VendorDeepSeek: "d-secret"},
		},
	}

	got := snap.ClientModels("key1")
	if len(got) != 2 || got[0].ID != "a-google" || got[1].ID != "c-deep" {
		t.Fatalf("ClientModels = %v", got)
	}
	if models := snap.ClientModels("stranger"); len(models) != 0 {
		t.Fatalf("stranger sees %d models", len(models))
	}

	key, ok := snap.VendorKey("key1", VendorGoogle)
	if !ok || key != "g-secret" {
		t.Fatalf("VendorKey = %q, %v", key, ok)
	}
	if _, ok := snap.VendorKey("key1", VendorOpenAI); ok {
		t.Fatal("client without an openai key got one")
	}
}

// A judgment vendor is registered like any other, and its adapter must be
// judge-capable — the capability filter trusts the registry to say so.
func TestTypeSafeVendorBuildsAJudgeAdapter(t *testing.T) {
	if !ValidVendor(VendorTypeSafe) {
		t.Fatal("typesafe is not a valid vendor")
	}
	m := model("jev-latest", VendorTypeSafe, EffortLow)
	m.Capabilities = []string{CapabilityJudge}
	if err := ValidateModel(m); err != nil {
		t.Fatalf("ValidateModel: %v", err)
	}

	adapter, err := buildAdapter(m, "ts-secret")
	if err != nil {
		t.Fatalf("buildAdapter: %v", err)
	}
	if _, ok := adapter.(llm.Judge); !ok {
		t.Error("typesafe adapter does not implement llm.Judge")
	}
	// It must not pass for a chat model even if a rule names it by mistake.
	if _, err := adapter.GenerateText(llm.NewChatContext()); err == nil {
		t.Error("typesafe adapter accepted a text generation")
	}
}

func TestOpenAIVendorBuildsAJudgeAdapter(t *testing.T) {
	m := model("gpt-6-luna", VendorOpenAI, EffortLow)
	m.Capabilities = []string{CapabilityJudge}
	if err := ValidateModel(m); err != nil {
		t.Fatalf("ValidateModel: %v", err)
	}

	adapter, err := buildAdapter(m, "sk-secret")
	if err != nil {
		t.Fatalf("buildAdapter: %v", err)
	}
	if _, ok := adapter.(llm.Judge); !ok {
		t.Error("openai adapter with judge capability does not implement llm.Judge")
	}
}
