package grpcapi

import (
	"context"
	"errors"
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
	list, err := s.db.ListAgents(ctx)
	if err != nil {
		return nil, dbStatus(err)
	}
	owner := strings.ToLower(strings.TrimSpace(req.GetOwner()))
	out := &pb.ListAgentsResponse{}
	for _, a := range list {
		if owner != "" && a.Owner != owner {
			continue
		}
		out.Agents = append(out.Agents, agentProto(a))
	}
	return out, nil
}

func agentProto(a *proxydb.Agent) *pb.AgentInfo {
	return &pb.AgentInfo{
		Name: a.Name, Owner: a.Owner, OrgId: a.OrgID, TeamId: a.TeamID, Description: a.Description,
		Skills: a.Skills, Repos: a.Repos, AcceptFrom: a.AcceptFrom, Recording: a.Recording, ApiKeyName: a.KeyName,
		UpdatedAt: timestamppb.New(a.UpdatedAt),
	}
}

func (s *AdminServer) SetAgentRecording(ctx context.Context, req *pb.SetAgentRecordingRequest) (*pb.SetAgentRecordingResponse, error) {
	rec := strings.ToLower(strings.TrimSpace(req.GetRecording()))
	if !agents.ValidRecording(rec) {
		return nil, status.Errorf(codes.InvalidArgument, "recording %q: want off, metadata, full or empty", req.GetRecording())
	}
	err := s.db.SetAgentRecording(ctx, req.GetName(), rec)
	if errors.Is(err, proxydb.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "no agent %q", req.GetName())
	}
	if err != nil {
		return nil, dbStatus(err)
	}
	return &pb.SetAgentRecordingResponse{}, nil
}

func (s *AdminServer) ListAgentTasks(ctx context.Context, req *pb.ListAgentTasksRequest) (*pb.ListAgentTasksResponse, error) {
	a, err := s.db.GetAgentByName(ctx, req.GetAgent())
	if errors.Is(err, proxydb.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "no agent %q", req.GetAgent())
	}
	if err != nil {
		return nil, dbStatus(err)
	}
	f := proxydb.TaskFilter{Agent: a.KeyID, Limit: int(req.GetLimit())}
	if req.GetOpenOnly() {
		f.Statuses = []string{proxydb.TaskSubmitted, proxydb.TaskWorking, proxydb.TaskInputRequired}
	}
	tasks, err := s.db.ListAgentTasks(ctx, f)
	if err != nil {
		return nil, dbStatus(err)
	}
	out := &pb.ListAgentTasksResponse{}
	for _, t := range tasks {
		out.Tasks = append(out.Tasks, taskProto(t))
	}
	return out, nil
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
		StatusReason: t.StatusReason, Depth: t.Depth,
		CreatedAt: timestamppb.New(t.CreatedAt), UpdatedAt: timestamppb.New(t.UpdatedAt),
	}
	for _, m := range t.Messages {
		out.Messages = append(out.Messages, &pb.AgentMessage{From: m.FromName, Role: m.Role, Text: m.Text, CreatedAt: timestamppb.New(m.CreatedAt)})
	}
	return out
}
