package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeAgents is an agents server whose task ends on the second outbox call.
type fakeAgents struct {
	mu        sync.Mutex
	calls     []string
	args      map[string]map[string]any
	auth      string
	stuck     bool // the task never ends
	emptyCard bool // the caller's card says nothing
}

func (f *fakeAgents) serve(t *testing.T) string {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "agents", Version: "1"}, nil)
	t0 := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	task := func(status string, at time.Time, msgs ...string) map[string]any {
		out := map[string]any{"task_id": "t1", "context_id": "c1", "from": "me", "to": "rev", "status": status,
			"external_ref": "PROJ-1", "created_at": t0, "updated_at": at}
		var ms []map[string]any
		for i, m := range msgs {
			role := "user"
			if i > 0 {
				role = "agent"
			}
			ms = append(ms, map[string]any{"from": "me", "role": role, "author": "agent", "text": m, "at": at})
		}
		if ms != nil {
			out["messages"] = ms
		}
		return out
	}
	tool := func(name string, result func(n int) any) {
		s.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var in map[string]any
			_ = json.Unmarshal(r.Params.Arguments, &in)
			f.mu.Lock()
			f.calls = append(f.calls, name)
			f.args[name] = in
			n := 0
			for _, c := range f.calls {
				if c == name {
					n++
				}
			}
			f.mu.Unlock()
			b, _ := json.Marshal(result(n))
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
		})
	}
	tool("send_message", func(int) any { return task("submitted", t0) })
	tool("outbox", func(n int) any {
		if f.stuck {
			return map[string]any{"tasks": []any{}}
		}
		if n == 1 {
			return map[string]any{"tasks": []any{task("working", t0.Add(time.Second))}}
		}
		return map[string]any{"tasks": []any{task("completed", t0.Add(2*time.Second))}}
	})
	tool("get_task", func(int) any {
		if f.stuck {
			return task("working", t0, "review it")
		}
		return task("completed", t0.Add(2*time.Second), "review it", "LGTM")
	})
	tool("list_agents", func(int) any {
		out := map[string]any{"agents": []any{
			map[string]any{"name": "rev", "skills": []string{"code-review"}, "accepts_you": false},
			map[string]any{"name": "me", "description": "Reviews Go", "skills": []string{"go"}, "accept_from": []string{"team:t1"}, "you": true},
		}}
		if f.emptyCard {
			out["agents"] = []any{map[string]any{"name": "me", "you": true}}
		}
		return out
	})
	tool("inbox", func(int) any { return map[string]any{"tasks": []any{}} })
	tool("register_agent", func(int) any {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := map[string]any{"name": "me"}
		for k, v := range f.args["register_agent"] {
			out[k] = v
		}
		return out
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth = r.Header.Get("Authorization")
		f.mu.Unlock()
		if r.URL.Path != "/mcp/agents" {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestInboxSendWaits(t *testing.T) {
	f := &fakeAgents{args: map[string]map[string]any{}}
	url := f.serve(t)
	t.Setenv("KTMS_AGENT_KEY", "agent-key")

	code, out, errOut := callIn("review it", "--http", url, "inbox", "send", "rev", "-", "--ref", "PROJ-1", "--wait", "1m")
	if code != 0 || !strings.Contains(out, "me → rev  completed") || !strings.Contains(out, "ref PROJ-1") || !strings.Contains(out, "LGTM") {
		t.Fatalf("send --wait = %d\n%s\n%s", code, out, errOut)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := strings.Join(f.calls, ","); got != "send_message,outbox,outbox,get_task" {
		t.Fatalf("calls = %s", got)
	}
	if a := f.args["send_message"]; a["text"] != "review it" || a["external_ref"] != "PROJ-1" {
		t.Fatalf("send_message args = %v", a)
	}
	// The second wait starts after the change the first one saw.
	if a := f.args["outbox"]; a["since"] != "2026-11-08T12:00:01Z" {
		t.Fatalf("outbox since = %v", a["since"])
	}
	if f.auth != "Bearer agent-key" {
		t.Fatalf("auth = %q", f.auth)
	}
}

func TestInboxFind(t *testing.T) {
	f := &fakeAgents{args: map[string]map[string]any{}}
	url := f.serve(t)
	t.Setenv("KTMS_AGENT_KEY", "agent-key")
	code, out, errOut := callIn("", "--http", url, "inbox", "find", "code", "review")
	if code != 0 || !strings.Contains(out, "code-review") || !strings.Contains(out, "no") {
		t.Fatalf("find = %d\n%s\n%s", code, out, errOut)
	}
	if q := f.args["list_agents"]["query"]; q != "code review" {
		t.Fatalf("query = %v", q)
	}
	t.Setenv("KTMS_AGENT_KEY", "")
	if code, _, _ := callIn("", "--http", url, "inbox", "find"); code == 0 {
		t.Fatal("find without an agent key succeeded")
	}
}

func TestInboxSendWaitEnds(t *testing.T) {
	f := &fakeAgents{args: map[string]map[string]any{}, stuck: true}
	url := f.serve(t)
	t.Setenv("KTMS_AGENT_KEY", "agent-key")
	start := time.Now()
	code, out, errOut := callIn("", "--http", url, "inbox", "send", "rev", "x", "--wait", "2s")
	if code == 0 || !strings.Contains(out, "me → rev  working") || strings.Contains(out, "review it") || !strings.Contains(errOut, "Still working after 2s") {
		t.Fatalf("send --wait on a stuck task = %d\n%s\n%s", code, out, errOut)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("waited %s for --wait 2s", d)
	}
}

func TestInboxRegisterKeepsOtherFields(t *testing.T) {
	f := &fakeAgents{args: map[string]map[string]any{}}
	url := f.serve(t)
	t.Setenv("KTMS_AGENT_KEY", "agent-key")
	code, out, errOut := callIn("", "--http", url, "inbox", "register", "--skill", "code-review", "--skill", "go")
	if code != 0 || !strings.Contains(out, "Card of me updated") {
		t.Fatalf("register = %d\n%s\n%s", code, out, errOut)
	}
	a := f.args["register_agent"]
	if a["description"] != "Reviews Go" || fmt.Sprint(a["skills"]) != "[code-review go]" || fmt.Sprint(a["accept_from"]) != "[team:t1]" || fmt.Sprint(a["repos"]) != "[]" {
		t.Fatalf("register_agent args = %v", a)
	}
	if code, _, errOut := callIn("", "--http", url, "inbox", "register", "--accept-from", ""); code != 0 || fmt.Sprint(f.args["register_agent"]["accept_from"]) != "[]" {
		t.Fatalf("clearing accept_from: %d %v %s", code, f.args["register_agent"], errOut)
	}
}

func TestAgentsRunSetsTheCard(t *testing.T) {
	f := &fakeAgents{args: map[string]map[string]any{}}
	url := f.serve(t)
	t.Setenv("KTMS_AGENT_KEY", "agent-key")
	code, _, errOut := callIn("", "--http", url, "agents", "run", "--once", "--cmd", "true", "--description", "Reviews KTMS", "--skill", "review", "--repo", "ai-process/ktms")
	if code != 0 || !strings.Contains(errOut, "card of me updated") {
		t.Fatalf("agents run = %d\n%s", code, errOut)
	}
	if a := f.args["register_agent"]; a["description"] != "Reviews KTMS" || fmt.Sprint(a["skills"]) != "[review]" || fmt.Sprint(a["repos"]) != "[ai-process/ktms]" || fmt.Sprint(a["accept_from"]) != "[team:t1]" {
		t.Fatalf("register_agent args = %v", a)
	}
}

func TestAgentsRunWarnsOnAnEmptyCard(t *testing.T) {
	f := &fakeAgents{args: map[string]map[string]any{}, emptyCard: true}
	url := f.serve(t)
	t.Setenv("KTMS_AGENT_KEY", "agent-key")
	code, _, errOut := callIn("", "--http", url, "agents", "run", "--once", "--cmd", "true")
	if code != 0 || !strings.Contains(errOut, "card of me is empty") || f.args["register_agent"] != nil {
		t.Fatalf("agents run = %d\n%s", code, errOut)
	}
}
