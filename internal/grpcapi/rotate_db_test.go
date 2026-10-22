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
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
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

// During a rotation a replica may meet a key written with a version its
// keyring lacks; that key is skipped and the rest of the config still loads.
func TestLoaderSkipsAnUnreadableVendorKey(t *testing.T) {
	db := rotateTestDB(t)
	ctx := context.Background()
	newer := NewAdminServer(db, keyring(t, 1, 2), nil)
	if _, err := newer.MintAPIKey(ctx, &pb.MintAPIKeyRequest{Name: "c", Scopes: []string{"generate"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := newer.SetVendorKey(ctx, &pb.SetVendorKeyRequest{ApiKeyName: "c", Vendor: "openai", PlainKey: "v2-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.NewLoader(db, keyring(t, 1), nil).Load(ctx); err != nil {
		t.Fatalf("a replica on the old keyring must still load its config: %v", err)
	}
}
