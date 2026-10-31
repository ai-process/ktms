package grpcapi

import (
	"context"
	"errors"
	"slices"
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

const (
	scheduleReadTimeout = time.Second      // a slow database must not hold a heartbeat back
	scheduleMemoTTL     = 10 * time.Second // schedules change rarely; heartbeats come every few seconds
)

var (
	scheduleSources = []string{"local", ScheduleSourceKTMS}
	capacitySources = []string{"schedule", "override", "ktms_schedule", "ktms_override", "default"}
)

// knownOr keeps a value the agent reports only if it is one of known, so
// a node cannot store arbitrary text in the pool.
func knownOr(v string, known []string) string {
	if slices.Contains(known, v) {
		return v
	}
	return ""
}

type memoSchedule struct {
	at time.Time
	s  *pb.NodeSchedule
}

// SetSchedules lets heartbeats carry the schedules KTMS keeps for nodes.
func (s *NodeRegistryServer) SetSchedules(r NodeScheduleReader) { s.schedules = r }

// scheduleFor is the schedule to send an agent that takes its levels from
// KTMS; nil when it could not be read, so the agent keeps the last one.
func (s *NodeRegistryServer) scheduleFor(ctx context.Context, nodeID string) *pb.NodeSchedule {
	if s.schedules == nil {
		return nil
	}
	if m, ok := s.schedMemo.Load(nodeID); ok && time.Since(m.(memoSchedule).at) < s.schedTTL {
		return m.(memoSchedule).s
	}
	out := s.readSchedule(ctx, nodeID)
	if out != nil {
		s.schedMemo.Store(nodeID, memoSchedule{at: time.Now(), s: out})
	}
	return out
}

func (s *NodeRegistryServer) readSchedule(ctx context.Context, nodeID string) *pb.NodeSchedule {
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
	if req.GetClear() {
		row, err := s.db.ClearNodeOverride(ctx, req.GetNodeId())
		if errors.Is(err, proxydb.ErrNotFound) {
			return &pb.NodeScheduleInfo{NodeId: req.GetNodeId()}, nil // nothing left: the node runs at 100%
		}
		if err != nil {
			return nil, dbStatus(err)
		}
		log.Info().Str("node_id", req.GetNodeId()).Msg("node override cleared")
		return nodeScheduleInfo(row, time.Now()), nil
	}
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
	row, err := s.db.SetNodeOverride(ctx, req.GetNodeId(), int32(req.GetLevel()), u)
	if err != nil {
		return nil, dbStatus(err)
	}
	log.Info().Str("node_id", req.GetNodeId()).Uint32("level", req.GetLevel()).Time("until", u).Msg("node override set")
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
