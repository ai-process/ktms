package mcpgw

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ai-process/ktms/internal/agents"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/migrations"
)

// agentsFixture is a gateway with the real agents service on the test Postgres.
type agentsFixture struct {
	svc  *agents.Service
	ts   *httptest.Server
	ids  verifier
	mint func(name string, allowed ...string) (string, *apikeys.Identity)
}

func newAgentsFixture(t *testing.T) *agentsFixture {
	t.Helper()
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
	f := &agentsFixture{ids: verifier{}}
	f.mint = func(name string, allowed ...string) (string, *apikeys.Identity) {
		id := uuid.NewString()
		name = name + "-" + id[:8]
		if _, err := pool.Exec(ctx, `INSERT INTO api_key (id, name, key_lookup, key_hash, scopes) VALUES ($1, $2, $3, $3, '{mcp}')`, id, name, "lookup-"+id); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM api_key WHERE id = $1`, id) })
		if len(allowed) == 0 {
			allowed = []string{"agents"}
		}
		ident := &apikeys.Identity{KeyID: id, Name: name, Scopes: []string{apikeys.ScopeMCP}, KeyAllowedMCP: allowed}
		f.ids["plain-"+name] = ident
		return "plain-" + name, ident
	}
	f.svc = agents.New(proxydb.New(pool), agents.Config{}, nil, nil)
	sn := &snaps{}
	sn.s.Store(&registry.Snapshot{})
	gw := New(sn, f.ids, nil, &recorder{}, "test").WithAgents(f.svc, 0)
	t.Cleanup(gw.Close)
	mux := http.NewServeMux()
	gw.Register(mux)
	f.ts = httptest.NewServer(mux)
	t.Cleanup(f.ts.Close)
	return f
}

type rpcReply struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func (f *agentsFixture) rpc(t *testing.T, key, agent, method string, params any) (a2aTask, *rpcError) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, f.ts.URL+"/a2a/"+agent, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%s: HTTP %d", method, res.StatusCode)
	}
	var r rpcReply
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	var task a2aTask
	if r.Error == nil {
		if err := json.Unmarshal(r.Result, &task); err != nil {
			t.Fatal(err)
		}
	}
	return task, r.Error
}

func a2aText(m string) map[string]any {
	return map[string]any{"role": "user", "messageId": uuid.NewString(), "parts": []map[string]any{{"kind": "text", "text": m}}}
}

func TestA2ABridge(t *testing.T) {
	f := newAgentsFixture(t)
	ctx := context.Background()
	aliceKey, _ := f.mint("alice")
	_, bob := f.mint("bob")
	b := strings.ToLower(bob.Name)
	if _, err := f.svc.Me(ctx, bob); err != nil {
		t.Fatal(err)
	}

	task, rerr := f.rpc(t, aliceKey, b, "message/send", map[string]any{"message": a2aText("review PR 12")})
	if rerr != nil || task.Kind != "task" || task.Status.State != "submitted" || task.Metadata["to"] != b || task.History[0].Parts[0].Text != "review PR 12" {
		t.Fatalf("send = %+v, %+v", task, rerr)
	}
	if _, err := f.svc.Reply(ctx, bob, task.ID, "which branch?", proxydb.TaskInputRequired); err != nil {
		t.Fatal(err)
	}
	got, rerr := f.rpc(t, aliceKey, b, "tasks/get", map[string]any{"id": task.ID})
	if rerr != nil || got.Status.State != "input-required" || got.Status.Message == nil || got.Status.Message.Parts[0].Text != "which branch?" {
		t.Fatalf("get = %+v, %+v", got, rerr)
	}
	// A message on the task answers the question.
	m := a2aText("main")
	m["taskId"] = task.ID
	got, rerr = f.rpc(t, aliceKey, b, "message/send", map[string]any{"message": m})
	if rerr != nil || got.Status.State != "working" || len(got.History) != 3 {
		t.Fatalf("answer = %+v, %+v", got, rerr)
	}
	if got, _ = f.rpc(t, aliceKey, b, "tasks/get", map[string]any{"id": task.ID, "historyLength": 1}); len(got.History) != 1 || got.History[0].Parts[0].Text != "main" {
		t.Fatalf("historyLength 1 = %+v", got.History)
	}

	// Blocking send waits for the answer.
	a2aPoll = 20 * time.Millisecond
	go func() {
		time.Sleep(100 * time.Millisecond)
		inbox, _ := f.svc.Inbox(ctx, bob, false, 10)
		for _, tk := range inbox {
			if tk.Status == proxydb.TaskSubmitted {
				_, _ = f.svc.Reply(ctx, bob, tk.ID, "done", proxydb.TaskCompleted)
			}
		}
	}()
	done, rerr := f.rpc(t, aliceKey, b, "message/send", map[string]any{"message": a2aText("second"), "configuration": map[string]any{"blocking": true}})
	if rerr != nil || done.Status.State != "completed" || done.Status.Message.Parts[0].Text != "done" {
		t.Fatalf("blocking send = %+v, %+v", done, rerr)
	}

	if got, rerr = f.rpc(t, aliceKey, b, "tasks/cancel", map[string]any{"id": task.ID}); rerr != nil || got.Status.State != "canceled" {
		t.Fatalf("cancel = %+v, %+v", got, rerr)
	}
	if _, rerr = f.rpc(t, aliceKey, b, "tasks/cancel", map[string]any{"id": task.ID}); rerr == nil || rerr.Code != codeNotCancelable {
		t.Fatalf("cancel an ended task: %+v", rerr)
	}
	if _, rerr = f.rpc(t, aliceKey, b, "tasks/get", map[string]any{"id": uuid.NewString()}); rerr == nil || rerr.Code != codeTaskNotFound {
		t.Fatalf("unknown task: %+v", rerr)
	}
	if _, rerr = f.rpc(t, aliceKey, "someone-else", "tasks/get", map[string]any{"id": task.ID}); rerr == nil || rerr.Code != codeTaskNotFound {
		t.Fatalf("a task through another agent's endpoint: %+v", rerr)
	}
	if _, rerr = f.rpc(t, aliceKey, b, "message/stream", map[string]any{"message": a2aText("x")}); rerr == nil || rerr.Code != codeUnsupported {
		t.Fatalf("stream: %+v", rerr)
	}
	if _, rerr = f.rpc(t, aliceKey, b, "nope", nil); rerr == nil || rerr.Code != codeMethodNotFound {
		t.Fatalf("unknown method: %+v", rerr)
	}
	file := map[string]any{"role": "user", "messageId": "1", "parts": []map[string]any{{"kind": "file", "file": map[string]any{"uri": "https://x"}}}}
	if _, rerr = f.rpc(t, aliceKey, b, "message/send", map[string]any{"message": file}); rerr == nil || rerr.Code != codeContentTypeNotS {
		t.Fatalf("file part: %+v", rerr)
	}
}

func TestA2ACardAndAccess(t *testing.T) {
	f := newAgentsFixture(t)
	ctx := context.Background()
	aliceKey, _ := f.mint("alice")
	_, bob := f.mint("bob")
	toolsKey, _ := f.mint("tools", "github")
	b := strings.ToLower(bob.Name)
	if _, err := f.svc.Register(ctx, bob, agents.Card{Description: "reviews Go", Skills: []string{"code-review"}}); err != nil {
		t.Fatal(err)
	}
	get := func(key, path string, hdr map[string]string) (*http.Response, map[string]any) {
		req, _ := http.NewRequest(http.MethodGet, f.ts.URL+path, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res, out
	}
	res, card := get(aliceKey, "/a2a/"+b+"/.well-known/agent-card.json", map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "ktms.example.com"})
	if res.StatusCode != http.StatusOK || card["url"] != "https://ktms.example.com/a2a/"+b || card["description"] != "reviews Go" || card["protocolVersion"] != A2AVersion {
		t.Fatalf("card %d = %+v", res.StatusCode, card)
	}
	if skills, _ := card["skills"].([]any); len(skills) != 1 {
		t.Fatalf("skills = %+v", card["skills"])
	}
	if res, _ := get(aliceKey, "/a2a/nobody/.well-known/agent-card.json", nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown agent's card: %d", res.StatusCode)
	}
	if res, _ := get("", "/a2a/"+b+"/.well-known/agent-card.json", nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("card without a key: %d", res.StatusCode)
	}
	if res, _ := get(toolsKey, "/a2a/"+b+"/.well-known/agent-card.json", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("card for a key without agents: %d", res.StatusCode)
	}
}
