package declarative

import (
	"bytes"
	"os"
	"strings"
	"testing"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/crypto"
)

const example = `api_version: ktms.ai-process.dev/v1alpha1
models:
  - id: gpt-4o-mini
    vendor: openai
    efforts: [EFFORT_LOW, EFFORT_MEDIUM, EFFORT_HIGH]
    price_in_per_mtok: 0.15
    price_out_per_mtok: 0.6
    price_cache_read_per_mtok: 0.075
  - id: gpt-4o
    vendor: openai
    efforts: [EFFORT_MEDIUM, EFFORT_HIGH]
    price_in_per_mtok: 2.5
    price_out_per_mtok: 10
rules:
  - name: high
    priority: 20
    effort: EFFORT_HIGH
    use: [gpt-4o, gpt-4o-mini]
  - name: default
    priority: 1
    use: [gpt-4o-mini]
organizations:
  - name: acme
    teams:
      - name: search
        monthly_budget_usd: 100
        allowed_models: [gpt-4o-mini]
api_keys:
  - name: billing
    scopes: [generate]
    policy:
      rpm_limit: 120
      tpm_limit: 200000
  - name: search-bot
    organization: acme
    team: search
vendor_keys:
  - api_key_name: billing
    vendor: openai
    plain_key_env: TEST_OPENAI_KEY
spend_limits:
  - subject_type: key
    subject_id: billing
    period: day
    limit_usd: 5
user_rate_limits:
  - user_id: "*"
    model_id: gpt-4o
    rpm: 30
`

func mustParse(t *testing.T, s string) *Config {
	t.Helper()
	c, err := Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestParseExample(t *testing.T) {
	c := mustParse(t, example+mcpExample)
	if len(c.Models) != 2 || !c.Models[0].GetEnabled() {
		t.Fatalf("models = %v; a model without enabled must be enabled", c.Models)
	}
	if c.SpendLimits[0].Nano() != 5_000_000_000 {
		t.Fatalf("limit_usd 5 = %d nano", c.SpendLimits[0].Nano())
	}
	if c.APIKeys[0].Policy.GetTpmLimit() != 200000 {
		t.Fatalf("policy = %v", c.APIKeys[0].Policy)
	}
	for _, s := range sections {
		if !c.Manages(s) {
			t.Errorf("section %s present but not managed", s)
		}
	}
	if mustParse(t, "api_version: "+APIVersion+"\nmodels: []\n").Manages("rules") {
		t.Fatal("a missing section must not be managed")
	}
}

func TestParseRejects(t *testing.T) {
	head := "api_version: " + APIVersion + "\n"
	for name, tc := range map[string]struct{ doc, want string }{
		"version":         {"api_version: v0\n", "api_version must be"},
		"section":         {head + "routes: []\n", `unknown section "routes"`},
		"model field":     {head + "models:\n  - {id: m, vendor: openai, cost: 1}\n", "models[0]"},
		"key field":       {head + "api_keys:\n  - {name: k, role: x}\n", "api_keys[0]"},
		"no catch-all":    {head + "models:\n  - {id: m, vendor: openai}\nrules:\n  - {name: r, priority: 1, effort: EFFORT_LOW, use: [m]}\n", "catch-all rule (no attributes) for EFFORT_MEDIUM"},
		"unknown model":   {head + "models: []\nrules:\n  - {name: r, priority: 1, use: [ghost]}\n", `model "ghost" is not in models`},
		"dup priority":    {head + "rules:\n  - {name: a, priority: 1, use: [m]}\n  - {name: b, priority: 1, use: [m]}\n", "priority 1 is also a's"},
		"team limit":      {head + "spend_limits:\n  - {subject_type: team, subject_id: t, period: month, limit_usd: 1}\n", "monthly_budget_usd"},
		"no limit":        {head + "spend_limits:\n  - {subject_type: key, subject_id: k, period: day}\n", "above 0"},
		"team needs org":  {head + "api_keys:\n  - {name: k, team: t}\n", "needs its organization"},
		"bad scope":       {head + "api_keys:\n  - {name: k, scopes: [root]}\n", `scope "root"`},
		"tenant reader":   {head + "organizations: [{name: o}]\napi_keys:\n  - {name: k, scopes: [\"admin:read\"], organization: o}\n", "cannot belong"},
		"vk no env":       {head + "vendor_keys:\n  - {api_key_name: k, vendor: openai}\n", "plain_key_env"},
		"vk unknown key":  {head + "api_keys: []\nvendor_keys:\n  - {api_key_name: k, vendor: openai, plain_key_env: X}\n", `api key "k" is not in api_keys`},
		"empty":           {"", "empty configuration"},
		"weights":         {head + "rules:\n  - {name: r, priority: 1, use: [a, b], weights: [1]}\n", "one entry per model"},
		"twice in chain":  {head + "rules:\n  - {name: r, priority: 1, use: [a, a]}\n", "twice in use"},
		"unknown team":    {head + "organizations:\n  - {name: o}\napi_keys:\n  - {name: k, organization: o, team: t}\n", `team "t" is not in organization "o"`},
		"usd and nanousd": {head + "spend_limits:\n  - {subject_type: key, subject_id: k, period: day, limit_usd: 1, limit_nanousd: 5}\n", "not both"},
	} {
		_, err := Parse([]byte(tc.doc))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func emptyState() *State { return &State{Config: &pb.GetConfigResponse{}} }

func TestPlanFromEmptyCreatesEverything(t *testing.T) {
	c := mustParse(t, example)
	p, err := Build(c, emptyState(), Options{Env: env(map[string]string{"TEST_OPENAI_KEY": "sk-1"})})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, ch := range p.Changes {
		kinds = append(kinds, string(ch.Op)+" "+ch.Kind+" "+ch.Name)
	}
	got := strings.Join(kinds, "\n")
	// Dependencies first: organization before team, models before rules, keys before vendor keys.
	order := []string{"create organization acme", "create team acme/search", "create model gpt-4o-mini",
		"create api key billing", "create vendor key billing/openai", "update rules", "create spend limit key/billing/day",
		"create user rate limit */gpt-4o"}
	last := -1
	for _, want := range order {
		i := strings.Index(got, want)
		if i < 0 || i < last {
			t.Fatalf("plan out of order or missing %q:\n%s", want, got)
		}
		last = i
	}
	var buf bytes.Buffer
	p.Write(&buf)
	if strings.Contains(buf.String(), "sk-1") {
		t.Fatal("the plan shows a vendor key")
	}
	if !strings.Contains(buf.String(), "Plan: ") {
		t.Fatalf("plan output: %s", buf.String())
	}
}

// liveOf is what Keystone would report after applying the example.
func liveOf(t *testing.T, c *Config, vendorKey string) *State {
	t.Helper()
	st := &State{Config: &pb.GetConfigResponse{Models: c.Models, Rules: c.Rules, UserRateLimits: c.UserRateLimits}}
	st.Orgs = []*pb.Organization{{Id: "o1", Name: "acme"}}
	st.Teams = []*pb.Team{{Id: "t1", OrgId: "o1", Name: "search", MonthlyBudgetUsd: 100, AllowedModels: []string{"gpt-4o-mini"}}}
	st.Keys = []*pb.APIKeyInfo{
		{Name: "billing", Scopes: []string{"generate"}, Policy: &pb.APIKeyPolicy{RpmLimit: 120, TpmLimit: 200000}},
		{Name: "search-bot", Scopes: []string{"generate"}, OrgId: "o1", TeamId: "t1"},
		{Name: "console", Scopes: []string{"admin"}},
		{Name: "dashboard", Scopes: []string{"admin:read"}},
	}
	st.Config.VendorKeys = []*pb.VendorKeyInfo{{ApiKeyName: "billing", Vendor: "openai", Fingerprint: crypto.Fingerprint(vendorKey)}}
	st.Config.SpendLimits = []*pb.SpendLimitSpec{
		{SubjectType: "key", SubjectId: "billing", Period: "day", LimitNanousd: 5_000_000_000},
		{SubjectType: "team", SubjectId: "t1", Period: "month", LimitNanousd: 100_000_000_000},
	}
	return st
}

func TestPlanMatchingLiveIsEmpty(t *testing.T) {
	c := mustParse(t, example)
	p, err := Build(c, liveOf(t, c, "sk-1"), Options{Env: env(map[string]string{"TEST_OPENAI_KEY": "sk-1"}), Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty() {
		var buf bytes.Buffer
		p.Write(&buf)
		t.Fatalf("plan not empty:\n%s", buf.String())
	}
	if len(p.Warnings) != 2 || !strings.Contains(p.Warnings[0], "admin key console") || !strings.Contains(p.Warnings[1], "admin key dashboard") {
		t.Fatalf("warnings = %v; prune must spare the admin and admin:read keys and say so", p.Warnings)
	}
}

func TestPlanDetectsChanges(t *testing.T) {
	c := mustParse(t, example)
	live := liveOf(t, c, "sk-old")
	live.Config.Models = []*pb.ModelSpec{{Id: "gpt-4o-mini", Vendor: "openai", Enabled: true, PriceInPerMtok: 0.2, PriceOutPerMtok: 0.6, PriceCacheReadPerMtok: 0.075,
		Efforts: []pb.Effort{pb.Effort_EFFORT_HIGH, pb.Effort_EFFORT_LOW, pb.Effort_EFFORT_MEDIUM}}, c.Models[1], {Id: "old", Vendor: "openai", Enabled: true}}
	live.Keys[0].Policy = &pb.APIKeyPolicy{RpmLimit: 60}
	live.Keys = append(live.Keys, &pb.APIKeyInfo{Name: "stale", Scopes: []string{"generate"}})
	live.Config.SpendLimits[0].LimitNanousd = 1_000_000_000
	p, err := Build(c, live, Options{Env: env(map[string]string{"TEST_OPENAI_KEY": "sk-new"}), Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	p.Write(&buf)
	out := buf.String()
	for _, want := range []string{
		"~ model gpt-4o-mini", "price_in_per_mtok 0.2 → 0.15",
		"~ api key policy billing", "rpm_limit 60 → 120",
		"~ vendor key billing/openai", "changed in $TEST_OPENAI_KEY",
		"~ spend limit key/billing/day", "$1 → $5",
		"- api key stale", "- model old",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "efforts") {
		t.Errorf("effort order is not a change:\n%s", out)
	}
	if i, j := strings.Index(out, "- api key stale"), strings.Index(out, "- model old"); i > j {
		t.Errorf("deletes out of order:\n%s", out)
	}
	_, _, del := p.Count()
	if del != 2 {
		t.Errorf("deletes = %d, want 2", del)
	}
	// Without --prune nothing is deleted.
	p, _ = Build(c, live, Options{Env: env(map[string]string{"TEST_OPENAI_KEY": "sk-new"})})
	if _, _, del := p.Count(); del != 0 {
		t.Errorf("deletes without prune = %d", del)
	}
}

func TestPlanRefusesWhatTheAPICannotChange(t *testing.T) {
	c := mustParse(t, example)
	live := liveOf(t, c, "sk-1")
	live.Keys[0].Scopes = []string{"admin"}
	live.Keys[1].Disabled = true
	_, err := Build(c, live, Options{Env: env(map[string]string{})})
	for _, want := range []string{"billing has scopes [admin]", "search-bot is revoked", "TEST_OPENAI_KEY is not set"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}

func TestExportParsesBack(t *testing.T) {
	c := mustParse(t, example)
	live := liveOf(t, c, "sk-1")
	live.Config.Models[1].Enabled = false
	var buf bytes.Buffer
	if err := Export(live, &buf); err != nil {
		t.Fatal(err)
	}
	back, err := Parse(buf.Bytes())
	if err != nil {
		t.Fatalf("export does not parse: %v\n%s", err, buf.String())
	}
	for _, m := range back.Models {
		if m.GetId() == "gpt-4o" && m.GetEnabled() {
			t.Fatal("a disabled model came back enabled")
		}
	}
	if back.VendorKeys[0].PlainKeyEnv != "KTMS_VENDOR_KEY_BILLING_OPENAI" {
		t.Fatalf("vendor key env = %q", back.VendorKeys[0].PlainKeyEnv)
	}
	if strings.Contains(buf.String(), "team\n") && len(back.SpendLimits) != 1 {
		t.Fatalf("team caps belong to teams, spend limits = %v", back.SpendLimits)
	}
	p, err := Build(back, live, Options{Env: env(map[string]string{"KTMS_VENDOR_KEY_BILLING_OPENAI": "sk-1"}), Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty() {
		var out bytes.Buffer
		p.Write(&out)
		t.Fatalf("export then plan is not empty:\n%s\n%s", out.String(), buf.String())
	}
}

func TestSchemaCoversAdminMessages(t *testing.T) {
	s := string(Schema())
	for _, want := range []string{`"price_cache_read_per_mtok"`, `"shadow_model"`, `"EFFORT_HIGH"`, `"tpm_limit"`, `"plain_key_env"`, `"const": "` + APIVersion + `"`} {
		if !strings.Contains(s, want) {
			t.Errorf("schema lacks %s", want)
		}
	}
}

// The published schema and example stay in step with the code.
func TestPublishedSchemaAndExample(t *testing.T) {
	b, err := os.ReadFile("../../schema/ktms-config.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, Schema()) {
		t.Fatal("schema/ktms-config.schema.json is stale: go run ./cmd/ktms config schema > schema/ktms-config.schema.json")
	}
	if _, err := Load("../../schema/example.yaml"); err != nil {
		t.Fatalf("schema/example.yaml: %v", err)
	}
}

func TestReviewFindings(t *testing.T) {
	head := "api_version: " + APIVersion + "\n"
	if _, err := Parse([]byte(head + "api_keys: {name: billing}\n")); err == nil || !strings.Contains(err.Error(), "must be a list") {
		t.Fatalf("a mapping for api_keys = %v, want refused", err)
	}
	if _, err := Parse([]byte(head + "spend_limits: {subject_type: key}\n")); err == nil || !strings.Contains(err.Error(), "must be a list") {
		t.Fatalf("a mapping for spend_limits = %v, want refused", err)
	}
	// A key naming an organization that exists nowhere is refused, not minted untenanted.
	c := mustParse(t, head+"api_keys:\n  - {name: bot, organization: acme, team: search}\n")
	if _, err := Build(c, emptyState(), Options{}); err == nil || !strings.Contains(err.Error(), `organization "acme" exists neither`) {
		t.Fatalf("unknown organization = %v", err)
	}
	// Revoked keys' vendor keys are not exported, so the export validates.
	live := emptyState()
	live.Keys = []*pb.APIKeyInfo{{Name: "old", Scopes: []string{"generate"}, Disabled: true}}
	live.Config.VendorKeys = []*pb.VendorKeyInfo{{ApiKeyName: "old", Vendor: "openai"}}
	var buf bytes.Buffer
	if err := Export(live, &buf); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(buf.Bytes()); err != nil {
		t.Fatalf("export with a revoked key does not parse: %v\n%s", err, buf.String())
	}
	// Prune of a subject id with a slash deletes that limit, and plans are stable.
	live = emptyState()
	live.Config.SpendLimits = []*pb.SpendLimitSpec{
		{SubjectType: "user", SubjectId: "org/alice", Period: "day", LimitNanousd: 1},
		{SubjectType: "user", SubjectId: "bob", Period: "day", LimitNanousd: 1},
	}
	live.Config.UserRateLimits = []*pb.UserRateLimitSpec{{UserId: "a", ModelId: "*"}, {UserId: "b", ModelId: "*"}}
	c = mustParse(t, head+"spend_limits: []\nuser_rate_limits: []\n")
	first := ""
	for i := 0; i < 5; i++ {
		p, err := Build(c, live, Options{Prune: true})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		p.Write(&out)
		if i == 0 {
			first = out.String()
		} else if out.String() != first {
			t.Fatalf("plan order changes between runs:\n%s\n%s", first, out.String())
		}
	}
	if !strings.Contains(first, "- spend limit user/org/alice/day") {
		t.Fatalf("plan:\n%s", first)
	}
}
