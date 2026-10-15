package proxydb

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestSpendLimitCRUD(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	clean := func() {
		if _, err := db.pool.Exec(context.Background(), `DELETE FROM spend_limit WHERE subject_id LIKE 'test-%'`); err != nil {
			t.Errorf("clean spend_limit: %v", err)
		}
	}
	clean()
	t.Cleanup(clean)

	upsert := func(l *SpendLimit) error {
		_, err := db.Mutate(ctx, func(tx pgx.Tx) error { return UpsertSpendLimit(ctx, tx, l) }, nil)
		return err
	}
	if err := upsert(&SpendLimit{"key", "test-k", "day", 5}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := upsert(&SpendLimit{"key", "test-k", "day", 9}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := upsert(&SpendLimit{"user", "test-u", "month", 3}); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	got := map[[3]string]int64{}
	limits, err := db.LoadSpendLimits(ctx)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, l := range limits {
		got[[3]string{l.SubjectType, l.SubjectID, l.Period}] = l.LimitNanoUSD
	}
	if got[[3]string{"key", "test-k", "day"}] != 9 || got[[3]string{"user", "test-u", "month"}] != 3 {
		t.Fatalf("loaded %v", got)
	}

	// The schema refuses what the admin API would refuse anyway.
	for _, bad := range []*SpendLimit{
		{"org", "test-x", "day", 1},
		{"key", "test-x", "week", 1},
		{"key", "test-x", "day", 0},
		{"key", "", "day", 1},
	} {
		err := upsert(bad)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("%+v: err = %v, want check_violation", bad, err)
		}
	}

	del := func(subject, id, period string) error {
		_, err := db.Mutate(ctx, func(tx pgx.Tx) error { return DeleteSpendLimit(ctx, tx, subject, id, period) }, nil)
		return err
	}
	if err := del("key", "test-k", "day"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := del("key", "test-k", "day"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v, want ErrNotFound", err)
	}
}
