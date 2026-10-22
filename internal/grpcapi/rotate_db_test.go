package grpcapi

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/crypto"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/migrations"
)

// rotateTestDB gives the test a database of its own: RotateEncryption scans
// every vendor key, so rows from other tests must not be in the way. With
// REQUIRE_DB set (CI), no Postgres is a failure instead of a skip.
func rotateTestDB(t *testing.T) *proxydb.DB {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://gen:gen@localhost:5442/gen_test?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	skip := t.Skipf
	if os.Getenv("REQUIRE_DB") != "" {
		skip = t.Fatalf
	}
	admin, err := pgxpool.New(ctx, url)
	if err == nil {
		err = admin.Ping(ctx)
	}
	if err != nil {
		skip("no test postgres: %v", err)
		return nil
	}
	t.Cleanup(admin.Close)
	name := "ktms_rotate_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop test database %s: %v", name, err)
		}
	})
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := proxydb.Migrate(ctx, pool, migrations.FS()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return proxydb.New(pool)
}

func keyring(t *testing.T, versions ...int) *crypto.Keyring {
	t.Helper()
	var parts []string
	for _, v := range versions {
		key := strings.Repeat(string(rune('a'+v)), 32)
		parts = append(parts, string(rune('0'+v))+":"+base64.StdEncoding.EncodeToString([]byte(key)))
	}
	k, err := crypto.ParseKeyring(strings.Join(parts, ","))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// The documented rotation: add version 2, call RotateEncryption, drop version
// 1. Vendor keys must then decrypt with version 2 alone.
func TestRotateEncryptionLetsTheOldKeyGo(t *testing.T) {
	db := rotateTestDB(t)
	ctx := context.Background()
	name := "rotate-" + uuid.NewString()[:8]
	const secret = "sk-vendor-secret"

	v1 := NewAdminServer(db, keyring(t, 1), nil)
	if _, err := v1.MintAPIKey(ctx, &pb.MintAPIKeyRequest{Name: name, Scopes: []string{"generate"}}); err != nil {
		t.Fatalf("MintAPIKey: %v", err)
	}
	if _, err := v1.SetVendorKey(ctx, &pb.SetVendorKeyRequest{ApiKeyName: name, Vendor: "openai", PlainKey: secret}); err != nil {
		t.Fatalf("SetVendorKey: %v", err)
	}

	both := NewAdminServer(db, keyring(t, 1, 2), nil)
	res, err := both.RotateEncryption(ctx, &pb.RotateEncryptionRequest{})
	if err != nil || res.GetReencrypted() < 1 {
		t.Fatalf("RotateEncryption = %v, %v", res, err)
	}
	if again, err := both.RotateEncryption(ctx, &pb.RotateEncryptionRequest{}); err != nil || again.GetReencrypted() != 0 {
		t.Fatalf("second rotation re-encrypted %v (%v); it must be a no-op", again, err)
	}

	keys, err := db.LoadVendorKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	only2 := keyring(t, 2)
	for _, vk := range keys {
		if vk.APIKeyName != name {
			continue
		}
		if vk.KeyVersion != 2 {
			t.Fatalf("vendor key still at version %d", vk.KeyVersion)
		}
		plain, err := only2.Decrypt(vk.KeyCiphertext, int(vk.KeyVersion))
		if err != nil || plain != secret {
			t.Fatalf("decrypt with version 2 alone = %q, %v", plain, err)
		}
		return
	}
	t.Fatal("vendor key not found")
}

func keyIDs(t *testing.T, db *proxydb.DB) map[string]string {
	t.Helper()
	keys, err := db.LoadVendorKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, k := range keys {
		out[k.APIKeyName] = k.APIKeyID
	}
	return out
}

// During a rotation a replica may meet a key written with a version its
// keyring lacks: it keeps the credential it already had, skips a key it never
// had, still loads the rest, and refuses to start only on a wrong keyring.
func TestLoaderAcrossARotation(t *testing.T) {
	db := rotateTestDB(t)
	ctx := context.Background()
	old := NewAdminServer(db, keyring(t, 1), nil)
	for _, name := range []string{"steady", "rotated"} {
		if _, err := old.MintAPIKey(ctx, &pb.MintAPIKeyRequest{Name: name, Scopes: []string{"generate"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := old.SetVendorKey(ctx, &pb.SetVendorKeyRequest{ApiKeyName: name, Vendor: "openai", PlainKey: name + "-v1"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := old.UpsertModel(ctx, &pb.UpsertModelRequest{Model: &pb.ModelSpec{Id: "m", Vendor: "openai", Enabled: true,
		Efforts: []pb.Effort{pb.Effort_EFFORT_LOW, pb.Effort_EFFORT_MEDIUM, pb.Effort_EFFORT_HIGH}}}); err != nil {
		t.Fatal(err)
	}
	ids := keyIDs(t, db)
	canCall := func(l *registry.Loader, name string) bool { return len(l.Snapshot().ClientModels(ids[name])) == 1 }

	running := registry.NewLoader(db, keyring(t, 1), nil)
	if err := running.Load(ctx); err != nil || !canCall(running, "rotated") {
		t.Fatalf("baseline: %v", err)
	}
	// A newer replica sets the key again with version 2.
	newer := NewAdminServer(db, keyring(t, 1, 2), nil)
	if _, err := newer.SetVendorKey(ctx, &pb.SetVendorKeyRequest{ApiKeyName: "rotated", Vendor: "openai", PlainKey: "rotated-v2"}); err != nil {
		t.Fatal(err)
	}
	if err := running.Load(ctx); err != nil || !canCall(running, "rotated") || !canCall(running, "steady") {
		t.Fatalf("a running old-keyring replica must keep its credential and its config: %v", err)
	}
	fresh := registry.NewLoader(db, keyring(t, 1), nil)
	if err := fresh.Load(ctx); err != nil || canCall(fresh, "rotated") || !canCall(fresh, "steady") {
		t.Fatalf("a fresh old-keyring replica skips only the unreadable key: %v", err)
	}
	if err := registry.NewLoader(db, keyring(t, 3), nil).Load(ctx); err == nil {
		t.Fatal("a keyring that reads no key at all must refuse to start")
	}
	// Rotation must not report success while a key needs a missing version.
	if _, err := NewAdminServer(db, keyring(t, 1, 3), nil).RotateEncryption(ctx, &pb.RotateEncryptionRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("rotation over an unreadable key = %v, want FailedPrecondition", err)
	}
}
