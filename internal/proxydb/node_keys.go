package proxydb

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func (d *DB) GetNodeKeyByLookup(ctx context.Context, lookup string) (*NodeKey, error) {
	return scanNodeKey(d.pool.QueryRow(ctx,
		`SELECT id, node_id, name, key_lookup, key_hash, allowed_cidrs, allowed_models,
		        max_base_weight, max_concurrency, disabled_at, created_at
		 FROM node_key WHERE key_lookup = $1`, lookup))
}

func scanNodeKey(row pgx.Row) (*NodeKey, error) {
	var k NodeKey
	err := row.Scan(&k.ID, &k.NodeID, &k.Name, &k.KeyLookup, &k.KeyHash,
		&k.AllowedCIDRs, &k.AllowedModels, &k.MaxBaseWeight, &k.MaxConcurrency,
		&k.DisabledAt, &k.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

func (d *DB) InsertNodeKey(ctx context.Context, k *NodeKey) error {
	allowedCIDRs := k.AllowedCIDRs
	if allowedCIDRs == nil {
		allowedCIDRs = []string{}
	}
	allowedModels := k.AllowedModels
	if allowedModels == nil {
		allowedModels = []string{}
	}
	maxWeight := k.MaxBaseWeight
	if maxWeight <= 0 {
		maxWeight = 1000
	}
	maxConc := k.MaxConcurrency
	if maxConc <= 0 {
		maxConc = 256
	}
	err := d.pool.QueryRow(ctx,
		`INSERT INTO node_key (node_id, name, key_lookup, key_hash, allowed_cidrs, allowed_models, max_base_weight, max_concurrency)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id, created_at`,
		k.NodeID, k.Name, k.KeyLookup, k.KeyHash, allowedCIDRs, allowedModels, maxWeight, maxConc).Scan(&k.ID, &k.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrAlreadyExists
		}
		return err
	}
	k.AllowedCIDRs = allowedCIDRs
	k.AllowedModels = allowedModels
	k.MaxBaseWeight = maxWeight
	k.MaxConcurrency = maxConc
	return nil
}

func (d *DB) RevokeNodeKey(ctx context.Context, id int64) (string, error) {
	var nodeID string
	err := d.pool.QueryRow(ctx,
		`UPDATE node_key SET disabled_at = NOW() WHERE id = $1 AND disabled_at IS NULL RETURNING node_id`,
		id).Scan(&nodeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return nodeID, nil
}

func (d *DB) ListNodeKeys(ctx context.Context) ([]*NodeKey, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT id, node_id, name, key_lookup, key_hash, allowed_cidrs, allowed_models,
		        max_base_weight, max_concurrency, disabled_at, created_at
		 FROM node_key ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*NodeKey
	for rows.Next() {
		k, err := scanNodeKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
