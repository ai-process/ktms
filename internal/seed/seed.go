package seed

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/crypto"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
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
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", "ktms:seed"); err != nil {
		return nil, fmt.Errorf("take seed lock: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", "ktms:seed"); err != nil {
			log.Warn().Err(err).Msg("releasing seed lock failed")
		}
	}()

	clientName := strings.TrimSpace(cfg.ClientName)
	if clientName == "" {
		clientName = "dev-default"
	}

	var hasModels bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM model LIMIT 1)").Scan(&hasModels); err != nil {
		return nil, fmt.Errorf("check existing models: %w", err)
	}
	if hasModels {
		// Sync any newly provided vendor keys to the existing dev key without wiping the volume.
		updated, err := syncVendorKeys(ctx, conn, keyring, clientName, cfg)
		if err != nil {
			return nil, err
		}
		if updated {
			log.Info().Msg("auto-seed: updated vendor credentials on existing client key")
			return &Result{Seeded: true}, nil
		}
		log.Info().Msg("auto-seed: database already contains models, skipping")
		return &Result{Seeded: false}, nil
	}

	plainKey := strings.TrimSpace(cfg.ClientKey)
	var apiKeyRow *proxydb.APIKey
	if plainKey != "" {
		if !strings.HasPrefix(plainKey, apikeys.KeyPrefix) && !strings.HasPrefix(plainKey, apikeys.LegacyKeyPrefix) {
			plainKey = apikeys.KeyPrefix + plainKey
		}
		apiKeyRow = &proxydb.APIKey{
			ID:        uuid.NewString(),
			Name:      clientName,
			KeyLookup: apikeys.LookupHash(plainKey),
			KeyHash:   apikeys.VerifyHash(plainKey),
			Scopes:    []string{apikeys.ScopeGenerate},
		}
	} else {
		var err error
		plainKey, apiKeyRow, err = apikeys.Generate(clientName, []string{apikeys.ScopeGenerate})
		if err != nil {
			return nil, fmt.Errorf("generate api key: %w", err)
		}
	}

	if plainKey == "llm_dev_default_key" || plainKey == "ktms_dev_default_key" {
		log.Warn().Msg("auto-seed: using default dev client key; generate a unique key for non-dev environments")
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin seed transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var apiKeyID string
	err = tx.QueryRow(ctx, `SELECT id FROM api_key WHERE name = $1`, clientName).Scan(&apiKeyID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx,
			`INSERT INTO api_key (id, name, key_lookup, key_hash, scopes)
			 VALUES ($1, $2, $3, $4, $5)
			 RETURNING id`,
			apiKeyRow.ID, apiKeyRow.Name, apiKeyRow.KeyLookup, apiKeyRow.KeyHash, apiKeyRow.Scopes).Scan(&apiKeyID)
		if err != nil {
			return nil, fmt.Errorf("insert api key: %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("lookup api key: %w", err)
	} else if strings.TrimSpace(cfg.ClientKey) != "" {
		_, err = tx.Exec(ctx,
			`UPDATE api_key SET key_lookup = $1, key_hash = $2, scopes = $3 WHERE id = $4`,
			apiKeyRow.KeyLookup, apiKeyRow.KeyHash, apiKeyRow.Scopes, apiKeyID)
		if err != nil {
			return nil, fmt.Errorf("update api key: %w", err)
		}
	} else {
		// Existing api_key row was preserved; clear un-persisted generated key.
		plainKey = ""
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

func syncVendorKeys(ctx context.Context, conn *pgxpool.Conn, keyring *crypto.Keyring, clientName string, cfg Config) (bool, error) {
	if cfg.OpenAIKey == "" && cfg.GeminiKey == "" && cfg.AnthropicKey == "" {
		return false, nil
	}
	var apiKeyID string
	err := conn.QueryRow(ctx, `SELECT id FROM api_key WHERE name = $1`, clientName).Scan(&apiKeyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lookup api key for vendor sync: %w", err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin vendor sync tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var updated bool
	if cfg.OpenAIKey != "" {
		ct, v, err := keyring.Encrypt(cfg.OpenAIKey)
		if err != nil {
			return false, fmt.Errorf("encrypt openai key: %w", err)
		}
		if err := proxydb.SetVendorKey(ctx, tx, &proxydb.VendorKey{
			ID:            uuid.NewString(),
			APIKeyID:      apiKeyID,
			Vendor:        registry.VendorOpenAI,
			KeyCiphertext: ct,
			KeyVersion:    int32(v),
		}); err != nil {
			return false, fmt.Errorf("set openai vendor key: %w", err)
		}
		updated = true
	}
	if cfg.GeminiKey != "" {
		ct, v, err := keyring.Encrypt(cfg.GeminiKey)
		if err != nil {
			return false, fmt.Errorf("encrypt gemini key: %w", err)
		}
		if err := proxydb.SetVendorKey(ctx, tx, &proxydb.VendorKey{
			ID:            uuid.NewString(),
			APIKeyID:      apiKeyID,
			Vendor:        registry.VendorGoogle,
			KeyCiphertext: ct,
			KeyVersion:    int32(v),
		}); err != nil {
			return false, fmt.Errorf("set gemini vendor key: %w", err)
		}
		updated = true
	}
	if cfg.AnthropicKey != "" {
		ct, v, err := keyring.Encrypt(cfg.AnthropicKey)
		if err != nil {
			return false, fmt.Errorf("encrypt anthropic key: %w", err)
		}
		if err := proxydb.SetVendorKey(ctx, tx, &proxydb.VendorKey{
			ID:            uuid.NewString(),
			APIKeyID:      apiKeyID,
			Vendor:        registry.VendorAnthropic,
			KeyCiphertext: ct,
			KeyVersion:    int32(v),
		}); err != nil {
			return false, fmt.Errorf("set anthropic vendor key: %w", err)
		}
		updated = true
	}

	if updated {
		if _, err := tx.Exec(ctx, `UPDATE config_state SET version = version + 1, updated_at = NOW() WHERE id = 1`); err != nil {
			return false, fmt.Errorf("bump config version on vendor sync: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit vendor sync tx: %w", err)
	}
	return updated, nil
}

// DefaultModels returns standard model definitions across major providers.
func DefaultModels() []*proxydb.Model {
	return []*proxydb.Model{
		{
			ID:                   "gpt-4o-mini",
			Vendor:               registry.VendorOpenAI,
			Efforts:              []string{registry.EffortLow, registry.EffortMedium, registry.EffortHigh},
			RPM:                  1000,
			PriceInPerMtok:       0.15,
			PriceOutPerMtok:      0.60,
			PriceInBatchPerMtok:  0.075,
			PriceOutBatchPerMtok: 0.30,
			Enabled:              true,
		},
		{
			ID:                   "gpt-4o",
			Vendor:               registry.VendorOpenAI,
			Efforts:              []string{registry.EffortMedium, registry.EffortHigh},
			RPM:                  500,
			PriceInPerMtok:       2.50,
			PriceOutPerMtok:      10.00,
			PriceInBatchPerMtok:  1.25,
			PriceOutBatchPerMtok: 5.00,
			Enabled:              true,
		},
		{
			ID:             "text-embedding-3-small",
			Vendor:         registry.VendorOpenAI,
			Efforts:        []string{registry.EffortLow, registry.EffortMedium, registry.EffortHigh},
			Capabilities:   []string{registry.CapabilityEmbedding},
			RPM:            1000,
			PriceInPerMtok: 0.02,
			Enabled:        true,
		},
		{
			ID:                   "gemini-2.5-flash",
			Vendor:               registry.VendorGoogle,
			Efforts:              []string{registry.EffortLow, registry.EffortMedium, registry.EffortHigh},
			Capabilities:         []string{registry.CapabilityBatch, registry.CapabilityGoogleSearch},
			RPM:                  1000,
			PriceInPerMtok:       0.30,
			PriceOutPerMtok:      2.50,
			PriceInBatchPerMtok:  0.15,
			PriceOutBatchPerMtok: 1.25,
			Enabled:              true,
		},
		{
			ID:                   "gemini-2.5-pro",
			Vendor:               registry.VendorGoogle,
			Efforts:              []string{registry.EffortHigh},
			Capabilities:         []string{registry.CapabilityBatch, registry.CapabilityGoogleSearch},
			RPM:                  300,
			PriceInPerMtok:       1.25,
			PriceOutPerMtok:      10.00,
			PriceInBatchPerMtok:  0.625,
			PriceOutBatchPerMtok: 5.00,
			Enabled:              true,
		},
		{
			ID:             "gemini-embedding-001",
			Vendor:         registry.VendorGoogle,
			Efforts:        []string{registry.EffortLow, registry.EffortMedium, registry.EffortHigh},
			Capabilities:   []string{registry.CapabilityEmbedding},
			RPM:            1000,
			PriceInPerMtok: 0.15,
			Enabled:        true,
		},
		{
			ID:                   "claude-haiku-4-5-20251001",
			Vendor:               registry.VendorAnthropic,
			Efforts:              []string{registry.EffortLow, registry.EffortMedium, registry.EffortHigh},
			RPM:                  1000,
			PriceInPerMtok:       1.00,
			PriceOutPerMtok:      5.00,
			PriceInBatchPerMtok:  0.50,
			PriceOutBatchPerMtok: 2.50,
			Enabled:              true,
		},
		{
			ID:                   "claude-sonnet-5-5",
			Vendor:               registry.VendorAnthropic,
			Efforts:              []string{registry.EffortHigh},
			RPM:                  300,
			PriceInPerMtok:       2.00,
			PriceOutPerMtok:      10.00,
			PriceInBatchPerMtok:  1.00,
			PriceOutBatchPerMtok: 5.00,
			Enabled:              true,
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
	if registry.HasCapability(m, registry.CapabilityEmbedding) {
		return false
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
	keyDisplay := clientKey
	authHeader := clientKey
	if clientKey == "" {
		keyDisplay = "(existing key preserved)"
		authHeader = "<your-client-key>"
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
		keyDisplay, keyName, vList, authHeader)
	log.Info().Msg(banner)
}
