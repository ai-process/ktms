package declarative_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/crypto"
	"github.com/ai-process/ktms/internal/declarative"
	"github.com/ai-process/ktms/internal/grpcapi"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/migrations"
)

// keystone serves a real AdminServer over a fresh database of its own, so
// tests do not share configuration with each other or other packages.
func keystone(t *testing.T) pb.AdminServiceClient {
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
	name := "decl_" + hex.EncodeToString(b)
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
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pb.RegisterAdminServiceServer(srv, grpcapi.NewAdminServer(proxydb.New(pool), ring, nil))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///keystone",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewAdminServiceClient(conn)
}

const config = `api_version: ktms.ai-process.dev/v1alpha1
models:
  - id: gpt-4o-mini
    vendor: openai
    efforts: [EFFORT_LOW, EFFORT_MEDIUM, EFFORT_HIGH]
    price_in_per_mtok: 0.15
    price_out_per_mtok: 0.6
  - id: claude-x
    vendor: anthropic
    efforts: [EFFORT_HIGH]
    price_in_per_mtok: 3
    price_out_per_mtok: 15
rules:
  - {name: high, priority: 20, effort: EFFORT_HIGH, use: [claude-x, gpt-4o-mini]}
  - {name: default, priority: 1, use: [gpt-4o-mini]}
organizations:
  - name: acme
    teams:
      - {name: search, monthly_budget_usd: 50, allowed_models: [gpt-4o-mini]}
api_keys:
  - name: billing
    policy: {rpm_limit: 60, allowed_models: [gpt-4o-mini]}
  - {name: search-bot, organization: acme, team: search}
vendor_keys:
  - {api_key_name: billing, vendor: openai, plain_key_env: T_OPENAI}
spend_limits:
  - {subject_type: key, subject_id: billing, period: day, limit_usd: 2.5}
user_rate_limits:
  - {user_id: "*", model_id: claude-x, rpm: 10}
`

func plan(t *testing.T, c pb.AdminServiceClient, cfg *declarative.Config, env map[string]string, prune bool) (*declarative.State, *declarative.Plan) {
	t.Helper()
	live, err := declarative.Fetch(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	p, err := declarative.Build(cfg, live, declarative.Options{Prune: prune, Env: func(k string) (string, bool) { v, ok := env[k]; return v, ok }})
	if err != nil {
		t.Fatal(err)
	}
	return live, p
}

func show(p *declarative.Plan) string {
	var b bytes.Buffer
	p.Write(&b)
	return b.String()
}

func TestApplyThenPlanIsEmpty(t *testing.T) {
	c := keystone(t)
	cfg, err := declarative.Parse([]byte(config))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"T_OPENAI": "sk-test-1"}
	live, p := plan(t, c, cfg, env, false)
	res, err := declarative.Apply(context.Background(), c, live, p, nil)
	if err != nil {
		t.Fatalf("apply: %v\n%s", err, show(p))
	}
	if !strings.HasPrefix(res.Minted["billing"], "ktms_") || !strings.HasPrefix(res.Minted["search-bot"], "ktms_") {
		t.Fatalf("minted = %v", res.Minted)
	}
	if _, p := plan(t, c, cfg, env, true); !p.Empty() {
		t.Fatalf("second plan not empty:\n%s", show(p))
	}

	// A changed vendor key and price show up, and apply settles them.
	env["T_OPENAI"] = "sk-test-2"
	cfg.Models[0].PriceInPerMtok = 0.2
	live, p = plan(t, c, cfg, env, false)
	out := show(p)
	if !strings.Contains(out, "~ vendor key billing/openai") || !strings.Contains(out, "price_in_per_mtok 0.15 → 0.2") {
		t.Fatalf("plan:\n%s", out)
	}
	if _, err := declarative.Apply(context.Background(), c, live, p, nil); err != nil {
		t.Fatal(err)
	}
	if _, p := plan(t, c, cfg, env, false); !p.Empty() {
		t.Fatalf("plan after the change not empty:\n%s", show(p))
	}

	// Prune removes what the file dropped, rules first so the model can go.
	cfg2, _ := declarative.Parse([]byte(strings.Replace(strings.Replace(config,
		"  - {name: high, priority: 20, effort: EFFORT_HIGH, use: [claude-x, gpt-4o-mini]}\n", "", 1),
		`  - id: claude-x
    vendor: anthropic
    efforts: [EFFORT_HIGH]
    price_in_per_mtok: 3
    price_out_per_mtok: 15
`, "", 1)))
	if cfg2 == nil {
		t.Fatal("trimmed config does not parse")
	}
	cfg2.UserRateLimits = nil
	cfg2.Models[0].PriceInPerMtok = 0.2
	live, p = plan(t, c, cfg2, env, true)
	if !strings.Contains(show(p), "- model claude-x") {
		t.Fatalf("prune plan:\n%s", show(p))
	}
	if _, err := declarative.Apply(context.Background(), c, live, p, nil); err != nil {
		t.Fatalf("prune apply: %v\n%s", err, show(p))
	}
	if _, p := plan(t, c, cfg2, env, true); !p.Empty() {
		t.Fatalf("plan after prune not empty:\n%s", show(p))
	}
}

// Export from one Keystone and apply to a clean one gives the same configuration.
func TestExportRestoresOnACleanKeystone(t *testing.T) {
	src, dst := keystone(t), keystone(t)
	cfg, _ := declarative.Parse([]byte(config))
	env := map[string]string{"T_OPENAI": "sk-test-1"}
	live, p := plan(t, src, cfg, env, false)
	if _, err := declarative.Apply(context.Background(), src, live, p, nil); err != nil {
		t.Fatal(err)
	}
	export := func(c pb.AdminServiceClient) string {
		live, err := declarative.Fetch(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if err := declarative.Export(live, &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	first := export(src)
	back, err := declarative.Parse([]byte(first))
	if err != nil {
		t.Fatalf("export does not parse: %v\n%s", err, first)
	}
	exported := map[string]string{"KTMS_VENDOR_KEY_BILLING_OPENAI": "sk-test-1"}
	live, p = plan(t, dst, back, exported, false)
	if _, err := declarative.Apply(context.Background(), dst, live, p, nil); err != nil {
		t.Fatalf("restore: %v\n%s", err, show(p))
	}
	if second := export(dst); second != first {
		t.Fatalf("restored configuration differs:\n--- source\n%s\n--- restored\n%s", first, second)
	}
}
