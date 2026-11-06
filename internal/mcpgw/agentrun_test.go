package mcpgw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ai-process/ktms/internal/agentrun"
	"github.com/ai-process/ktms/internal/agents"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/migrations"
)

type scripted struct{ prompts []string }

func (s *scripted) Run(_ context.Context, prompt string) (string, error) {
	s.prompts = append(s.prompts, prompt)
	if len(s.prompts) == 1 {
		return "Which branch?\nSTATUS: input-required", nil
	}
	return "Reviewed main: LGTM.\nSTATUS: completed", nil
}

// ktms agents run answers a real inbox through the gateway: a question, the
// sender's answer, then the result.
func TestAgentRunThroughGateway(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgres://gen:gen@localhost:5442/gen_test?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		if os.Getenv("REQUIRE_DB") != "" {
			t.Fatalf("no test postgres: %v", err)
		}
		t.Skipf("no test postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := proxydb.Migrate(ctx, pool, migrations.FS()); err != nil {
		t.Fatal(err)
	}
	ids := verifier{}
	mint := func(name string) (string, *apikeys.Identity) {
		id := uuid.NewString()
		name = name + "-" + id[:8]
		if _, err := pool.Exec(ctx, `INSERT INTO api_key (id, name, key_lookup, key_hash, scopes) VALUES ($1, $2, $3, $3, '{mcp}')`, id, name, "lookup-"+id); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM api_key WHERE id = $1`, id) })
		ident := &apikeys.Identity{KeyID: id, Name: name, Scopes: []string{apikeys.ScopeMCP}, KeyAllowedMCP: []string{"agents"}}
		ids["plain-"+name] = ident
		return "plain-" + name, ident
	}
	_, alice := mint("alice")
	bobKey, bob := mint("bob")

	svc := agents.New(proxydb.New(pool), agents.Config{}, nil, nil)
	sn := &snaps{}
	sn.s.Store(&registry.Snapshot{})
	gw := New(sn, ids, nil, &recorder{}, "test").WithAgents(svc, 0)
	t.Cleanup(gw.Close)
	mux := http.NewServeMux()
	gw.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	if _, err := svc.Me(ctx, bob); err != nil {
		t.Fatal(err)
	}
	task, err := svc.Send(ctx, alice, strings.ToLower(bob.Name), "review the agents sidecar", "")
	if err != nil {
		t.Fatal(err)
	}
	c := &agentrun.Client{Endpoint: ts.URL + "/mcp/agents", Key: bobKey}
	t.Cleanup(func() { _ = c.Close() })
	r := &scripted{}
	cfg := agentrun.Config{Timeout: 5 * time.Second}

	if err := agentrun.Once(ctx, c, r, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(ctx, alice, task.ID)
	if err != nil || got.Status != proxydb.TaskInputRequired || got.Messages[len(got.Messages)-1].Text != "Which branch?" {
		t.Fatalf("after the question: %+v, %v", got, err)
	}
	if !strings.Contains(r.prompts[0], "review the agents sidecar") {
		t.Fatalf("prompt = %s", r.prompts[0])
	}
	// Waiting for the sender: nothing runs.
	if err := agentrun.Once(ctx, c, r, cfg); err != nil || len(r.prompts) != 1 {
		t.Fatalf("ran while waiting for the sender: %d, %v", len(r.prompts), err)
	}
	if _, err := svc.Reply(ctx, alice, task.ID, "main", proxydb.TaskWorking); err != nil {
		t.Fatal(err)
	}
	if err := agentrun.Once(ctx, c, r, cfg); err != nil {
		t.Fatal(err)
	}
	got, err = svc.Get(ctx, alice, task.ID)
	if err != nil || got.Status != proxydb.TaskCompleted || got.Messages[len(got.Messages)-1].Text != "Reviewed main: LGTM." {
		t.Fatalf("after the answer: %+v, %v", got, err)
	}
	if !strings.Contains(r.prompts[1], "Which branch?") || !strings.Contains(r.prompts[1], "main") {
		t.Fatalf("second prompt lacks the conversation: %s", r.prompts[1])
	}
}
