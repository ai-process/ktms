package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/crypto"
	"github.com/ai-process/ktms/internal/grpcapi"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/server"
	"github.com/ai-process/ktms/migrations"
)

const bootstrapKey = "cli-test-admin"

// keystone serves the real Admin API, with its auth interceptor, over a fresh
// database and returns its address.
func keystone(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		base = "postgres://gen:gen@localhost:5442/gen_test?sslmode=disable"
	}
	skip := t.Skipf
	if os.Getenv("REQUIRE_DB") != "" {
		skip = t.Fatalf
	}
	admin, err := pgxpool.New(ctx, base)
	if err != nil {
		skip("postgres: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		skip("postgres: %v", err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "cli_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	cfg, _ := pgxpool.ParseConfig(base)
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close()
	})
	if err := proxydb.Migrate(ctx, pool, migrations.FS()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	ring, err := crypto.ParseKeyring("1:" + base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	db := proxydb.New(pool)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	// Each change reloads routing at once, so route explain sees it.
	loader := registry.NewLoader(db, ring, nil)
	adminSrv := grpcapi.NewAdminServer(db, ring, func() { _ = loader.Load(context.Background()) })
	adminSrv.SetRouteExplainer(grpcapi.NewProxyServer(loader, nil).ExplainRoute)
	srv := server.NewGRPCServer(pb.UnimplementedGatewayServiceServer{}, adminSrv, nil,
		apikeys.NewVerifier(db, bootstrapKey), addr, 0)
	go func() { _ = srv.Start() }()
	t.Cleanup(srv.Stop)
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return addr
}

func callIn(stdin string, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestAgainstKeystone(t *testing.T) {
	addr := keystone(t)
	t.Setenv("KTMS_CONFIG", filepath.Join(t.TempDir(), "config.yaml"))
	t.Setenv("KTMS_ADDR", addr)
	t.Setenv("KTMS_ADMIN_KEY", bootstrapKey)
	t.Setenv("TEST_OPENAI_KEY", "sk-test")
	must := func(want int, stdin string, args ...string) string {
		t.Helper()
		code, out, errs := callIn(stdin, args...)
		if code != want {
			t.Fatalf("ktms %s = %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, want, out, errs)
		}
		return out
	}

	must(0, "", "config", "apply", "--yes", "-f", "../../schema/example.yaml")
	if out := must(0, "", "config", "plan", "--detailed-exitcode", "-f", "../../schema/example.yaml"); !strings.Contains(out, "No changes") {
		t.Fatalf("plan after apply: %s", out)
	}
	if out := must(0, "", "models", "list"); !strings.Contains(out, "gpt-4o-mini") || !strings.Contains(out, "openai") {
		t.Fatalf("models list: %s", out)
	}
	if out := must(0, "", "rules", "list"); !strings.Contains(out, "PRIORITY") {
		t.Fatalf("rules list: %s", out)
	}
	if out := must(0, "", "teams", "list"); !strings.Contains(out, "acme/search") {
		t.Fatalf("teams list: %s", out)
	}

	// A minted key is printed once and never listed.
	plain := strings.TrimSpace(must(0, "", "keys", "mint", "bot", "--team", "acme/search", "--rpm", "30"))
	if !strings.HasPrefix(plain, "ktms_") {
		t.Fatalf("mint printed %q", plain)
	}
	out := must(0, "", "keys", "list", "-o", "json")
	if strings.Contains(out, plain) {
		t.Fatal("keys list shows a key")
	}
	var keys []map[string]any
	if err := json.Unmarshal([]byte(out), &keys); err != nil {
		t.Fatalf("keys list -o json: %v\n%s", err, out)
	}
	if out := must(0, "", "keys", "list"); !strings.Contains(out, "acme/search") {
		t.Fatalf("keys list: %s", out)
	}
	if out := must(0, "", "keys", "list", "-o", "yaml"); !strings.Contains(out, "name: bot") {
		t.Fatalf("keys list -o yaml: %s", out)
	}
	must(0, "", "keys", "policy", "set", "bot", "--tpm", "1000")
	must(0, "sk-vendor\n", "vendor-keys", "set", "bot", "openai")
	if out := must(0, "", "vendor-keys", "list"); !strings.Contains(out, crypto.Fingerprint("sk-vendor")) || strings.Contains(out, "sk-vendor") {
		t.Fatalf("vendor-keys list: %s", out)
	}

	// bot holds an openai key now and its team allows gpt-4o-mini only.
	if out := must(0, "", "route", "explain", "bot", "--effort", "low"); !strings.Contains(out, "rule: default") || !strings.Contains(out, "tries: gpt-4o-mini") {
		t.Fatalf("route explain:\n%s", out)
	}
	if out := must(0, "", "route", "explain", "search-bot", "--effort", "low", "-o", "json"); !strings.Contains(out, `"dropped": "no_vendor_key"`) || !strings.Contains(out, `"error": "FAILED_PRECONDITION: no_vendor_key"`) {
		t.Fatalf("route explain for a key without vendor keys:\n%s", out)
	}
	must(exitNotFound, "", "route", "explain", "ghost", "--effort", "low")
	must(exitUsage, "", "route", "explain", "bot", "--effort", "extreme")

	must(0, "", "limits", "set", "key", "bot", "day", "2.5")
	if out := must(0, "", "limits", "list"); !strings.Contains(out, "$2.5000") {
		t.Fatalf("limits list: %s", out)
	}
	// This server has no limits snapshot or Redis; scripts/cli-e2e.sh checks real spend.
	if out := must(0, "", "spend", "get", "key", "bot"); !strings.Contains(out, "day") {
		t.Fatalf("spend get: %s", out)
	}
	must(0, "", "spend", "get", "team", "acme/search")
	must(0, "", "limits", "delete", "key", "bot", "day")

	node := strings.TrimSpace(must(0, "", "node-keys", "mint", "gpu-1", "--node-id", "gpu-1", "--allowed-cidrs", "10.0.0.0/8"))
	if node == "" {
		t.Fatal("node key not printed")
	}
	must(0, "", "node-keys", "revoke", "gpu-1", "--yes")
	must(0, "", "nodes", "list")

	// Exit codes by class.
	must(exitUsage, "", "limits", "set", "key", "bot", "day", "-1")
	must(exitRejected, "", "keys", "mint", "bad", "--scopes", "root")
	must(exitNotFound, "", "keys", "revoke", "ghost", "--yes")
	must(exitUsage, "", "spend", "get", "nobody", "x")
	must(exitUsage, "", "keys", "revoke", "bot") // not a terminal and no --yes
	must(0, "", "keys", "revoke", "bot", "--yes")
	t.Setenv("KTMS_ADMIN_KEY", "ktms_wrong")
	must(exitAuth, "", "models", "list")

	// A read key reads and is refused on a change.
	t.Setenv("KTMS_ADMIN_KEY", bootstrapKey)
	reader := strings.TrimSpace(must(0, "", "keys", "mint", "viewer", "--scopes", "admin:read"))
	t.Setenv("KTMS_ADMIN_KEY", reader)
	must(0, "", "config", "export")
	must(exitAuth, "", "keys", "mint", "other")
}

func TestInitAgainstKeystone(t *testing.T) {
	addr := keystone(t)
	dir := t.TempDir()
	t.Setenv("KTMS_CONFIG", filepath.Join(dir, "config.yaml"))
	t.Setenv("XDG_CONFIG_HOME", dir)
	for _, v := range initVendors {
		t.Setenv(v.env, "")
	}
	file := filepath.Join(dir, "ktms.yaml")

	// address, TLS, HTTP, admin key, where to keep it, OpenAI, Gemini, Anthropic, key name, apply
	answers := strings.Join([]string{addr, "n", "", bootstrapKey, "file", "sk-init", "", "", "", "y"}, "\n") + "\n"
	code, out, errs := callIn(answers, "init", "dev", "-f", file)
	if code != 0 {
		t.Fatalf("init = %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
	if strings.Contains(out, "sk-init") || strings.Contains(errs, "sk-init") {
		t.Fatal("init printed the vendor key")
	}
	if !strings.Contains(out, "ktms_") || !strings.Contains(out, "Kept the client key in file:") {
		t.Fatalf("init output:\n%s", out)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, "gpt-4o-mini") || strings.Contains(s, "gemini") || strings.Contains(s, "vendor_keys") || !strings.Contains(s, "name: my-app") {
		t.Fatalf("written file:\n%s", s)
	}

	must := func(args ...string) string {
		t.Helper()
		code, out, errs := callIn("", args...)
		if code != 0 {
			t.Fatalf("ktms %s = %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, out, errs)
		}
		return out
	}
	// The context is current and its keys work without anything in the shell.
	if out := must("config", "plan", "--detailed-exitcode", "-f", file); !strings.Contains(out, "No changes") {
		t.Fatalf("plan after init: %s", out)
	}
	if out := must("vendor-keys", "list"); !strings.Contains(out, crypto.Fingerprint("sk-init")) {
		t.Fatalf("vendor-keys list: %s", out)
	}
	if out := must("route", "explain", "my-app", "--effort", "low"); !strings.Contains(out, "tries: gpt-4o-mini") {
		t.Fatalf("route explain:\n%s", out)
	}
	if out := must("context", "list"); !strings.Contains(out, "* ") || !strings.Contains(out, "dev.client.key") {
		t.Fatalf("context list: %s", out)
	}

	// Run again: the context is reused and the live configuration exported.
	code, out, errs = callIn("y\ny\n", "init", "dev", "-f", file)
	if code != 0 || !strings.Contains(out, "already") {
		t.Fatalf("second init = %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
	if out := must("config", "plan", "--detailed-exitcode", "-f", file); !strings.Contains(out, "No changes") {
		t.Fatalf("plan of the exported file: %s", out)
	}
}
