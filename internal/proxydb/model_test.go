package proxydb

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestModelCachePricesRoundTrip(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()
	want := &Model{
		ID: "cache-" + uuid.NewString()[:8], Vendor: "openai", Efforts: []string{"low"},
		PriceInPerMtok: 2, PriceOutPerMtok: 8, PriceCacheReadPerMtok: 0.5, PriceCacheWritePerMtok: 2.5, Enabled: true,
	}
	if _, err := d.Mutate(ctx, func(tx pgx.Tx) error { return UpsertModel(ctx, tx, want) }, nil); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	models, err := d.LoadModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		if m.ID == want.ID {
			if m.PriceCacheReadPerMtok != 0.5 || m.PriceCacheWritePerMtok != 2.5 {
				t.Fatalf("cache prices = %v / %v, want 0.5 / 2.5", m.PriceCacheReadPerMtok, m.PriceCacheWritePerMtok)
			}
			return
		}
	}
	t.Fatalf("model %s not loaded", want.ID)
}

func TestModelTranslateFieldsRoundTrip(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()
	want := &Model{
		ID: "mt-" + uuid.NewString()[:8], Vendor: "local", Efforts: []string{"low"}, Capabilities: []string{"translate"},
		LanguagesSource: []string{"en", "fr"}, LanguagesTarget: []string{"de"}, PricePerMillionChars: 1.5, Enabled: true,
	}
	plain := &Model{ID: "plain-" + uuid.NewString()[:8], Vendor: "openai", Efforts: []string{"low"}, Enabled: true}
	if _, err := d.Mutate(ctx, func(tx pgx.Tx) error {
		if err := UpsertModel(ctx, tx, want); err != nil {
			return err
		}
		return UpsertModel(ctx, tx, plain)
	}, nil); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	models, err := d.LoadModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, m := range models {
		switch m.ID {
		case want.ID:
			found++
			if strings.Join(m.LanguagesSource, ",") != "en,fr" || strings.Join(m.LanguagesTarget, ",") != "de" || m.PricePerMillionChars != 1.5 {
				t.Errorf("translate fields = %v %v %v", m.LanguagesSource, m.LanguagesTarget, m.PricePerMillionChars)
			}
		case plain.ID:
			found++
			if len(m.LanguagesSource) != 0 || len(m.LanguagesTarget) != 0 {
				t.Errorf("a plain model loaded languages %v %v", m.LanguagesSource, m.LanguagesTarget)
			}
		}
	}
	if found != 2 {
		t.Fatalf("loaded %d of the 2 models", found)
	}
}

func TestCachePriceDefaults(t *testing.T) {
	for _, tc := range []struct {
		vendor      string
		read, write float64
	}{
		{"anthropic", 0.3, 3.75},
		{"bedrock", 0.75, 3.75},
		{"openai", 3, 3},
		{"google", 3, 3},
	} {
		m := &Model{Vendor: tc.vendor, PriceInPerMtok: 3}
		if r, w := m.CacheReadPerMtok(), m.CacheWritePerMtok(); !near(r, tc.read) || !near(w, tc.write) {
			t.Errorf("%s defaults = %v / %v, want %v / %v", tc.vendor, r, w, tc.read, tc.write)
		}
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

// A model with competencies and no efforts stores (efforts is NOT NULL), and
// a rule's needs come back as written.
func TestCompetenciesAndNeedsRoundTrip(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()
	want := &Model{ID: "comp-" + uuid.NewString()[:8], Vendor: "openai", Enabled: true,
		Competencies: map[string]string{"general": "expert", "medical": "basic"}}
	rule := &Rule{ID: uuid.NewString(), Name: "r-" + want.ID, Priority: 7, Attributes: map[string]string{},
		Needs: map[string]string{"medical": "basic"}, Enabled: true, Use: []string{want.ID}}
	if _, err := d.Mutate(ctx, func(tx pgx.Tx) error {
		if err := UpsertModel(ctx, tx, want); err != nil {
			return err
		}
		return ReplaceRules(ctx, tx, []*Rule{rule})
	}, nil); err != nil {
		t.Fatalf("store: %v", err)
	}
	models, err := d.LoadModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range models {
		if m.ID == want.ID {
			found = m.Competencies["general"] == "expert" && m.Competencies["medical"] == "basic"
		}
	}
	if !found {
		t.Fatalf("model %s: competencies not loaded", want.ID)
	}
	rules, err := d.LoadRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		if r.Name == rule.Name && r.Needs["medical"] == "basic" {
			return
		}
	}
	t.Fatalf("rule %s: needs not loaded", rule.Name)
}
