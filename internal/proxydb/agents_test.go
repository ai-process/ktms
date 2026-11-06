package proxydb

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"
)

func TestListAgentTasksByStatus(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	var keys []string
	for range 2 {
		id := uuid.NewString()
		if _, err := db.pool.Exec(ctx,
			`INSERT INTO api_key (id, name, key_lookup, key_hash, scopes) VALUES ($1, $2, $3, $3, '{mcp}')`,
			id, "agent-"+id[:8], "lookup-"+id); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := db.pool.Exec(context.Background(), `DELETE FROM api_key WHERE id = $1`, id); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		})
		if err := db.UpsertAgent(ctx, &Agent{KeyID: id, Name: "agent-" + id[:8]}); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, id)
	}
	byStatus := map[string]string{}
	for _, st := range []string{TaskSubmitted, TaskInputRequired, TaskCompleted, TaskCanceled} {
		task := &AgentTask{ID: uuid.NewString(), ContextID: uuid.NewString(), FromAgent: keys[0], ToAgent: keys[1], Status: st}
		if err := db.CreateAgentTask(ctx, task, "hi", 0); err != nil {
			t.Fatal(err)
		}
		byStatus[st] = task.ID
	}

	for _, tc := range []struct {
		statuses []string
		want     []string
	}{
		{nil, []string{TaskSubmitted, TaskInputRequired, TaskCompleted, TaskCanceled}},
		{[]string{TaskCompleted}, []string{TaskCompleted}},
		{[]string{TaskInputRequired, TaskCanceled}, []string{TaskInputRequired, TaskCanceled}},
		{[]string{TaskWorking, TaskFailed}, nil},
	} {
		tasks, err := db.ListAgentTasks(ctx, TaskFilter{Agent: keys[1], Statuses: tc.statuses})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, task := range tasks {
			if byStatus[task.Status] != task.ID {
				t.Fatalf("statuses %v: task %s in %s is not ours", tc.statuses, task.ID, task.Status)
			}
			got = append(got, task.Status)
		}
		slices.Sort(got)
		want := slices.Sorted(slices.Values(tc.want))
		if !slices.Equal(got, want) {
			t.Errorf("statuses %v = %v, want %v", tc.statuses, got, want)
		}
	}
}
