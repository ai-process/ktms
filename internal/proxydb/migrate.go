package proxydb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// ledgerTable records what has been applied. Named for this service so the
// ledger survives sharing a database with anything else.
const ledgerTable = "llmproxy_schema_migration"

// migrationLockKey serializes the applying replicas. A rolling deploy starts
// several at once and they would otherwise race on the same ALTER TABLE.
const migrationLockKey = "ktms:migrations"

// Migrate applies every embedded file that the ledger has not recorded yet.
//
// Each file runs in its own transaction, so a failure leaves the earlier ones
// applied and the rest pending. A failure is the caller's cue to refuse to
// serve: half-migrated is a state the code was not written for.
func Migrate(ctx context.Context, pool *pgxpool.Pool, src fs.FS) error {
	names, err := sqlFiles(src)
	if err != nil {
		return err
	}

	// One connection for the whole run: the advisory lock is session-scoped.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	// The timeouts are set per transaction (SET LOCAL) rather than on the
	// session: this connection goes back to the pool afterwards and would
	// otherwise carry a 5s lock_timeout into ordinary request traffic.
	//
	// Waiting here is the point — the other replica is mid-migration — so the
	// lock wait is bounded by the caller's context, not by lock_timeout, which
	// would apply to advisory locks too and turn a slow migration into a crash
	// loop on every other replica.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtextextended($1, 0))", migrationLockKey); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtextextended($1, 0))", migrationLockKey); err != nil {
			log.Warn().Err(err).Msg("releasing the migration lock failed; it goes with the connection")
		}
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS `+ledgerTable+` (
			name       varchar(255) PRIMARY KEY,
			checksum   varchar(64) NOT NULL,
			applied_at timestamp(0) without time zone NOT NULL DEFAULT localtimestamp
		)`); err != nil {
		return fmt.Errorf("create ledger: %w", err)
	}

	applied := map[string]string{}
	rows, err := conn.Query(ctx, "SELECT name, checksum FROM "+ledgerTable)
	if err != nil {
		return fmt.Errorf("read ledger: %w", err)
	}
	for rows.Next() {
		var name, sum string
		if err := rows.Scan(&name, &sum); err != nil {
			rows.Close()
			return fmt.Errorf("read ledger: %w", err)
		}
		applied[name] = sum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read ledger: %w", err)
	}

	pending := 0
	for _, name := range names {
		body, err := fs.ReadFile(src, name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		sum := checksum(body)
		if was, ok := applied[name]; ok {
			// Editing an applied migration is a mistake, but not one worth
			// crash-looping a live service over.
			if was != sum {
				log.Warn().Str("migration", name).Msg("applied migration has changed on disk")
			}
			continue
		}

		if err := applyOnce(ctx, conn, name, string(body), sum); err != nil {
			return err
		}
		pending++
	}

	log.Info().Int("applied", pending).Int("total", len(names)).Msg("schema up to date")
	return nil
}

// lockAttempts: a DDL that lost the race for its lock is worth a few quiet
// retries. Failing out crash-loops the pod, which works but looks like an
// incident and backs off to minutes.
const lockAttempts = 4

func applyOnce(ctx context.Context, conn *pgxpool.Conn, name, body, sum string) error {
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		err := applyInTx(ctx, conn, name, body, sum)
		if err == nil {
			log.Info().Str("migration", name).Msg("migration applied")
			return nil
		}
		if attempt >= lockAttempts || !isLockContention(err) {
			return err
		}
		log.Warn().Err(err).Str("migration", name).Dur("retry_in", backoff).
			Msg("migration could not take its lock")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 3
	}
}

func applyInTx(ctx context.Context, conn *pgxpool.Conn, name, body, sum string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin %s: %w", name, err)
	}
	// A DDL statement that cannot take its lock must fail rather than wait:
	// while it queues for ACCESS EXCLUSIVE, every later query on that table
	// queues behind it, which is how a migration turns into an outage. SET
	// LOCAL reverts with the transaction either way.
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '5s'"); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("set lock_timeout for %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '5min'"); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("set statement_timeout for %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx, body); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("apply %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO "+ledgerTable+" (name, checksum, applied_at) VALUES ($1, $2, localtimestamp)",
		name, sum,
	); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("record %s: %w", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit %s: %w", name, err)
	}
	return nil
}

// isLockContention reports the two failures that go away on their own: the
// lock_timeout above firing, and a deadlock with concurrent traffic.
func isLockContention(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "55P03" || pgErr.Code == "40P01"
}

func sqlFiles(src fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(src, ".")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func checksum(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
