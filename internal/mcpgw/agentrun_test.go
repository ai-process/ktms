package mcpgw

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ai-process/ktms/internal/agentrun"
	"github.com/ai-process/ktms/internal/proxydb"
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
	f := newAgentsFixture(t)
	ctx := context.Background()
	svc, ts := f.svc, f.ts
	_, alice := f.mint("alice")
	bobKey, bob := f.mint("bob")

	if _, err := svc.Me(ctx, bob); err != nil {
		t.Fatal(err)
	}
	task, err := svc.Send(ctx, alice, strings.ToLower(bob.Name), "review the agents sidecar", "", "")
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
	if _, err := svc.Reply(ctx, alice, task.ID, "main", proxydb.TaskWorking, false); err != nil {
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

// ktms agents run waits on the inbox through the gateway and answers a task
// as soon as it is sent, with no polling interval in between.
func TestAgentRunWaitsThroughGateway(t *testing.T) {
	f := newAgentsFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, alice := f.mint("alice")
	bobKey, bob := f.mint("bob")
	if _, err := f.svc.Me(ctx, bob); err != nil {
		t.Fatal(err)
	}
	c := &agentrun.Client{Endpoint: f.ts.URL + "/mcp/agents", Key: bobKey}
	t.Cleanup(func() { _ = c.Close() })
	done := make(chan error, 1)
	go func() {
		done <- agentrun.Run(ctx, c, &scripted{prompts: []string{"skip the question"}}, agentrun.Config{Interval: time.Hour, Wait: 3 * time.Second})
	}()
	time.Sleep(500 * time.Millisecond) // the sidecar is waiting
	task, err := f.svc.Send(ctx, alice, strings.ToLower(bob.Name), "review PR 12", "", "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var got *proxydb.AgentTask
	for time.Now().Before(deadline) {
		if got, err = f.svc.Get(ctx, alice, task.ID); err == nil && got.Status == proxydb.TaskCompleted {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil || got == nil || got.Status != proxydb.TaskCompleted {
		t.Fatalf("task = %+v, %v", got, err)
	}
}
