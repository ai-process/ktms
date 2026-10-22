package proxydb

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ai-process/ktms/migrations"
)

func newTenancy(t *testing.T, d *DB, budgetNano int64) (orgID, teamID string) {
	t.Helper()
	ctx := context.Background()
	org := &Organization{ID: uuid.NewString(), Name: "org-" + uuid.NewString()[:8], CreatedAt: time.Now()}
	if err := d.CreateOrganization(ctx, org); err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	team := &Team{ID: uuid.NewString(), OrgID: org.ID, Name: "team", AllowedModels: []string{}, CreatedAt: time.Now()}
	if _, err := d.Mutate(ctx, func(tx pgx.Tx) error {
		if err := CreateTeamTx(ctx, tx, team); err != nil {
			return err
		}
		return SetTeamBudgetTx(ctx, tx, team.ID, budgetNano)
	}, nil); err != nil {
		t.Fatalf("create team: %v", err)
	}
	return org.ID, team.ID
}

func teamKey(t *testing.T, d *DB, orgID, teamID string) string {
	t.Helper()
	name := "k-" + uuid.NewString()[:8]
	if err := d.InsertAPIKey(context.Background(), &APIKey{
		ID: uuid.NewString(), Name: name, KeyLookup: uuid.NewString(), KeyHash: "h", Scopes: []string{"generate"},
		OrgID: &orgID, TeamID: &teamID,
	}); err != nil {
		t.Fatalf("InsertAPIKey: %v", err)
	}
	return name
}

func spendLimitCount(t *testing.T, d *DB, teamID string) int {
	t.Helper()
	var n int
	if err := d.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM spend_limit WHERE subject_type = 'team' AND subject_id = $1`, teamID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A team without a budget has no spend_limit row; removing a budget that is
// not there, or deleting such a team, must not fail.
func TestTeamWithoutBudgetCanBeUpdatedAndDeleted(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()
	_, teamID := newTenancy(t, d, 0)

	if _, err := d.Mutate(ctx, func(tx pgx.Tx) error { return SetTeamBudgetTx(ctx, tx, teamID, 0) }, nil); err != nil {
		t.Fatalf("clearing an absent budget: %v", err)
	}
	if _, err := d.Mutate(ctx, func(tx pgx.Tx) error { return DeleteTeamTx(ctx, tx, teamID) }, nil); err != nil {
		t.Fatalf("deleting a budget-less team: %v", err)
	}
}

func TestDeleteTeamDetachesRevokedKeysAndRefusesActiveOnes(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()
	orgID, teamID := newTenancy(t, d, 500e9)
	key := teamKey(t, d, orgID, teamID)

	_, err := d.Mutate(ctx, func(tx pgx.Tx) error { return DeleteTeamTx(ctx, tx, teamID) }, nil)
	if !errors.Is(err, ErrTenantInUse) {
		t.Fatalf("err = %v, want ErrTenantInUse while an active key remains", err)
	}
	if err := d.DisableAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Mutate(ctx, func(tx pgx.Tx) error { return DeleteTeamTx(ctx, tx, teamID) }, nil); err != nil {
		t.Fatalf("a team with only revoked keys must be deletable: %v", err)
	}
	if n := spendLimitCount(t, d, teamID); n != 0 {
		t.Fatalf("%d spend limits left behind by the deleted team", n)
	}
}

func TestDeleteOrganizationRemovesTeamSpendLimits(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()
	orgID, teamID := newTenancy(t, d, 100e9)
	if _, err := d.Mutate(ctx, func(tx pgx.Tx) error { return DeleteOrganizationTx(ctx, tx, orgID) }, nil); err != nil {
		t.Fatalf("DeleteOrganizationTx: %v", err)
	}
	if n := spendLimitCount(t, d, teamID); n != 0 {
		t.Fatalf("%d orphaned team spend limits after the organization went", n)
	}
}

// Migrations must be safe to re-run (a crash before the version is recorded).
func TestTenancyMigrationIsIdempotent(t *testing.T) {
	d := setupTestDB(t)
	body, err := fs.ReadFile(migrations.FS(), "20261019120000_organizations_teams.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.pool.Exec(context.Background(), string(body)); err != nil {
		t.Fatalf("re-running the migration: %v", err)
	}
}

// The poller bills a batch to its key's team, never to a team_id the caller
// left in the stored attributes.
func TestLeasedBatchCarriesTheKeysTeam(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()
	orgID, teamID := newTenancy(t, d, 0)
	keyID := uuid.NewString()
	if err := d.InsertAPIKey(ctx, &APIKey{ID: keyID, Name: "k-" + keyID[:8], KeyLookup: keyID, KeyHash: "h",
		Scopes: []string{"generate"}, OrgID: &orgID, TeamID: &teamID}); err != nil {
		t.Fatal(err)
	}
	batchID := uuid.NewString()
	if _, err := d.CreateBatch(ctx, &Batch{ID: batchID, APIKeyID: keyID, KeyName: "k", Model: "m", State: "PENDING",
		Attributes: map[string]string{"team_id": "forged"}, CreatedAt: time.Now().UTC()}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.pool.Exec(context.Background(), `DELETE FROM batch WHERE id = $1`, batchID) })
	leased, err := d.LeaseUnfinishedBatches(ctx, time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range leased {
		if b.ID == batchID {
			if b.TeamID != teamID || b.OrgID != orgID {
				t.Fatalf("leased batch tenant = %q/%q, want %q/%q", b.OrgID, b.TeamID, orgID, teamID)
			}
			return
		}
	}
	t.Fatal("batch not leased")
}
