package seed

import (
	"slices"
	"testing"

	"github.com/ai-process/ktms/internal/registry"
)

func TestDefaultModelsAndRulesValidation(t *testing.T) {
	models := DefaultModels()
	if len(models) == 0 {
		t.Fatal("expected at least one default model")
	}

	testCases := []struct {
		name          string
		activeVendors []string
	}{
		{name: "no active vendors", activeVendors: nil},
		{name: "openai only", activeVendors: []string{registry.VendorOpenAI}},
		{name: "google only", activeVendors: []string{registry.VendorGoogle}},
		{name: "anthropic only", activeVendors: []string{registry.VendorAnthropic}},
		{name: "all vendors", activeVendors: []string{registry.VendorOpenAI, registry.VendorGoogle, registry.VendorAnthropic}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			rules := DefaultRules(models, tc.activeVendors)
			if len(rules) == 0 {
				t.Fatal("expected at least one default rule")
			}
			if err := registry.Validate(models, rules); err != nil {
				t.Fatalf("registry.Validate failed: %v", err)
			}
		})
	}
}

func TestEmbeddingModelsExcludedFromGenerationRules(t *testing.T) {
	models := DefaultModels()
	rules := DefaultRules(models, []string{registry.VendorOpenAI, registry.VendorGoogle})

	for _, r := range rules {
		for _, modelID := range r.Use {
			if modelID == "text-embedding-3-small" || modelID == "gemini-embedding-001" {
				t.Errorf("rule %s unexpectedly includes embedding model %s", r.Name, modelID)
			}
		}
	}
}

func TestDefaultRulesChainOrder(t *testing.T) {
	models := DefaultModels()

	// When openai is active, openai models should precede other models in the low chain.
	rulesOpenAI := DefaultRules(models, []string{registry.VendorOpenAI})
	for _, r := range rulesOpenAI {
		if r.Effort == registry.EffortLow {
			if len(r.Use) == 0 {
				t.Fatal("expected non-empty low chain")
			}
			if r.Use[0] != "gpt-4o-mini" {
				t.Errorf("expected gpt-4o-mini to be first in low chain, got %s", r.Use[0])
			}
		}
	}

	// When google is active, gemini models should precede other models in the low chain.
	rulesGemini := DefaultRules(models, []string{registry.VendorGoogle})
	for _, r := range rulesGemini {
		if r.Effort == registry.EffortLow {
			if len(r.Use) == 0 {
				t.Fatal("expected non-empty low chain")
			}
			if r.Use[0] != "gemini-2.5-flash" {
				t.Errorf("expected gemini-2.5-flash to be first in low chain, got %s", r.Use[0])
			}
		}
	}
}

func TestDefaultRulesPutTheModelMadeForTheEffortFirst(t *testing.T) {
	chains := map[string][]string{}
	for _, r := range DefaultRules(DefaultModels(), []string{registry.VendorOpenAI}) {
		chains[r.Name] = r.Use
	}
	want := map[string][]string{
		"default-low":    {"gpt-4o-mini"},
		"default-medium": {"gpt-4o", "gpt-4o-mini"},
		"default-high":   {"gpt-4o", "gpt-4o-mini"},
	}
	for name, w := range want {
		if got := chains[name]; !slices.Equal(got[:len(w)], w) {
			t.Errorf("%s starts with %v, want %v", name, got, w)
		}
	}

	// With every vendor, high tries the high-only models before the rest.
	for _, r := range DefaultRules(DefaultModels(), []string{registry.VendorOpenAI, registry.VendorGoogle, registry.VendorAnthropic}) {
		if r.Name == "default-high" && !slices.Equal(r.Use[:3], []string{"gemini-2.5-pro", "claude-sonnet-5-5", "gpt-4o"}) {
			t.Errorf("default-high = %v", r.Use)
		}
	}
}
