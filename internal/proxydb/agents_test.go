package proxydb

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func agentKey(t *testing.T, d *DB, name string) string {
	t.Helper()
	id := uuid.NewString()
	if err := d.InsertAPIKey(context.Background(), &APIKey{ID: id, Name: name, KeyLookup: uuid.NewString(), KeyHash: "h", Scopes: []string{"mcp"}}); err != nil {
		t.Fatalf("InsertAPIKey: %v", err)
	}
	t.Cleanup(func() {
		if _, err := d.pool.Exec(context.Background(), `DELETE FROM api_key WHERE id = $1`, id); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	return id
}

// A revoked key frees its agent's name; a live key's agent keeps it, and the
// first-call insert never overwrites a card already written.
func TestAgentNames(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()
	name := "dup-" + uuid.NewString()[:8]
	old := agentKey(t, d, strings.ToUpper(name))
	if err := d.InsertAgent(ctx, old, name); err != nil {
		t.Fatal(err)
	}
	desc := "kept"
	if err := d.UpdateAgentCard(ctx, old, AgentCard{Description: &desc, AcceptFrom: []string{"bob"}}); err != nil {
		t.Fatal(err)
	}
	if err := d.InsertAgent(ctx, old, name); err != nil {
		t.Fatal(err)
	}
	if a, _ := d.GetAgentByKey(ctx, old); a.Description != "kept" || len(a.AcceptFrom) != 1 {
		t.Fatalf("a repeated insert rewrote the card: %+v", a)
	}

	other := agentKey(t, d, "x"+name)
	if err := d.InsertAgent(ctx, other, name); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("took a live agent's name: %v", err)
	}
	if err := d.DisableAPIKey(ctx, strings.ToUpper(name)); err != nil {
		t.Fatal(err)
	}
	if a, _ := d.GetAgentByKey(ctx, old); !a.Released || !a.Disabled {
		t.Fatalf("revoked key's agent = %+v", a)
	}
	fresh := agentKey(t, d, name)
	if err := d.InsertAgent(ctx, fresh, name); err != nil {
		t.Fatalf("a new key of a revoked name: %v", err)
	}
	if a, err := d.GetAgentByName(ctx, name); err != nil || a.KeyID != fresh {
		t.Fatalf("by name = %+v, %v", a, err)
	}
	if err := d.SetAgentRecording(ctx, name, "full"); err != nil {
		t.Fatal(err)
	}
	if a, _ := d.GetAgentByKey(ctx, fresh); a.Recording != "full" {
		t.Fatalf("recording went to the old card: %+v", a)
	}
	if keys, err := d.AgentKeysByName(ctx, name); err != nil || len(keys) != 2 {
		t.Fatalf("keys of the name = %v, %v", keys, err)
	}
}

// Trust given to a revoked key's agent by name does not pass to a newer key of
// that name, and the list never empties into "anyone".
func TestRevokedNameLosesTrust(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()
	name := "trusted-" + uuid.NewString()[:8]
	old := agentKey(t, d, name)
	bob := agentKey(t, d, "bob-"+uuid.NewString()[:8])
	for k, n := range map[string]string{old: name, bob: "bob-" + bob[:8]} {
		if err := d.InsertAgent(ctx, k, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.UpdateAgentCard(ctx, bob, AgentCard{AcceptFrom: []string{name, "owner:x@y.z"}}); err != nil {
		t.Fatal(err)
	}
	if err := d.DisableAPIKey(ctx, name); err != nil {
		t.Fatal(err)
	}
	if a, _ := d.GetAgentByKey(ctx, bob); len(a.AcceptFrom) != 2 || a.AcceptFrom[0] != AcceptRevoked+name || a.AcceptFrom[1] != "owner:x@y.z" {
		t.Fatalf("accept_from after the revoke = %v", a.AcceptFrom)
	}
}

func TestAgentMessagesWindow(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()
	a, b := agentKey(t, d, "wa-"+uuid.NewString()[:8]), agentKey(t, d, "wb-"+uuid.NewString()[:8])
	for _, k := range []string{a, b} {
		if err := d.InsertAgent(ctx, k, "w-"+k[:13]); err != nil {
			t.Fatal(err)
		}
	}
	task := &AgentTask{ID: uuid.NewString(), ContextID: uuid.NewString(), FromAgent: a, ToAgent: b, Status: TaskSubmitted}
	if err := d.CreateAgentTask(ctx, task, strings.Repeat("q", 100), TaskLimits{}); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		if _, err := d.AddAgentMessage(ctx, task.ID, b, "agent", strings.Repeat(string(rune('a'+i)), 100), TaskWorking, "", 0); err != nil {
			t.Fatal(err)
		}
	}
	got, err := d.GetAgentTask(ctx, task.ID)
	if err != nil || len(got.Messages) != 0 {
		t.Fatalf("task = %+v, %v", got, err)
	}
	// The first message and the two newest fit 200 bytes each.
	if err := d.WithAgentMessages(ctx, []*AgentTask{got}, 200); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 3 || got.Omitted != 2 || got.Messages[0].Text[0] != 'q' || got.Messages[2].Text[0] != 'd' {
		t.Fatalf("windowed = %d messages, %d omitted", len(got.Messages), got.Omitted)
	}
	// Nothing else fits: the first message stays, cut to the window.
	if err := d.WithAgentMessages(ctx, []*AgentTask{got}, 50); err != nil || len(got.Messages) != 1 || got.Omitted != 4 ||
		!got.Messages[0].Truncated || len(got.Messages[0].Text) > 50 {
		t.Fatalf("nothing fits = %d, %d, %v", len(got.Messages), got.Omitted, err)
	}
	if err := d.WithAgentMessages(ctx, []*AgentTask{got}, 0); err != nil || len(got.Messages) != 5 || got.Omitted != 0 {
		t.Fatalf("all = %d, %d, %v", len(got.Messages), got.Omitted, err)
	}

	// The context lock and count hold for any spelling of the uuid.
	if err := d.CreateAgentTask(ctx, &AgentTask{ID: uuid.NewString(), ContextID: "{" + strings.ToUpper(task.ContextID) + "}",
		FromAgent: a, ToAgent: b, Status: TaskSubmitted}, "x", TaskLimits{Messages: 5}); !errors.Is(err, ErrContextFull) {
		t.Fatalf("another spelling of a full context: %v", err)
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockContext(ctx, tx, "{"+strings.ToUpper(task.ContextID)+"}", 0); err != nil {
		t.Fatal(err)
	}
	var free bool
	if err := d.pool.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtext('agent_context:' || $1))`, task.ContextID).Scan(&free); err != nil || free {
		t.Fatalf("the canonical spelling took the lock held by another: %v, %v", free, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// Two tasks an hour from a to b.
	if err := d.CreateAgentTask(ctx, &AgentTask{ID: uuid.NewString(), ContextID: uuid.NewString(), FromAgent: a, ToAgent: b, Status: TaskSubmitted},
		"x", TaskLimits{PairPerHour: 2}); err != nil {
		t.Fatal(err)
	}
	if err := d.CreateAgentTask(ctx, &AgentTask{ID: uuid.NewString(), ContextID: uuid.NewString(), FromAgent: a, ToAgent: b, Status: TaskSubmitted},
		"x", TaskLimits{PairPerHour: 2}); !errors.Is(err, ErrPairBusy) {
		t.Fatalf("third task of the hour: %v", err)
	}
	if err := d.CreateAgentTask(ctx, &AgentTask{ID: uuid.NewString(), ContextID: uuid.NewString(), FromAgent: b, ToAgent: a, Status: TaskSubmitted},
		"x", TaskLimits{PairPerHour: 2}); err != nil {
		t.Fatalf("the other direction: %v", err)
	}
}

func TestTaskStatuses(t *testing.T) {
	for _, s := range TaskOpenStatuses {
		if TaskTerminal(s) {
			t.Fatalf("%s is terminal", s)
		}
	}
	for _, s := range []string{TaskCompleted, TaskFailed, TaskCanceled} {
		if !TaskTerminal(s) {
			t.Fatalf("%s is not terminal", s)
		}
	}
}

// A first message larger than the window is cut, not dropped; the count of the
// others comes from the same snapshot.
func TestAgentMessagesFirstCut(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()
	a, b := agentKey(t, d, "ca-"+uuid.NewString()[:8]), agentKey(t, d, "cb-"+uuid.NewString()[:8])
	for _, k := range []string{a, b} {
		if err := d.InsertAgent(ctx, k, "c-"+k[:13]); err != nil {
			t.Fatal(err)
		}
	}
	task := &AgentTask{ID: uuid.NewString(), ContextID: uuid.NewString(), FromAgent: a, ToAgent: b, Status: TaskSubmitted}
	if err := d.CreateAgentTask(ctx, task, strings.Repeat("я", 300), TaskLimits{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := d.AddAgentMessage(ctx, task.ID, b, "agent", strings.Repeat("z", 80), TaskWorking, "", 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.WithAgentMessages(ctx, []*AgentTask{task}, 101); err != nil {
		t.Fatal(err)
	}
	first := task.Messages[0]
	if !first.Truncated || len(first.Text) > 101 || !strings.HasPrefix(strings.Repeat("я", 300), first.Text) {
		t.Fatalf("first message = %d bytes, truncated %v", len(first.Text), first.Truncated)
	}
	if len(task.Messages) != 2 || task.Omitted != 2 || task.Messages[1].Truncated {
		t.Fatalf("%d messages, %d omitted", len(task.Messages), task.Omitted)
	}
}
