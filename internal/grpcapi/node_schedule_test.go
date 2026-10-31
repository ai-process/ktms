package grpcapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/proxydb"
)

type fakeSchedules struct {
	row *proxydb.NodeSchedule
	err error
}

func (f *fakeSchedules) GetNodeSchedule(context.Context, string) (*proxydb.NodeSchedule, error) {
	return f.row, f.err
}

func TestHeartbeatSendsScheduleOnlyToKTMSManagedNodes(t *testing.T) {
	s, mgr := newRegistry(t)
	lvl, until := int32(0), time.Now().Add(time.Hour)
	expired := time.Now().Add(-time.Minute)
	sched := &fakeSchedules{row: &proxydb.NodeSchedule{NodeID: "node-1", Text: "09:00-18:00 0\n", Version: 7, OverrideLevel: &lvl, OverrideUntil: &until}}
	s.SetSchedules(sched)
	ctx := apikeys.WithIdentity(context.Background(), &apikeys.Identity{NodeID: "node-1"})
	if _, err := s.RegisterNode(ctx, &pb.RegisterNodeRequest{NodeId: "node-1", Endpoint: "http://127.0.0.1:9", Models: specs("tiny:1b")}); err != nil {
		t.Fatal(err)
	}

	resp, err := s.Heartbeat(ctx, &pb.HeartbeatRequest{NodeId: "node-1", ScheduleSource: "local"})
	if err != nil || resp.GetSchedule() != nil {
		t.Fatalf("local node got a schedule: %v %v", resp.GetSchedule(), err)
	}

	resp, err = s.Heartbeat(ctx, &pb.HeartbeatRequest{NodeId: "node-1", ScheduleSource: "ktms", CapacitySource: "ktms_override", CapacityUntil: timestamppb.New(until)})
	if err != nil {
		t.Fatal(err)
	}
	if g := resp.GetSchedule(); g.GetVersion() != 7 || g.GetText() != "09:00-18:00 0\n" || g.GetOverride().GetLevel() != 0 || !g.GetOverride().GetUntil().AsTime().Equal(until) {
		t.Fatalf("schedule = %v", g)
	}
	admin := &AdminServer{nodeMgr: mgr}
	nodes, _ := admin.ListNodes(context.Background(), &pb.ListNodesRequest{})
	if n := nodes.GetNodes()[0]; n.GetScheduleSource() != "ktms" || n.GetCapacitySource() != "ktms_override" || !n.GetCapacityUntil().AsTime().Equal(until) {
		t.Fatalf("ListNodes sources = %q %q %v", n.GetScheduleSource(), n.GetCapacitySource(), n.GetCapacityUntil())
	}

	sched.row.OverrideUntil = &expired
	resp, _ = s.Heartbeat(ctx, &pb.HeartbeatRequest{NodeId: "node-1", ScheduleSource: "ktms"})
	if resp.GetSchedule().GetOverride() != nil {
		t.Fatal("an expired override was sent")
	}

	sched.row, sched.err = nil, proxydb.ErrNotFound
	resp, _ = s.Heartbeat(ctx, &pb.HeartbeatRequest{NodeId: "node-1", ScheduleSource: "ktms"})
	if g := resp.GetSchedule(); g == nil || g.GetVersion() != 0 || g.GetText() != "" {
		t.Fatalf("no row: schedule = %v, want version 0", g)
	}

	sched.err = errors.New("db down")
	resp, err = s.Heartbeat(ctx, &pb.HeartbeatRequest{NodeId: "node-1", ScheduleSource: "ktms"})
	if err != nil || !resp.GetOk() || resp.GetSchedule() != nil {
		t.Fatalf("db down: resp %v err %v; want an ok heartbeat without a schedule", resp, err)
	}
}

func TestNodeScheduleAdminRPCs(t *testing.T) {
	db := rotateTestDB(t)
	s := NewAdminServer(db, nil, nil)
	ctx := context.Background()

	if info, err := s.GetNodeSchedule(ctx, &pb.GetNodeScheduleRequest{NodeId: "mac"}); err != nil || info.GetVersion() != 0 {
		t.Fatalf("unset: %v %v", info, err)
	}
	if _, err := s.SetNodeSchedule(ctx, &pb.SetNodeScheduleRequest{NodeId: "mac", Text: "10:00-11:00 50\n10:30-12:00 0\n"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("overlapping rules: %v", err)
	}
	if _, err := s.SetNodeSchedule(ctx, &pb.SetNodeScheduleRequest{Text: "10:00-11:00 0\n"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no node id: %v", err)
	}
	info, err := s.SetNodeSchedule(ctx, &pb.SetNodeScheduleRequest{NodeId: "mac", Text: "mon-fri 09:00-18:00 0\n"})
	if err != nil || info.GetVersion() != 1 {
		t.Fatalf("set: %v %v", info, err)
	}
	for _, bad := range []*pb.SetNodeCapacityOverrideRequest{
		{NodeId: "mac", Level: 0},
		{NodeId: "mac", Level: 101, Until: timestamppb.New(time.Now().Add(time.Hour))},
		{NodeId: "mac", Level: 0, Until: timestamppb.New(time.Now().Add(10 * time.Second))},
		{NodeId: "mac", Level: 0, Until: timestamppb.New(time.Now().Add(25 * time.Hour))},
	} {
		if _, err := s.SetNodeCapacityOverride(ctx, bad); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%v: %v, want InvalidArgument", bad, err)
		}
	}
	info, err = s.SetNodeCapacityOverride(ctx, &pb.SetNodeCapacityOverrideRequest{NodeId: "mac", Level: 30, Until: timestamppb.New(time.Now().Add(time.Hour))})
	if err != nil || info.GetVersion() != 2 || info.GetOverride().GetLevel() != 30 {
		t.Fatalf("override: %v %v", info, err)
	}
	info, err = s.SetNodeCapacityOverride(ctx, &pb.SetNodeCapacityOverrideRequest{NodeId: "mac", Clear: true})
	if err != nil || info.GetVersion() != 3 || info.GetOverride() != nil || info.GetText() != "mon-fri 09:00-18:00 0\n" {
		t.Fatalf("clear: %v %v", info, err)
	}
	list, err := s.ListNodeSchedules(ctx, &pb.ListNodeSchedulesRequest{})
	if err != nil || len(list.GetSchedules()) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	if _, err := s.DeleteNodeSchedule(ctx, &pb.DeleteNodeScheduleRequest{NodeId: "mac"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteNodeSchedule(ctx, &pb.DeleteNodeScheduleRequest{NodeId: "mac"}); status.Code(err) != codes.NotFound {
		t.Fatalf("second delete: %v", err)
	}
}
