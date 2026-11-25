package registry

import (
	"maps"
	"strings"
	"testing"

	"github.com/ai-process/ktms/internal/proxydb"
)

func competent(id string, comp map[string]string, efforts ...string) *proxydb.Model {
	return &proxydb.Model{ID: id, Vendor: VendorGoogle, Competencies: comp, Efforts: efforts, Enabled: true}
}

func TestMeetsIsOrdinalForCompetencies(t *testing.T) {
	m := competent("m", map[string]string{"general": "proficient", "medical": "expert"})
	cases := []struct {
		topic, level string
		want         bool
	}{
		{"general", "basic", true},
		{"general", "proficient", true},
		{"general", "expert", false},
		{"medical", "basic", true},
		{"legal", "basic", false}, // unknown topic: no error, no model
	}
	for _, c := range cases {
		if got := Meets(m, c.topic, c.level); got != c.want {
			t.Errorf("Meets(%s=%s) = %v, want %v", c.topic, c.level, got, c.want)
		}
	}
}

// A model described by efforts keeps serving exactly those efforts, so a
// config from 1.6 routes as before.
func TestMeetsKeepsEffortsExact(t *testing.T) {
	m := competent("m", nil, EffortHigh)
	if Meets(m, TopicGeneral, LevelBasic) {
		t.Error("a high-only model must not take low requests")
	}
	if !Meets(m, TopicGeneral, LevelExpert) {
		t.Error("a high model must take high requests")
	}
	if got := Competencies(competent("m", map[string]string{"medical": "basic"}, EffortLow, EffortMedium)); !maps.Equal(got, map[string]string{"medical": "basic", "general": "proficient"}) {
		t.Errorf("Competencies = %v", got)
	}
	// An explicit general wins over efforts and is ordinal.
	m = competent("m", map[string]string{"general": "expert"}, EffortHigh)
	if !Meets(m, TopicGeneral, LevelBasic) {
		t.Error("explicit general: expert must meet basic")
	}
}

func TestExcessAndRuleNeeds(t *testing.T) {
	m := competent("m", map[string]string{"general": "expert", "medical": "proficient"})
	if got := Excess(m, map[string]string{"general": "basic", "medical": "proficient"}); got != 2 {
		t.Errorf("Excess = %d, want 2", got)
	}
	if !RuleMatchesNeeds(map[string]string{"medical": "basic"}, map[string]string{"medical": "expert", "general": "basic"}) {
		t.Error("a rule for medical >= basic must match medical: expert")
	}
	if RuleMatchesNeeds(map[string]string{"medical": "proficient"}, map[string]string{"general": "expert"}) {
		t.Error("a rule for medical must not match a request without it")
	}
}

func TestParseAndRequestNeeds(t *testing.T) {
	got, err := ParseNeeds(" medical=basic , General=Expert")
	if err != nil || FormatNeeds(got) != "general=expert,medical=basic" {
		t.Fatalf("ParseNeeds = %v, %v", got, err)
	}
	for _, bad := range []string{"medical", "medical=top", "a=basic,a=expert", "Bad Topic=basic"} {
		if _, err := ParseNeeds(bad); err == nil {
			t.Errorf("ParseNeeds(%q): want an error", bad)
		}
	}
	n, err := RequestNeeds(map[string]string{"medical": "basic"}, EffortHigh)
	if err != nil || FormatNeeds(n) != "general=expert,medical=basic" {
		t.Errorf("RequestNeeds = %v, %v", n, err)
	}
	n, _ = RequestNeeds(map[string]string{"general": "basic"}, EffortHigh)
	if n["general"] != "basic" {
		t.Errorf("needs name general: effort must not override it, got %v", n)
	}
	if _, err := RequestNeeds(map[string]string{"medical": "guru"}, ""); err == nil {
		t.Error("unknown level: want an error")
	}
}

func TestValidateCompetencies(t *testing.T) {
	models := []*proxydb.Model{
		competent("small", map[string]string{"general": "basic"}),
		competent("big", map[string]string{"general": "expert", "medical": "proficient"}),
	}
	// No rules and no catch-all: selection routes every request.
	if err := Validate(models, []*proxydb.Rule{rule("medical", 1, "", map[string]string{"tenant": "a"}, "big")}); err != nil {
		t.Fatalf("Validate without catch-all: %v", err)
	}
	r := rule("medical", 1, "", nil, "small")
	r.Needs = map[string]string{"medical": "basic"}
	if err := Validate(models, []*proxydb.Rule{r}); err == nil || !strings.Contains(err.Error(), "meets its needs") {
		t.Fatalf("a chain without a medical model: err = %v", err)
	}
	r.Needs = map[string]string{"medical": "superb"}
	if err := Validate(models, []*proxydb.Rule{r}); err == nil || !strings.Contains(err.Error(), "unknown level") {
		t.Fatalf("bad rule level: err = %v", err)
	}
	bad := competent("x", map[string]string{"general": "guru"})
	if err := ValidateModel(bad); err == nil {
		t.Fatal("bad model level: want an error")
	}
	// Efforts only: the catch-all stays required, as in 1.6.
	legacy := []*proxydb.Model{competent("old", nil, EffortLow, EffortMedium, EffortHigh)}
	if err := Validate(legacy, []*proxydb.Rule{rule("t", 1, "", map[string]string{"tenant": "a"}, "old")}); err == nil || !strings.Contains(err.Error(), "catch-all") {
		t.Fatalf("legacy config without catch-all: err = %v", err)
	}
}
