package proxydb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DB struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *DB { return &DB{pool: pool} }

// querier lets the same query helpers run on the pool or inside a tx.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// --- api keys ---

func (d *DB) GetAPIKeyByLookup(ctx context.Context, lookup string) (*APIKey, error) {
	return scanAPIKey(d.pool.QueryRow(ctx,
		`SELECT id, name, key_lookup, key_hash, scopes, disabled_at, created_at
		 FROM api_key WHERE key_lookup = $1`, lookup))
}

func (d *DB) GetAPIKeyByName(ctx context.Context, name string) (*APIKey, error) {
	return scanAPIKey(d.pool.QueryRow(ctx,
		`SELECT id, name, key_lookup, key_hash, scopes, disabled_at, created_at
		 FROM api_key WHERE name = $1`, name))
}

func scanAPIKey(row pgx.Row) (*APIKey, error) {
	var k APIKey
	err := row.Scan(&k.ID, &k.Name, &k.KeyLookup, &k.KeyHash, &k.Scopes, &k.DisabledAt, &k.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

func (d *DB) InsertAPIKey(ctx context.Context, k *APIKey) error {
	_, err := d.pool.Exec(ctx,
		`INSERT INTO api_key (id, name, key_lookup, key_hash, scopes) VALUES ($1, $2, $3, $4, $5)`,
		k.ID, k.Name, k.KeyLookup, k.KeyHash, k.Scopes)
	return err
}

func (d *DB) DisableAPIKey(ctx context.Context, name string) error {
	tag, err := d.pool.Exec(ctx,
		`UPDATE api_key SET disabled_at = NOW() WHERE name = $1 AND disabled_at IS NULL`, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (d *DB) ListAPIKeys(ctx context.Context) ([]*APIKey, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT id, name, key_lookup, key_hash, scopes, disabled_at, created_at
		 FROM api_key ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// --- config load (pool or tx) ---

func (d *DB) LoadModels(ctx context.Context) ([]*Model, error) { return loadModels(ctx, d.pool) }

func loadModels(ctx context.Context, q querier) ([]*Model, error) {
	rows, err := q.Query(ctx,
		`SELECT id, vendor, endpoint, efforts, capabilities, rpm, price_in_per_mtok,
		        price_out_per_mtok, price_in_peak_per_mtok, price_out_peak_per_mtok,
		        price_in_batch_per_mtok, price_out_batch_per_mtok,
		        daily_tokens_per_key, daily_tokens_per_user, enabled
		 FROM model ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Model
	for rows.Next() {
		var m Model
		if err := rows.Scan(&m.ID, &m.Vendor, &m.Endpoint, &m.Efforts, &m.Capabilities, &m.RPM,
			&m.PriceInPerMtok, &m.PriceOutPerMtok, &m.PriceInPeakPerMtok, &m.PriceOutPeakPerMtok,
			&m.PriceInBatchPerMtok, &m.PriceOutBatchPerMtok,
			&m.DailyTokensPerKey, &m.DailyTokensPerUser, &m.Enabled); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

func (d *DB) LoadRules(ctx context.Context) ([]*Rule, error) { return loadRules(ctx, d.pool) }

func loadRules(ctx context.Context, q querier) ([]*Rule, error) {
	rows, err := q.Query(ctx,
		`SELECT r.id, r.name, r.priority, r.effort, r.attributes, r.enabled,
		        COALESCE(array_agg(rm.model_id ORDER BY rm.position)
		                 FILTER (WHERE rm.model_id IS NOT NULL), '{}')
		 FROM rule r
		 LEFT JOIN rule_model rm ON rm.rule_id = r.id
		 GROUP BY r.id ORDER BY r.priority DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Rule
	for rows.Next() {
		var r Rule
		var attrs []byte
		if err := rows.Scan(&r.ID, &r.Name, &r.Priority, &r.Effort, &attrs, &r.Enabled, &r.Use); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(attrs, &r.Attributes); err != nil {
			return nil, fmt.Errorf("rule %s attributes: %w", r.Name, err)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

func (d *DB) LoadVendorKeys(ctx context.Context) ([]*VendorKey, error) {
	return loadVendorKeys(ctx, d.pool)
}

func loadVendorKeys(ctx context.Context, q querier) ([]*VendorKey, error) {
	rows, err := q.Query(ctx,
		`SELECT vk.id, vk.api_key_id, ak.name, vk.vendor, vk.key_ciphertext, vk.key_version, vk.created_at
		 FROM vendor_key vk JOIN api_key ak ON ak.id = vk.api_key_id
		 ORDER BY ak.name, vk.vendor`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*VendorKey
	for rows.Next() {
		var v VendorKey
		if err := rows.Scan(&v.ID, &v.APIKeyID, &v.APIKeyName, &v.Vendor, &v.KeyCiphertext,
			&v.KeyVersion, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &v)
	}
	return out, rows.Err()
}

func (d *DB) ConfigVersion(ctx context.Context) (int64, error) {
	var v int64
	err := d.pool.QueryRow(ctx, `SELECT version FROM config_state WHERE id = 1`).Scan(&v)
	return v, err
}

// --- mutations ---

// Mutate runs fn inside a transaction, then revalidates the resulting config
// via validate and bumps the version counter. Any error rolls everything back.
func (d *DB) Mutate(ctx context.Context, fn func(tx pgx.Tx) error,
	validate func(models []*Model, rules []*Rule) error) (int64, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return 0, err
	}
	if validate != nil {
		models, err := loadModels(ctx, tx)
		if err != nil {
			return 0, err
		}
		rules, err := loadRules(ctx, tx)
		if err != nil {
			return 0, err
		}
		if err := validate(models, rules); err != nil {
			return 0, err
		}
	}
	var version int64
	if err := tx.QueryRow(ctx,
		`UPDATE config_state SET version = version + 1, updated_at = NOW() WHERE id = 1
		 RETURNING version`).Scan(&version); err != nil {
		return 0, err
	}
	return version, tx.Commit(ctx)
}

func UpsertModel(ctx context.Context, tx pgx.Tx, m *Model) error {
	if m.Capabilities == nil {
		m.Capabilities = []string{} // NOT NULL column; nil would insert NULL
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO model (id, vendor, endpoint, efforts, capabilities, rpm, price_in_per_mtok,
		                    price_out_per_mtok, price_in_peak_per_mtok, price_out_peak_per_mtok,
		                    price_in_batch_per_mtok, price_out_batch_per_mtok,
		                    daily_tokens_per_key, daily_tokens_per_user, enabled)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		 ON CONFLICT (id) DO UPDATE SET vendor = $2, endpoint = $3, efforts = $4, capabilities = $5,
		   rpm = $6, price_in_per_mtok = $7, price_out_per_mtok = $8, price_in_peak_per_mtok = $9,
		   price_out_peak_per_mtok = $10, price_in_batch_per_mtok = $11, price_out_batch_per_mtok = $12,
		   daily_tokens_per_key = $13, daily_tokens_per_user = $14, enabled = $15, updated_at = NOW()`,
		m.ID, m.Vendor, m.Endpoint, m.Efforts, m.Capabilities, m.RPM,
		m.PriceInPerMtok, m.PriceOutPerMtok, m.PriceInPeakPerMtok, m.PriceOutPeakPerMtok,
		m.PriceInBatchPerMtok, m.PriceOutBatchPerMtok,
		m.DailyTokensPerKey, m.DailyTokensPerUser, m.Enabled)
	return err
}

func DeleteModel(ctx context.Context, tx pgx.Tx, id string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM model WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ReplaceRules swaps the whole rule set; position encodes chain order.
func ReplaceRules(ctx context.Context, tx pgx.Tx, rules []*Rule) error {
	if _, err := tx.Exec(ctx, `DELETE FROM rule`); err != nil {
		return err
	}
	for _, r := range rules {
		attrs, err := json.Marshal(r.Attributes)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO rule (id, name, priority, effort, attributes, enabled)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			r.ID, r.Name, r.Priority, r.Effort, attrs, r.Enabled); err != nil {
			return err
		}
		for pos, modelID := range r.Use {
			if _, err := tx.Exec(ctx,
				`INSERT INTO rule_model (rule_id, position, model_id) VALUES ($1, $2, $3)`,
				r.ID, pos, modelID); err != nil {
				return fmt.Errorf("rule %s model %s: %w", r.Name, modelID, err)
			}
		}
	}
	return nil
}

func SetVendorKey(ctx context.Context, tx pgx.Tx, v *VendorKey) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO vendor_key (id, api_key_id, vendor, key_ciphertext, key_version)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (api_key_id, vendor) DO UPDATE SET key_ciphertext = $4, key_version = $5`,
		v.ID, v.APIKeyID, v.Vendor, v.KeyCiphertext, v.KeyVersion)
	return err
}

func DeleteVendorKey(ctx context.Context, tx pgx.Tx, apiKeyID, vendor string) error {
	tag, err := tx.Exec(ctx,
		`DELETE FROM vendor_key WHERE api_key_id = $1 AND vendor = $2`, apiKeyID, vendor)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateVendorKeyCiphertext is the rotation path: same secret, newer keyring version.
func UpdateVendorKeyCiphertext(ctx context.Context, tx pgx.Tx, id string, ciphertext []byte, version int32) error {
	_, err := tx.Exec(ctx,
		`UPDATE vendor_key SET key_ciphertext = $2, key_version = $3 WHERE id = $1`,
		id, ciphertext, version)
	return err
}

func LoadVendorKeysTx(ctx context.Context, tx pgx.Tx) ([]*VendorKey, error) {
	return loadVendorKeys(ctx, tx)
}
