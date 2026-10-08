package proxydb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"

	"github.com/ai-process/llm-proxy/internal/llm"
)

// ErrBatchAlreadyExists is returned on idempotency conflict if client resubmitted.
var ErrBatchAlreadyExists = errors.New("proxydb: batch already exists")

func (d *DB) CreateBatch(ctx context.Context, b *Batch, items []*BatchItem) (*Batch, error) {
	if b.Attributes == nil {
		b.Attributes = map[string]string{}
	}
	attrsJSON, err := json.Marshal(b.Attributes)
	if err != nil {
		return nil, fmt.Errorf("marshal attributes: %w", err)
	}

	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Idempotency check: if client_batch_id was provided, check if it already exists for this api key.
	if b.ClientBatchID != "" {
		existing, err := getBatchByClientBatchID(ctx, tx, b.APIKeyID, b.ClientBatchID)
		if err == nil {
			return existing, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}

	var query string
	if b.ClientBatchID != "" {
		query = `INSERT INTO batch (id, client_batch_id, api_key_id, key_name, model, vendor_job_name,
		                    state, total_count, done_count, failed_count, attributes,
		                    action_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		 ON CONFLICT (api_key_id, client_batch_id) WHERE client_batch_id IS NOT NULL AND client_batch_id != '' DO NOTHING`
	} else {
		query = `INSERT INTO batch (id, client_batch_id, api_key_id, key_name, model, vendor_job_name,
		                    state, total_count, done_count, failed_count, attributes,
		                    action_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`
	}
	tag, err := tx.Exec(ctx, query,
		b.ID, b.ClientBatchID, b.APIKeyID, b.KeyName, b.Model, b.VendorJobName,
		b.State, b.TotalCount, b.DoneCount, b.FailedCount, attrsJSON,
		b.ActionID, b.CreatedAt,
	)
	if err != nil {
		_ = tx.Rollback(ctx)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && b.ClientBatchID != "" {
			existing, getErr := d.GetBatchByClientBatchID(ctx, b.APIKeyID, b.ClientBatchID)
			if getErr == nil {
				return existing, nil
			}
		}
		return nil, fmt.Errorf("insert batch: %w", err)
	}
	if tag.RowsAffected() == 0 && b.ClientBatchID != "" {
		_ = tx.Rollback(ctx)
		existing, getErr := d.GetBatchByClientBatchID(ctx, b.APIKeyID, b.ClientBatchID)
		if getErr == nil {
			return existing, nil
		}
		return nil, fmt.Errorf("batch insert conflict, but failed to fetch existing batch: %w", getErr)
	}

	if len(items) > 0 {
		batch := &pgx.Batch{}
		for _, item := range items {
			batch.Queue(
				`INSERT INTO batch_item (batch_id, custom_id, position, status, response_schema, result, error, input_tokens, output_tokens)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				item.BatchID, item.CustomID, item.Position, item.Status,
				item.ResponseSchema, item.Result, item.Error, item.InputTokens, item.OutputTokens,
			)
		}
		br := tx.SendBatch(ctx, batch)
		for range items {
			if _, err := br.Exec(); err != nil {
				br.Close()
				return nil, fmt.Errorf("insert batch item: %w", err)
			}
		}
		if err := br.Close(); err != nil {
			return nil, fmt.Errorf("close batch items insert: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

func (d *DB) GetBatch(ctx context.Context, id, apiKeyID string) (*Batch, error) {
	row := d.pool.QueryRow(ctx,
		`SELECT id, client_batch_id, api_key_id, key_name, model, vendor_job_name, state,
		        total_count, done_count, failed_count, attributes, action_id,
		        created_at, completed_at, leased_until
		 FROM batch WHERE id = $1 AND api_key_id = $2`, id, apiKeyID)
	return scanBatch(row)
}

func (d *DB) GetBatchByID(ctx context.Context, id string) (*Batch, error) {
	row := d.pool.QueryRow(ctx,
		`SELECT id, client_batch_id, api_key_id, key_name, model, vendor_job_name, state,
		        total_count, done_count, failed_count, attributes, action_id,
		        created_at, completed_at, leased_until
		 FROM batch WHERE id = $1`, id)
	return scanBatch(row)
}

func (d *DB) GetBatchByClientBatchID(ctx context.Context, apiKeyID, clientBatchID string) (*Batch, error) {
	return getBatchByClientBatchID(ctx, d.pool, apiKeyID, clientBatchID)
}

func getBatchByClientBatchID(ctx context.Context, q querier, apiKeyID, clientBatchID string) (*Batch, error) {
	row := q.QueryRow(ctx,
		`SELECT id, client_batch_id, api_key_id, key_name, model, vendor_job_name, state,
		        total_count, done_count, failed_count, attributes, action_id,
		        created_at, completed_at, leased_until
		 FROM batch WHERE api_key_id = $1 AND client_batch_id = $2`, apiKeyID, clientBatchID)
	return scanBatch(row)
}

func scanBatch(row pgx.Row) (*Batch, error) {
	var b Batch
	var attrsJSON []byte
	var clientBatchID *string
	err := row.Scan(&b.ID, &clientBatchID, &b.APIKeyID, &b.KeyName, &b.Model, &b.VendorJobName, &b.State,
		&b.TotalCount, &b.DoneCount, &b.FailedCount, &attrsJSON, &b.ActionID,
		&b.CreatedAt, &b.CompletedAt, &b.LeasedUntil)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if clientBatchID != nil {
		b.ClientBatchID = *clientBatchID
	}
	if len(attrsJSON) > 0 {
		_ = json.Unmarshal(attrsJSON, &b.Attributes)
	}
	if b.Attributes == nil {
		b.Attributes = map[string]string{}
	}
	return &b, nil
}

func (d *DB) ListBatchItems(ctx context.Context, batchID string, limit, offset int) ([]*BatchItem, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT batch_id, custom_id, position, status, response_schema, result, error, input_tokens, output_tokens
		 FROM batch_item
		 WHERE batch_id = $1
		 ORDER BY position ASC
		 LIMIT $2 OFFSET $3`, batchID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []*BatchItem
	for rows.Next() {
		var item BatchItem
		if err := rows.Scan(&item.BatchID, &item.CustomID, &item.Position, &item.Status,
			&item.ResponseSchema, &item.Result, &item.Error, &item.InputTokens, &item.OutputTokens); err != nil {
			return nil, err
		}
		items = append(items, &item)
	}
	return items, rows.Err()
}

func (d *DB) ListAllBatchItems(ctx context.Context, batchID string) ([]*BatchItem, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT batch_id, custom_id, position, status, response_schema, result, error, input_tokens, output_tokens
		 FROM batch_item
		 WHERE batch_id = $1
		 ORDER BY position ASC`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var items []*BatchItem
	for rows.Next() {
		var item BatchItem
		if err := rows.Scan(&item.BatchID, &item.CustomID, &item.Position, &item.Status,
			&item.ResponseSchema, &item.Result, &item.Error, &item.InputTokens, &item.OutputTokens); err != nil {
			return nil, err
		}
		items = append(items, &item)
	}
	return items, rows.Err()
}

func (d *DB) LeaseUnfinishedBatches(ctx context.Context, leaseDuration time.Duration, limit int) ([]*Batch, error) {
	rows, err := d.pool.Query(ctx,
		`UPDATE batch
		 SET leased_until = NOW() + $1
		 WHERE id IN (
		     SELECT id FROM batch
		     WHERE state IN ('PENDING', 'RUNNING')
		       AND (leased_until IS NULL OR leased_until < NOW())
		     ORDER BY created_at ASC
		     LIMIT $2
		     FOR UPDATE SKIP LOCKED
		 )
		 RETURNING id, client_batch_id, api_key_id, key_name, model, vendor_job_name, state,
		           total_count, done_count, failed_count, attributes, action_id,
		           created_at, completed_at, leased_until`,
		leaseDuration, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var batches []*Batch
	for rows.Next() {
		var b Batch
		var attrsJSON []byte
		var clientBatchID *string
		if err := rows.Scan(&b.ID, &clientBatchID, &b.APIKeyID, &b.KeyName, &b.Model, &b.VendorJobName, &b.State,
			&b.TotalCount, &b.DoneCount, &b.FailedCount, &attrsJSON, &b.ActionID,
			&b.CreatedAt, &b.CompletedAt, &b.LeasedUntil); err != nil {
			return nil, err
		}
		if clientBatchID != nil {
			b.ClientBatchID = *clientBatchID
		}
		if len(attrsJSON) > 0 {
			_ = json.Unmarshal(attrsJSON, &b.Attributes)
		}
		batches = append(batches, &b)
	}
	return batches, rows.Err()
}

func (d *DB) UpdateBatchRunning(ctx context.Context, id string, total, done, failed int32) error {
	tag, err := d.pool.Exec(ctx,
		`UPDATE batch
		 SET state = 'RUNNING', total_count = $1, done_count = $2, failed_count = $3
		 WHERE id = $4 AND state IN ('PENDING', 'RUNNING')`,
		total, done, failed, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (d *DB) CompleteBatch(ctx context.Context, id string, finalState string, doneCount, failedCount int32, completedAt time.Time, items []*BatchItem) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE batch
		 SET state = $1, done_count = $2, failed_count = $3, completed_at = $4, leased_until = NULL
		 WHERE id = $5 AND state IN ('PENDING', 'RUNNING')`,
		finalState, doneCount, failedCount, completedAt, id)
	if err != nil {
		return fmt.Errorf("update batch final state: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	if len(items) > 0 {
		batch := &pgx.Batch{}
		for _, item := range items {
			batch.Queue(
				`UPDATE batch_item
				 SET status = $1, result = $2, error = $3, input_tokens = $4, output_tokens = $5
				 WHERE batch_id = $6 AND custom_id = $7`,
				item.Status, item.Result, item.Error, item.InputTokens, item.OutputTokens,
				item.BatchID, item.CustomID,
			)
		}
		br := tx.SendBatch(ctx, batch)
		for range items {
			if _, err := br.Exec(); err != nil {
				br.Close()
				return fmt.Errorf("update batch item: %w", err)
			}
		}
		if err := br.Close(); err != nil {
			return fmt.Errorf("close batch items update: %w", err)
		}
	}

	if finalState == "CANCELLED" {
		cancelErrBytes := llm.NewBatchItemError(codes.Canceled, "batch_cancelled", "batch was cancelled").JSON()
		if _, err := tx.Exec(ctx,
			`UPDATE batch_item
			 SET status = 'FAILED', error = $1
			 WHERE batch_id = $2 AND status = 'PENDING'`,
			cancelErrBytes, id); err != nil {
			return fmt.Errorf("fail remaining pending items on complete cancelled: %w", err)
		}
	}

	return tx.Commit(ctx)
}

func (d *DB) ExpireBatch(ctx context.Context, id string, expiredAt time.Time) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE batch
		 SET state = 'EXPIRED', completed_at = $1, leased_until = NULL
		 WHERE id = $2 AND state IN ('PENDING', 'RUNNING')`,
		expiredAt, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	expiredErrBytes := llm.NewBatchItemError(codes.DeadlineExceeded, "batch_expired", "batch expired before item completed").JSON()
	if _, err := tx.Exec(ctx,
		`UPDATE batch_item
		 SET status = 'FAILED', error = $1
		 WHERE batch_id = $2 AND status = 'PENDING'`,
		expiredErrBytes, id); err != nil {
		return fmt.Errorf("fail pending items on expire: %w", err)
	}

	return tx.Commit(ctx)
}

func (d *DB) DeleteBatchesOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := d.pool.Exec(ctx,
		`DELETE FROM batch
		 WHERE (completed_at IS NOT NULL AND completed_at < $1)
		    OR (completed_at IS NULL AND state IN ('SUCCEEDED', 'FAILED', 'CANCELLED', 'EXPIRED') AND created_at < $1)`,
		cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
