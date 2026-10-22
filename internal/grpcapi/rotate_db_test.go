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
	"github.com/ai-process/ktms/migrations"
)

func rotateTestDB(t *testing.T) *proxydb.DB {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://gen:gen@localhost:5442/gen_test?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Skipf("no test postgres: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("no test postgres: %v", err)
	}
	if err := proxydb.Migrate(ctx, pool, migrations.FS()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)
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
