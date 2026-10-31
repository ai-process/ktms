package grpcapi

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/nodeagent"
	"github.com/ai-process/ktms/internal/proxydb"
)

// ScheduleSourceKTMS marks an agent that takes its levels from KTMS.
const ScheduleSourceKTMS = "ktms"

// NodeScheduleReader is what heartbeats need of the schedule store.
type NodeScheduleReader interface {
	GetNodeSchedule(ctx context.Context, nodeID string) (*proxydb.NodeSchedule, error)
}

const scheduleReadTimeout = time.Second // a slow database must not hold a heartbeat back

// SetSchedules lets heartbeats carry the schedules KTMS keeps for nodes.
func (s *NodeRegistryServer) SetSchedules(r NodeScheduleReader) { s.schedules = r }

// scheduleFor is the schedule to send an agent that takes its levels from
// KTMS; nil when it could not be read, so the agent keeps the last one.
func (s *NodeRegistryServer) scheduleFor(ctx context.Context, nodeID string) *pb.NodeSchedule {
	if s.schedules == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, scheduleReadTimeout)
	defer cancel()
	row, err := s.schedules.GetNodeSchedule(ctx, nodeID)
	if errors.Is(err, proxydb.ErrNotFound) {
		return &pb.NodeSchedule{} // version 0: none set, the node runs at 100%
	}
	if err != nil {
		log.Warn().Err(err).Str("node_id", nodeID).Msg("node schedule unreadable; the node keeps its last one")
		return nil
	}
	return nodeSchedulePB(row, time.Now())
}

func nodeSchedulePB(row *proxydb.NodeSchedule, now time.Time) *pb.NodeSchedule {
	return &pb.NodeSchedule{Text: row.Text, Version: row.Version, Override: overridePB(row, now)}
}

// overridePB is the override in force; an expired one is not sent.
func overridePB(row *proxydb.NodeSchedule, now time.Time) *pb.NodeCapacityOverride {
	if row.OverrideLevel == nil || row.OverrideUntil == nil || !row.OverrideUntil.After(now) {
		return nil
	}
	return &pb.NodeCapacityOverride{Level: uint32(*row.OverrideLevel), Until: timestamppb.New(*row.OverrideUntil)}
}

func nodeScheduleInfo(row *proxydb.NodeSchedule, now time.Time) *pb.NodeScheduleInfo {
	return &pb.NodeScheduleInfo{
		NodeId:    row.NodeID,
		Text:      row.Text,
		Version:   row.Version,
		Override:  overridePB(row, now),
		UpdatedAt: timestamppb.New(row.UpdatedAt),
	}
}

func requireNodeID(id string) error {
	if strings.TrimSpace(id) == "" {
		return status.Error(codes.InvalidArgument, "node_id is required")
	}
	return nil
}

func (s *AdminServer) GetNodeSchedule(ctx context.Context, req *pb.GetNodeScheduleRequest) (*pb.NodeScheduleInfo, error) {
	if err := requireNodeID(req.GetNodeId()); err != nil {
		return nil, err
	}
	row, err := s.db.GetNodeSchedule(ctx, req.GetNodeId())
	if errors.Is(err, proxydb.ErrNotFound) {
		return &pb.NodeScheduleInfo{NodeId: req.GetNodeId()}, nil
	}
	if err != nil {
		return nil, dbStatus(err)
	}
	return nodeScheduleInfo(row, time.Now()), nil
}

func (s *AdminServer) ListNodeSchedules(ctx context.Context, _ *pb.ListNodeSchedulesRequest) (*pb.ListNodeSchedulesResponse, error) {
	rows, err := s.db.ListNodeSchedules(ctx)
	if err != nil {
		return nil, dbStatus(err)
	}
	now := time.Now()
	out := &pb.ListNodeSchedulesResponse{}
	for _, r := range rows {
		out.Schedules = append(out.Schedules, nodeScheduleInfo(r, now))
	}
	return out, nil
}

// SetNodeSchedule checks the rules with the agent's own parser, so a node
// never receives a schedule it would refuse.
func (s *AdminServer) SetNodeSchedule(ctx context.Context, req *pb.SetNodeScheduleRequest) (*pb.NodeScheduleInfo, error) {
	if err := requireNodeID(req.GetNodeId()); err != nil {
		return nil, err
	}
	if _, err := nodeagent.ParseSchedule(strings.NewReader(req.GetText())); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "schedule: %v", err)
	}
	row, err := s.db.SetNodeScheduleText(ctx, req.GetNodeId(), req.GetText())
	if err != nil {
		return nil, dbStatus(err)
	}
	log.Info().Str("node_id", req.GetNodeId()).Int64("version", row.Version).Msg("node schedule set")
	return nodeScheduleInfo(row, time.Now()), nil
}

func (s *AdminServer) SetNodeCapacityOverride(ctx context.Context, req *pb.SetNodeCapacityOverrideRequest) (*pb.NodeScheduleInfo, error) {
	if err := requireNodeID(req.GetNodeId()); err != nil {
		return nil, err
	}
	var level *int32
	var until *time.Time
	if !req.GetClear() {
		if req.GetLevel() > 100 {
			return nil, status.Errorf(codes.InvalidArgument, "level %d is above 100", req.GetLevel())
		}
		if req.GetUntil() == nil {
			return nil, status.Error(codes.InvalidArgument, "until is required (or clear)")
		}
		u := req.GetUntil().AsTime()
		switch d := time.Until(u); {
		case d < nodeagent.MinOverride:
			return nil, status.Errorf(codes.InvalidArgument, "until must be at least %v ahead", nodeagent.MinOverride)
		case d > nodeagent.MaxOverride:
			return nil, status.Errorf(codes.InvalidArgument, "until may be at most %v ahead", nodeagent.MaxOverride)
		}
		l := int32(req.GetLevel())
		level, until = &l, &u
	}
	row, err := s.db.SetNodeOverride(ctx, req.GetNodeId(), level, until)
	if err != nil {
		return nil, dbStatus(err)
	}
	log.Info().Str("node_id", req.GetNodeId()).Bool("clear", req.GetClear()).Uint32("level", req.GetLevel()).Msg("node override set")
	return nodeScheduleInfo(row, time.Now()), nil
}

func (s *AdminServer) DeleteNodeSchedule(ctx context.Context, req *pb.DeleteNodeScheduleRequest) (*pb.DeleteNodeScheduleResponse, error) {
	if err := requireNodeID(req.GetNodeId()); err != nil {
		return nil, err
	}
	if err := s.db.DeleteNodeSchedule(ctx, req.GetNodeId()); err != nil {
		return nil, dbStatus(err)
	}
	return &pb.DeleteNodeScheduleResponse{}, nil
}
