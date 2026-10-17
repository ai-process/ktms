package seed

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/llm-proxy/internal/apikeys"
	"github.com/ai-process/llm-proxy/internal/crypto"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
)

// Config configures the auto-seeding routine.
type Config struct {
	Enabled      bool
	ClientKey    string
	ClientName   string
	OpenAIKey    string
	GeminiKey    string
	AnthropicKey string
}

// Result describes the outcome of a seeding attempt.
type Result struct {
	Seeded    bool
	ClientKey string
	KeyName   string
	Models    []string
	Vendors   []string
}

// Run idempotently seeds initial API keys, vendor credentials, default models,
// and catch-all routing rules if the database is unpopulated.
func Run(ctx context.Context, pool *pgxpool.Pool, keyring *crypto.Keyring, cfg Config) (*Result, error) {
	if !cfg.Enabled {
		return &Result{Seeded: false}, nil
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection for seed: %w", err)
	}
	defer conn.Release()

	// Serialize with session advisory lock so only one replica seeds.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", "llmproxy:seed"); err != nil {
		return nil, fmt.Errorf("take seed lock: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", "llmproxy:seed"); err != nil {
			log.Warn().Err(err).Msg("releasing seed lock failed")
		}
	}()

	var hasModels bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM model LIMIT 1)").Scan(&hasModels); err != nil {
		return nil, fmt.Errorf("check existing models: %w", err)
	}
	if hasModels {
		log.Info().Msg("auto-seed: database already contains models, skipping")
		return &Result{Seeded: false}, nil
	}

	clientName := strings.TrimSpace(cfg.ClientName)
	if clientName == "" {
		clientName = "dev-default"
	}

	plainKey := strings.TrimSpace(cfg.ClientKey)
	var apiKeyRow *proxydb.APIKey
	if plainKey != "" {
		if !strings.HasPrefix(plainKey, apikeys.KeyPrefix) {
			plainKey = apikeys.KeyPrefix + plainKey
		}
		apiKeyRow = &proxydb.APIKey{
			ID:        uuid.NewString(),
			Name:      clientName,
			KeyLookup: apikeys.LookupHash(plainKey),
			KeyHash:   apikeys.VerifyHash(plainKey),
			Scopes:    []string{apikeys.ScopeGenerate, apikeys.ScopeAdmin},
		}
	} else {
		var err error
		plainKey, apiKeyRow, err = apikeys.Generate(clientName, []string{apikeys.ScopeGenerate, apikeys.ScopeAdmin})
		if err != nil {
			return nil, fmt.Errorf("generate api key: %w", err)
		}
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin seed transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var apiKeyID string
	err = tx.QueryRow(ctx,
		`INSERT INTO api_key (id, name, key_lookup, key_hash, scopes)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (name) DO UPDATE SET key_lookup = EXCLUDED.key_lookup, key_hash = EXCLUDED.key_hash
		 RETURNING id`,
		apiKeyRow.ID, apiKeyRow.Name, apiKeyRow.KeyLookup, apiKeyRow.KeyHash, apiKeyRow.Scopes).Scan(&apiKeyID)
	if err != nil {
		return nil, fmt.Errorf("insert api key: %w", err)
	}

	var seededVendors []string
	if cfg.OpenAIKey != "" {
		ct, v, err := keyring.Encrypt(cfg.OpenAIKey)
		if err != nil {
			return nil, fmt.Errorf("encrypt openai key: %w", err)
		}
		if err := proxydb.SetVendorKey(ctx, tx, &proxydb.VendorKey{
			ID:            uuid.NewString(),
			APIKeyID:      apiKeyID,
			Vendor:        registry.VendorOpenAI,
			KeyCiphertext: ct,
			KeyVersion:    int32(v),
		}); err != nil {
			return nil, fmt.Errorf("set openai vendor key: %w", err)
		}
		seededVendors = append(seededVendors, registry.VendorOpenAI)
	}

	if cfg.GeminiKey != "" {
		ct, v, err := keyring.Encrypt(cfg.GeminiKey)
		if err != nil {
			return nil, fmt.Errorf("encrypt gemini key: %w", err)
		}
		if err := proxydb.SetVendorKey(ctx, tx, &proxydb.VendorKey{
			ID:            uuid.NewString(),
			APIKeyID:      apiKeyID,
			Vendor:        registry.VendorGoogle,
			KeyCiphertext: ct,
			KeyVersion:    int32(v),
		}); err != nil {
			return nil, fmt.Errorf("set gemini vendor key: %w", err)
		}
		seededVendors = append(seededVendors, registry.VendorGoogle)
	}

	if cfg.AnthropicKey != "" {
		ct, v, err := keyring.Encrypt(cfg.AnthropicKey)
		if err != nil {
			return nil, fmt.Errorf("encrypt anthropic key: %w", err)
		}
		if err := proxydb.SetVendorKey(ctx, tx, &proxydb.VendorKey{
			ID:            uuid.NewString(),
			APIKeyID:      apiKeyID,
			Vendor:        registry.VendorAnthropic,
			KeyCiphertext: ct,
			KeyVersion:    int32(v),
		}); err != nil {
			return nil, fmt.Errorf("set anthropic vendor key: %w", err)
		}
		seededVendors = append(seededVendors, registry.VendorAnthropic)
	}

	models := DefaultModels()
	for _, m := range models {
		if err := proxydb.UpsertModel(ctx, tx, m); err != nil {
			return nil, fmt.Errorf("upsert model %s: %w", m.ID, err)
		}
	}

	rules := DefaultRules(models, seededVendors)
	if err := proxydb.ReplaceRules(ctx, tx, rules); err != nil {
		return nil, fmt.Errorf("replace seed rules: %w", err)
	}

	// Validate config state matches proxy constraints
	if err := registry.Validate(models, rules); err != nil {
		return nil, fmt.Errorf("seeded config invalid: %w", err)
	}

	if _, err := tx.Exec(ctx, `UPDATE config_state SET version = version + 1, updated_at = NOW() WHERE id = 1`); err != nil {
		return nil, fmt.Errorf("bump config version: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit seed transaction: %w", err)
	}

	var modelIDs []string
	for _, m := range models {
		modelIDs = append(modelIDs, m.ID)
	}

	printBanner(plainKey, clientName, seededVendors)

	return &Result{
		Seeded:    true,
		ClientKey: plainKey,
		KeyName:   clientName,
		Models:    modelIDs,
		Vendors:   seededVendors,
	}, nil
}

// DefaultModels returns standard model definitions across major providers.
func DefaultModels() []*proxydb.Model {
	return []*proxydb.Model{
		{
			ID:                  "gpt-4o-mini",
			Vendor:              registry.VendorOpenAI,
			Efforts:             []string{registry.EffortLow, registry.EffortMedium, registry.EffortHigh},
			Capabilities:        []string{"tools", "json_schema"},
			RPM:                 1000,
			PriceInPerMtok:      0.15,
			PriceOutPerMtok:     0.60,
			PriceInBatchPerMtok: 0.075,
			PriceOutBatchPerMtok: 0.30,
			Enabled:             true,
		},
		{
			ID:                  "gpt-4o",
			Vendor:              registry.VendorOpenAI,
			Efforts:             []string{registry.EffortMedium, registry.EffortHigh},
			Capabilities:        []string{"tools", "json_schema"},
			RPM:                 500,
			PriceInPerMtok:      2.50,
			PriceOutPerMtok:     10.00,
			PriceInBatchPerMtok: 1.25,
			PriceOutBatchPerMtok: 5.00,
			Enabled:             true,
		},
		{
			ID:             "text-embedding-3-small",
			Vendor:         registry.VendorOpenAI,
			Efforts:        []string{registry.EffortLow, registry.EffortMedium, registry.EffortHigh},
			Capabilities:   []string{"embeddings"},
			RPM:            1000,
			PriceInPerMtok: 0.02,
			Enabled:        true,
		},
		{
			ID:                  "gemini-2.5-flash",
			Vendor:              registry.VendorGoogle,
			Efforts:             []string{registry.EffortLow, registry.EffortMedium, registry.EffortHigh},
			Capabilities:        []string{"tools", "json_schema", "batch", "google_search"},
			RPM:                 1000,
			PriceInPerMtok:      0.15,
			PriceOutPerMtok:     0.60,
			PriceInBatchPerMtok: 0.075,
			PriceOutBatchPerMtok: 0.30,
			Enabled:             true,
		},
		{
			ID:                  "gemini-2.5-pro",
			Vendor:              registry.VendorGoogle,
			Efforts:             []string{registry.EffortHigh},
			Capabilities:        []string{"tools", "json_schema", "batch", "google_search"},
			RPM:                 300,
			PriceInPerMtok:      1.25,
			PriceOutPerMtok:     5.00,
			PriceInBatchPerMtok: 0.625,
			PriceOutBatchPerMtok: 2.50,
			Enabled:             true,
		},
		{
			ID:             "text-embedding-004",
			Vendor:         registry.VendorGoogle,
			Efforts:        []string{registry.EffortLow, registry.EffortMedium, registry.EffortHigh},
			Capabilities:   []string{"embeddings"},
			RPM:            1000,
			PriceInPerMtok: 0.025,
			Enabled:        true,
		},
		{
			ID:                  "claude-3-5-haiku-latest",
			Vendor:              registry.VendorAnthropic,
			Efforts:             []string{registry.EffortLow, registry.EffortMedium, registry.EffortHigh},
			Capabilities:        []string{"tools", "json_schema"},
			RPM:                 1000,
			PriceInPerMtok:      0.80,
			PriceOutPerMtok:     4.00,
			PriceInBatchPerMtok: 0.40,
			PriceOutBatchPerMtok: 2.00,
			Enabled:             true,
		},
		{
			ID:                  "claude-3-7-sonnet-latest",
			Vendor:              registry.VendorAnthropic,
			Efforts:             []string{registry.EffortHigh},
			Capabilities:        []string{"tools", "json_schema"},
			RPM:                 300,
			PriceInPerMtok:      3.00,
			PriceOutPerMtok:     15.00,
			PriceInBatchPerMtok: 1.50,
			PriceOutBatchPerMtok: 7.50,
			Enabled:             true,
		},
	}
}

// DefaultRules creates catch-all rules prioritized based on provided vendor keys.
func DefaultRules(models []*proxydb.Model, activeVendors []string) []*proxydb.Rule {
	vendorActive := map[string]bool{}
	for _, v := range activeVendors {
		vendorActive[v] = true
	}

	filterByEffort := func(targetEffort string) []string {
		var active, fallback []string
		for _, m := range models {
			if !serves(m, targetEffort) {
				continue
			}
			if vendorActive[m.Vendor] {
				active = append(active, m.ID)
			} else {
				fallback = append(fallback, m.ID)
			}
		}
		if len(active) > 0 {
			return append(active, fallback...)
		}
		return fallback
	}

	lowChain := filterByEffort(registry.EffortLow)
	medChain := filterByEffort(registry.EffortMedium)
	highChain := filterByEffort(registry.EffortHigh)

	var allChain []string
	seen := map[string]bool{}
	for _, id := range append(append(lowChain, medChain...), highChain...) {
		if !seen[id] {
			seen[id] = true
			allChain = append(allChain, id)
		}
	}

	return []*proxydb.Rule{
		{
			ID:         uuid.NewString(),
			Name:       "default-low",
			Priority:   10,
			Effort:     registry.EffortLow,
			Attributes: map[string]string{},
			Enabled:    true,
			Use:        lowChain,
		},
		{
			ID:         uuid.NewString(),
			Name:       "default-medium",
			Priority:   9,
			Effort:     registry.EffortMedium,
			Attributes: map[string]string{},
			Enabled:    true,
			Use:        medChain,
		},
		{
			ID:         uuid.NewString(),
			Name:       "default-high",
			Priority:   8,
			Effort:     registry.EffortHigh,
			Attributes: map[string]string{},
			Enabled:    true,
			Use:        highChain,
		},
		{
			ID:         uuid.NewString(),
			Name:       "default-catchall",
			Priority:   1,
			Effort:     "",
			Attributes: map[string]string{},
			Enabled:    true,
			Use:        allChain,
		},
	}
}

func serves(m *proxydb.Model, effort string) bool {
	for _, c := range m.Capabilities {
		if c == "embeddings" {
			return false
		}
	}
	for _, e := range m.Efforts {
		if e == effort {
			return true
		}
	}
	return false
}

func printBanner(clientKey, keyName string, vendors []string) {
	vList := "none configured"
	if len(vendors) > 0 {
		vList = strings.Join(vendors, ", ")
	}
	banner := fmt.Sprintf(`
================================================================================
[llm-proxy] Quickstart Auto-Seed Completed!
Client Key:    %s (name: %s)
Vendor Keys:   %s

Try your first request:
  curl http://localhost:8080/v1/chat/completions \
    -H "Authorization: Bearer %s" \
    -H "Content-Type: application/json" \
    -d '{"model":"auto","messages":[{"role":"user","content":"Say hi in three words."}]}'
================================================================================`,
		clientKey, keyName, vList, clientKey)
	log.Info().Msg(banner)
}
