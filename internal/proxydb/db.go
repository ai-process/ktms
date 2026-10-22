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
		`SELECT k.id, k.name, k.key_lookup, k.key_hash, k.scopes, k.disabled_at, k.created_at,
		        k.org_id, k.team_id, COALESCE(t.allowed_models, '{}')
		 FROM api_key k
		 LEFT JOIN team t ON t.id = k.team_id
		 WHERE k.key_lookup = $1`, lookup))
}

func (d *DB) GetAPIKeyByName(ctx context.Context, name string) (*APIKey, error) {
	return scanAPIKey(d.pool.QueryRow(ctx,
		`SELECT k.id, k.name, k.key_lookup, k.key_hash, k.scopes, k.disabled_at, k.created_at,
		        k.org_id, k.team_id, COALESCE(t.allowed_models, '{}')
		 FROM api_key k
		 LEFT JOIN team t ON t.id = k.team_id
		 WHERE k.name = $1`, name))
}

func scanAPIKey(row pgx.Row) (*APIKey, error) {
	var k APIKey
	err := row.Scan(&k.ID, &k.Name, &k.KeyLookup, &k.KeyHash, &k.Scopes, &k.DisabledAt, &k.CreatedAt,
		&k.OrgID, &k.TeamID, &k.TeamAllowedModels)
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
		`INSERT INTO api_key (id, name, key_lookup, key_hash, scopes, org_id, team_id) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		k.ID, k.Name, k.KeyLookup, k.KeyHash, k.Scopes, k.OrgID, k.TeamID)
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
		`SELECT k.id, k.name, k.key_lookup, k.key_hash, k.scopes, k.disabled_at, k.created_at,
		        k.org_id, k.team_id, COALESCE(t.allowed_models, '{}')
		 FROM api_key k
		 LEFT JOIN team t ON t.id = k.team_id
		 ORDER BY k.created_at`)
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
		        daily_tokens_per_key, daily_tokens_per_user, enabled,
		        burst, queue_timeout_ms, budget_grace_margin
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
			&m.DailyTokensPerKey, &m.DailyTokensPerUser, &m.Enabled,
			&m.Burst, &m.QueueTimeoutMs, &m.BudgetGraceMargin); err != nil {
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
		        r.shadow_model, r.shadow_percentage, r.shadow_eval,
		        COALESCE(array_agg(rm.model_id ORDER BY rm.position)
		                 FILTER (WHERE rm.model_id IS NOT NULL), '{}'),
		        COALESCE(array_agg(rm.weight ORDER BY rm.position)
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
		var weights []int32
		if err := rows.Scan(&r.ID, &r.Name, &r.Priority, &r.Effort, &attrs, &r.Enabled,
			&r.ShadowModel, &r.ShadowPercentage, &r.ShadowEval, &r.Use, &weights); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(attrs, &r.Attributes); err != nil {
			return nil, fmt.Errorf("rule %s attributes: %w", r.Name, err)
		}
		var hasNonZero bool
		for _, w := range weights {
			if w > 0 {
				hasNonZero = true
				break
			}
		}
		if hasNonZero {
			r.Weights = weights
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
		                    daily_tokens_per_key, daily_tokens_per_user, enabled,
		                    burst, queue_timeout_ms, budget_grace_margin)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		 ON CONFLICT (id) DO UPDATE SET vendor = $2, endpoint = $3, efforts = $4, capabilities = $5,
		   rpm = $6, price_in_per_mtok = $7, price_out_per_mtok = $8, price_in_peak_per_mtok = $9,
		   price_out_peak_per_mtok = $10, price_in_batch_per_mtok = $11, price_out_batch_per_mtok = $12,
		   daily_tokens_per_key = $13, daily_tokens_per_user = $14, enabled = $15,
		   burst = $16, queue_timeout_ms = $17, budget_grace_margin = $18, updated_at = NOW()`,
		m.ID, m.Vendor, m.Endpoint, m.Efforts, m.Capabilities, m.RPM,
		m.PriceInPerMtok, m.PriceOutPerMtok, m.PriceInPeakPerMtok, m.PriceOutPeakPerMtok,
		m.PriceInBatchPerMtok, m.PriceOutBatchPerMtok,
		m.DailyTokensPerKey, m.DailyTokensPerUser, m.Enabled,
		m.Burst, m.QueueTimeoutMs, m.BudgetGraceMargin)
	return err
}

func (d *DB) LoadUserRateLimits(ctx context.Context) ([]*UserRateLimit, error) {
	return loadUserRateLimits(ctx, d.pool)
}

func loadUserRateLimits(ctx context.Context, q querier) ([]*UserRateLimit, error) {
	rows, err := q.Query(ctx,
		`SELECT user_id, model_id, rpm, burst, queue_timeout_ms, daily_tokens
		 FROM user_rate_limits ORDER BY user_id, model_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*UserRateLimit
	for rows.Next() {
		var u UserRateLimit
		if err := rows.Scan(&u.UserID, &u.ModelID, &u.RPM, &u.Burst, &u.QueueTimeoutMs, &u.DailyTokens); err != nil {
			return nil, err
		}
		out = append(out, &u)
	}
	return out, rows.Err()
}

func UpsertUserRateLimit(ctx context.Context, tx pgx.Tx, u *UserRateLimit) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO user_rate_limits (user_id, model_id, rpm, burst, queue_timeout_ms, daily_tokens, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NOW())
		 ON CONFLICT (user_id, model_id) DO UPDATE SET
		   rpm = $3, burst = $4, queue_timeout_ms = $5, daily_tokens = $6, updated_at = NOW()`,
		u.UserID, u.ModelID, u.RPM, u.Burst, u.QueueTimeoutMs, u.DailyTokens)
	return err
}

func DeleteUserRateLimit(ctx context.Context, tx pgx.Tx, userID, modelID string) error {
	tag, err := tx.Exec(ctx,
		`DELETE FROM user_rate_limits WHERE user_id = $1 AND model_id = $2`,
		userID, modelID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
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
			`INSERT INTO rule (id, name, priority, effort, attributes, enabled, shadow_model, shadow_percentage, shadow_eval)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			r.ID, r.Name, r.Priority, r.Effort, attrs, r.Enabled, r.ShadowModel, r.ShadowPercentage, r.ShadowEval); err != nil {
			return err
		}
		for pos, modelID := range r.Use {
			var weight int32
			if pos < len(r.Weights) {
				weight = r.Weights[pos]
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO rule_model (rule_id, position, model_id, weight) VALUES ($1, $2, $3, $4)`,
				r.ID, pos, modelID, weight); err != nil {
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

// LockVendorKeysTx reads every vendor key and locks the rows until the
// transaction ends, so a key set meanwhile cannot be overwritten by a stale
// re-encryption of the previous one.
func LockVendorKeysTx(ctx context.Context, tx pgx.Tx) ([]*VendorKey, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, api_key_id, vendor, key_ciphertext, key_version FROM vendor_key ORDER BY id FOR UPDATE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*VendorKey
	for rows.Next() {
		var v VendorKey
		if err := rows.Scan(&v.ID, &v.APIKeyID, &v.Vendor, &v.KeyCiphertext, &v.KeyVersion); err != nil {
			return nil, err
		}
		out = append(out, &v)
	}
	return out, rows.Err()
}

func (d *DB) LoadSpendLimits(ctx context.Context) ([]*SpendLimit, error) {
	return loadSpendLimits(ctx, d.pool)
}

func loadSpendLimits(ctx context.Context, q querier) ([]*SpendLimit, error) {
	rows, err := q.Query(ctx,
		`SELECT subject_type, subject_id, period, limit_nanousd
		 FROM spend_limit ORDER BY subject_type, subject_id, period`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*SpendLimit
	for rows.Next() {
		var l SpendLimit
		if err := rows.Scan(&l.SubjectType, &l.SubjectID, &l.Period, &l.LimitNanoUSD); err != nil {
			return nil, err
		}
		out = append(out, &l)
	}
	return out, rows.Err()
}

func UpsertSpendLimit(ctx context.Context, tx pgx.Tx, l *SpendLimit) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO spend_limit (subject_type, subject_id, period, limit_nanousd, updated_at)
		 VALUES ($1, $2, $3, $4, NOW())
		 ON CONFLICT (subject_type, subject_id, period) DO UPDATE SET
		   limit_nanousd = $4, updated_at = NOW()`,
		l.SubjectType, l.SubjectID, l.Period, l.LimitNanoUSD)
	return err
}

func DeleteSpendLimit(ctx context.Context, tx pgx.Tx, subjectType, subjectID, period string) error {
	tag, err := tx.Exec(ctx,
		`DELETE FROM spend_limit WHERE subject_type = $1 AND subject_id = $2 AND period = $3`,
		subjectType, subjectID, period)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- organizations and teams ---

func (d *DB) CreateOrganization(ctx context.Context, org *Organization) error {
	_, err := d.pool.Exec(ctx,
		`INSERT INTO organization (id, name, created_at) VALUES ($1, $2, $3)`,
		org.ID, org.Name, org.CreatedAt)
	return err
}

func (d *DB) ListOrganizations(ctx context.Context) ([]*Organization, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT id, name, created_at FROM organization ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Organization
	for rows.Next() {
		var o Organization
		if err := rows.Scan(&o.ID, &o.Name, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}

func (d *DB) GetOrganization(ctx context.Context, id string) (*Organization, error) {
	var o Organization
	err := d.pool.QueryRow(ctx,
		`SELECT id, name, created_at FROM organization WHERE id = $1`, id).
		Scan(&o.ID, &o.Name, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// DeleteOrganizationTx removes an organization, its teams (cascade) and their
// spend limits, detaching disabled keys like DeleteTeamTx; active keys refuse it.
func DeleteOrganizationTx(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := tx.Exec(ctx,
		`UPDATE api_key SET team_id = NULL, org_id = NULL WHERE org_id = $1 AND disabled_at IS NOT NULL`, id); err != nil {
		return err
	}
	if err := refuseActiveKeys(ctx, tx, `org_id = $1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM spend_limit WHERE subject_type = 'team' AND subject_id IN (SELECT id::text FROM team WHERE org_id = $1)`, id); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM organization WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetTeamBudgetTx makes the team's monthly spend limit match its budget: the
// team row and spend_limit change together, so they cannot drift apart.
func SetTeamBudgetTx(ctx context.Context, tx pgx.Tx, teamID string, limitNanoUSD int64) error {
	if limitNanoUSD > 0 {
		return UpsertSpendLimit(ctx, tx, &SpendLimit{SubjectType: "team", SubjectID: teamID, Period: "month", LimitNanoUSD: limitNanoUSD})
	}
	_, err := tx.Exec(ctx, `DELETE FROM spend_limit WHERE subject_type = 'team' AND subject_id = $1`, teamID)
	return err
}

func CreateTeamTx(ctx context.Context, tx pgx.Tx, team *Team) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO team (id, org_id, name, monthly_budget_usd, allowed_models, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		team.ID, team.OrgID, team.Name, team.MonthlyBudgetUSD, team.AllowedModels, team.CreatedAt)
	return err
}

func (d *DB) GetTeam(ctx context.Context, id string) (*Team, error) {
	var t Team
	err := d.pool.QueryRow(ctx,
		`SELECT id, org_id, name, monthly_budget_usd, allowed_models, created_at
		 FROM team WHERE id = $1`, id).
		Scan(&t.ID, &t.OrgID, &t.Name, &t.MonthlyBudgetUSD, &t.AllowedModels, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (d *DB) ListTeams(ctx context.Context, orgID string) ([]*Team, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if orgID != "" {
		rows, err = d.pool.Query(ctx,
			`SELECT id, org_id, name, monthly_budget_usd, allowed_models, created_at
			 FROM team WHERE org_id = $1 ORDER BY created_at DESC`, orgID)
	} else {
		rows, err = d.pool.Query(ctx,
			`SELECT id, org_id, name, monthly_budget_usd, allowed_models, created_at
			 FROM team ORDER BY created_at DESC`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Team
	for rows.Next() {
		var t Team
		if err := rows.Scan(&t.ID, &t.OrgID, &t.Name, &t.MonthlyBudgetUSD, &t.AllowedModels, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

func UpdateTeamTx(ctx context.Context, tx pgx.Tx, team *Team) error {
	tag, err := tx.Exec(ctx,
		`UPDATE team SET name = $2, monthly_budget_usd = $3, allowed_models = $4
		 WHERE id = $1`,
		team.ID, team.Name, team.MonthlyBudgetUSD, team.AllowedModels)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteTeamTx removes a team and its spend limits. Disabled keys are detached
// (keys are never deleted, so they would pin the team forever); active keys
// make it ErrTenantInUse: revoke or move them first.
func DeleteTeamTx(ctx context.Context, tx pgx.Tx, id string) error {
	if _, err := tx.Exec(ctx,
		`UPDATE api_key SET team_id = NULL, org_id = NULL WHERE team_id = $1 AND disabled_at IS NOT NULL`, id); err != nil {
		return err
	}
	if err := refuseActiveKeys(ctx, tx, `team_id = $1`, id); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM team WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	_, err = tx.Exec(ctx, `DELETE FROM spend_limit WHERE subject_type = 'team' AND subject_id = $1`, id)
	return err
}

func refuseActiveKeys(ctx context.Context, tx pgx.Tx, where, id string) error {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM api_key WHERE `+where, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: %d active api key(s)", ErrTenantInUse, n)
	}
	return nil
}
