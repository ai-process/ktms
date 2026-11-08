package grpcapi

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/agents"
	"github.com/ai-process/ktms/internal/proxydb"
)

func (s *AdminServer) ListAgents(ctx context.Context, req *pb.ListAgentsRequest) (*pb.ListAgentsResponse, error) {
	list, err := s.db.ListAgents(ctx, proxydb.AgentFilter{Owner: strings.ToLower(strings.TrimSpace(req.GetOwner())), IncludeRevoked: req.GetIncludeRevoked()})
	if err != nil {
		return nil, dbStatus(err)
	}
	out := &pb.ListAgentsResponse{}
	for _, a := range list {
		out.Agents = append(out.Agents, agentProto(a))
	}
	return out, nil
}

// agentName is an agent's name as send_message reads it.
func agentName(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func agentProto(a *proxydb.Agent) *pb.AgentInfo {
	return &pb.AgentInfo{
		Name: a.Name, Owner: a.Owner, OrgId: a.OrgID, TeamId: a.TeamID, Description: a.Description,
		Skills: a.Skills, Repos: a.Repos, AcceptFrom: a.AcceptFrom, Recording: a.Recording, ApiKeyName: a.KeyName,
		UpdatedAt: timestamppb.New(a.UpdatedAt), Revoked: a.Disabled,
	}
}

func (s *AdminServer) SetAgentRecording(ctx context.Context, req *pb.SetAgentRecordingRequest) (*pb.SetAgentRecordingResponse, error) {
	rec := strings.ToLower(strings.TrimSpace(req.GetRecording()))
	if !agents.ValidRecording(rec) {
		return nil, status.Errorf(codes.InvalidArgument, "recording %q: want off, metadata, full or empty", req.GetRecording())
	}
	err := s.db.SetAgentRecording(ctx, agentName(req.GetName()), rec)
	if errors.Is(err, proxydb.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "no agent %q", req.GetName())
	}
	if err != nil {
		return nil, dbStatus(err)
	}
	return &pb.SetAgentRecordingResponse{}, nil
}

func (s *AdminServer) ListAgentTasks(ctx context.Context, req *pb.ListAgentTasksRequest) (*pb.ListAgentTasksResponse, error) {
	statuses, err := taskStatuses(req.GetStatuses(), req.GetOpenOnly())
	if err != nil {
		return nil, err
	}
	for _, r := range req.GetReasonCodes() {
		if !slices.Contains(proxydb.ReasonCodes, r) {
			return nil, status.Errorf(codes.InvalidArgument, "reason code %q: want one of %s", r, strings.Join(proxydb.ReasonCodes, ", "))
		}
	}
	a, err := s.db.GetAgentByName(ctx, agentName(req.GetAgent()))
	if errors.Is(err, proxydb.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "no agent %q", req.GetAgent())
	}
	if err != nil {
		return nil, dbStatus(err)
	}
	// An empty filter means every status, so open_only with only ended statuses must match nothing.
	if req.GetOpenOnly() && len(statuses) == 0 {
		return &pb.ListAgentTasksResponse{}, nil
	}
	tasks, err := s.db.ListAgentTasks(ctx, proxydb.TaskFilter{Agent: a.KeyID, Statuses: statuses, ReasonCodes: req.GetReasonCodes(), Limit: int(req.GetLimit())})
	if err != nil {
		return nil, dbStatus(err)
	}
	out := &pb.ListAgentTasksResponse{}
	for _, t := range tasks {
		out.Tasks = append(out.Tasks, taskProto(t))
	}
	return out, nil
}

// taskStatuses is the status filter of a listing, empty for any status; with
// openOnly, the open ones among those requested, which may be none.
func taskStatuses(requested []string, openOnly bool) ([]string, error) {
	var out []string
	for _, st := range requested {
		st = strings.ToLower(strings.TrimSpace(st))
		if !proxydb.ValidTaskStatus(st) {
			return nil, status.Errorf(codes.InvalidArgument, "status %q: want one of %s", st, strings.Join(proxydb.TaskStatuses, ", "))
		}
		if !slices.Contains(out, st) {
			out = append(out, st)
		}
	}
	if !openOnly {
		return out, nil
	}
	if len(out) == 0 {
		out = proxydb.TaskStatuses
	}
	var open []string
	for _, st := range out {
		if !proxydb.TaskTerminal(st) {
			open = append(open, st)
		}
	}
	return open, nil
}

func (s *AdminServer) GetAgentTask(ctx context.Context, req *pb.GetAgentTaskRequest) (*pb.AgentTask, error) {
	if uuid.Validate(req.GetTaskId()) != nil {
		return nil, status.Error(codes.InvalidArgument, "task_id must be a task id")
	}
	t, err := s.db.GetAgentTask(ctx, req.GetTaskId())
	if errors.Is(err, proxydb.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "no task %s", req.GetTaskId())
	}
	if err != nil {
		return nil, dbStatus(err)
	}
	return taskProto(t), nil
}

func taskProto(t *proxydb.AgentTask) *pb.AgentTask {
	out := &pb.AgentTask{
		TaskId: t.ID, ContextId: t.ContextID, From: t.FromName, To: t.ToName, Status: t.Status,
		StatusReason: t.StatusReason, ReasonCode: t.ReasonCode, Depth: t.Depth,
		CreatedAt: timestamppb.New(t.CreatedAt), UpdatedAt: timestamppb.New(t.UpdatedAt),
	}
	for _, m := range t.Messages {
		out.Messages = append(out.Messages, &pb.AgentMessage{From: m.FromName, Role: m.Role, Text: m.Text, CreatedAt: timestamppb.New(m.CreatedAt)})
	}
	return out
}
