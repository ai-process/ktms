package grpcapi

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/agents"
	"github.com/ai-process/ktms/internal/apikeys"
)

// ktms-admin mints a person's agent key with owner and lists it back by owner;
// the operator then sees the agent and its conversation.
func TestAgentKeysAndConversations(t *testing.T) {
	db := rotateTestDB(t)
	ctx := context.Background()
	s := NewAdminServer(db, keyring(t, 1), nil)
	owner := "Dev-" + uuid.NewString()[:8] + "@X.com"
	names := []string{"agent-" + uuid.NewString()[:8], "agent-" + uuid.NewString()[:8]}
	for _, n := range names {
		if _, err := s.MintAPIKey(ctx, &pb.MintAPIKeyRequest{Name: n, Scopes: []string{"mcp"}, Owner: owner,
			Policy: &pb.APIKeyPolicy{AllowedMcp: []string{agents.Server}}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.MintAPIKey(ctx, &pb.MintAPIKeyRequest{Name: names[0], Scopes: []string{"mcp"}}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("a second key of the same name: %v", err)
	}
	keys, err := s.ListAPIKeys(ctx, &pb.ListAPIKeysRequest{Owner: owner})
	if err != nil || len(keys.GetKeys()) != 2 || keys.GetKeys()[0].GetOwner() != "dev-"+owner[4:12]+"@x.com" {
		t.Fatalf("keys by owner = %v, %v", keys, err)
	}

	svc := agents.New(db, agents.Config{}, nil, nil)
	var ids []*apikeys.Identity
	for _, n := range names {
		row, err := db.GetAPIKeyByName(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		id := apikeys.IdentityFor(row)
		if _, err := svc.Me(ctx, id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	task, err := svc.Send(ctx, ids[0], names[1], "hello", "")
	if err != nil {
		t.Fatal(err)
	}

	list, err := s.ListAgents(ctx, &pb.ListAgentsRequest{Owner: keys.GetKeys()[0].GetOwner()})
	if err != nil || len(list.GetAgents()) != 2 || list.GetAgents()[0].GetApiKeyName() == "" {
		t.Fatalf("agents = %v, %v", list, err)
	}
	if _, err := s.RevokeAPIKey(ctx, &pb.RevokeAPIKeyRequest{Name: names[0]}); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.ListAgents(ctx, &pb.ListAgentsRequest{Owner: keys.GetKeys()[0].GetOwner()}); len(l.GetAgents()) != 1 {
		t.Fatalf("a revoked key's agent is listed: %v", l)
	}
	if l, _ := s.ListAgents(ctx, &pb.ListAgentsRequest{Owner: keys.GetKeys()[0].GetOwner(), IncludeRevoked: true}); len(l.GetAgents()) != 2 || l.GetAgents()[0].GetRevoked() != (l.GetAgents()[0].GetName() == names[0]) {
		t.Fatalf("include_revoked = %v", l)
	}
	if _, err := s.SetAgentRecording(ctx, &pb.SetAgentRecordingRequest{Name: names[1], Recording: "loud"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad recording: %v", err)
	}
	if _, err := s.SetAgentRecording(ctx, &pb.SetAgentRecordingRequest{Name: names[1], Recording: "full"}); err != nil {
		t.Fatal(err)
	}
	if a, _ := db.GetAgentByName(ctx, names[1]); a.Recording != "full" {
		t.Fatalf("recording = %q", a.Recording)
	}
	tasks, err := s.ListAgentTasks(ctx, &pb.ListAgentTasksRequest{Agent: names[1], OpenOnly: true})
	if err != nil || len(tasks.GetTasks()) != 1 || tasks.GetTasks()[0].GetFrom() != names[0] || len(tasks.GetTasks()[0].GetMessages()) != 0 {
		t.Fatalf("tasks = %v, %v", tasks, err)
	}
	got, err := s.GetAgentTask(ctx, &pb.GetAgentTaskRequest{TaskId: task.ID})
	if err != nil || len(got.GetMessages()) != 1 || got.GetMessages()[0].GetText() != "hello" {
		t.Fatalf("task = %v, %v", got, err)
	}
	if _, err := s.GetAgentTask(ctx, &pb.GetAgentTaskRequest{TaskId: "x"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := s.ListAgentTasks(ctx, &pb.ListAgentTasksRequest{Agent: "nobody-here"}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown agent: %v", err)
	}
	for _, tc := range []struct {
		statuses []string
		open     bool
		want     int
	}{
		{[]string{" Submitted "}, false, 1},
		{[]string{"completed", "canceled"}, false, 0},
		{[]string{"submitted", "completed"}, true, 1},
		{[]string{"completed"}, true, 0},
	} {
		tasks, err := s.ListAgentTasks(ctx, &pb.ListAgentTasksRequest{Agent: names[1], Statuses: tc.statuses, OpenOnly: tc.open})
		if err != nil || len(tasks.GetTasks()) != tc.want {
			t.Fatalf("statuses %v, open %v = %v, %v; want %d tasks", tc.statuses, tc.open, tasks, err, tc.want)
		}
	}
	if _, err := s.ListAgentTasks(ctx, &pb.ListAgentTasksRequest{Agent: names[1], Statuses: []string{"working", "paused"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown status: %v", err)
	}
	if tasks, err := s.ListAgentTasks(ctx, &pb.ListAgentTasksRequest{Agent: names[1], ReasonCodes: []string{"needs_owner"}}); err != nil || len(tasks.GetTasks()) != 0 {
		t.Fatalf("reason needs_owner = %v, %v", tasks, err)
	}
	if _, err := s.ListAgentTasks(ctx, &pb.ListAgentTasksRequest{Agent: names[1], ReasonCodes: []string{"bored"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown reason: %v", err)
	}

	// The owner of the recipient answers for it, then corrects the answer.
	ownerOf := keys.GetKeys()[0].GetOwner()
	if _, err := s.ReplyAgentTask(ctx, &pb.ReplyAgentTaskRequest{Agent: names[1], Owner: ownerOf, TaskId: task.ID, Text: "x"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("without the inbox: %v", err)
	}
	s.SetAgents(svc)
	if _, err := s.ReplyAgentTask(ctx, &pb.ReplyAgentTaskRequest{Agent: names[1], Owner: "eve@x.com", TaskId: task.ID, Text: "x"}); status.Code(err) != codes.NotFound {
		t.Fatalf("another person's agent: %v", err)
	}
	done, err := s.ReplyAgentTask(ctx, &pb.ReplyAgentTaskRequest{Agent: names[1], Owner: strings.ToUpper(ownerOf), TaskId: task.ID, Text: "hi from the person"})
	if err != nil || done.GetStatus() != "completed" || done.GetAnsweredBy() != "owner" || done.GetMessages()[1].GetAuthor() != "owner" {
		t.Fatalf("owner's answer = %v, %v", done, err)
	}
	fixed, err := s.ReplyAgentTask(ctx, &pb.ReplyAgentTaskRequest{Agent: names[1], Owner: ownerOf, TaskId: task.ID, Text: "better answer", Correction: true})
	if err != nil || fixed.GetStatus() != "completed" || !fixed.GetMessages()[2].GetCorrection() {
		t.Fatalf("owner's correction = %v, %v", fixed, err)
	}
	if _, err := s.ReplyAgentTask(ctx, &pb.ReplyAgentTaskRequest{Agent: names[1], Owner: ownerOf, TaskId: task.ID, Text: "x", Status: "failed", Correction: true}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a correction with a status: %v", err)
	}
}

func TestTaskStatuses(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		open bool
		want []string
	}{
		{nil, false, nil},
		{nil, true, []string{"submitted", "working", "input-required"}},
		{[]string{"Working", "working ", "failed"}, false, []string{"working", "failed"}},
		{[]string{"failed", "input-required", "completed"}, true, []string{"input-required"}},
		{[]string{"failed", "canceled"}, true, nil},
	} {
		got, err := taskStatuses(tc.in, tc.open)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("taskStatuses(%q, %v) = %q, %v; want %q", tc.in, tc.open, got, err, tc.want)
		}
	}
	for _, bad := range [][]string{{"paused"}, {"working", ""}, {"in-progress"}} {
		if _, err := taskStatuses(bad, false); status.Code(err) != codes.InvalidArgument {
			t.Errorf("taskStatuses(%q) = %v, want INVALID_ARGUMENT", bad, err)
		}
	}
}
