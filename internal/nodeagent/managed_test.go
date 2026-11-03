package nodeagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

func ktmsMode(c *AgentConfig) { c.ScheduleSource = ScheduleSourceKTMS }

func TestKTMSModeFollowsKTMSAndOwnerPauseWins(t *testing.T) {
	clock := &fakeClock{t: at("09:00")}
	a, reg, _, c := startControlledWith(t, "00:00-24:00 50\n", clock, &pb.NodeSchedule{Text: "00:00-24:00 30\n", Version: 1}, ktmsMode)
	waitLevel(t, reg, 30, 5*time.Second)
	if hb := reg.lastBeat(); hb.GetScheduleSource() != "ktms" || hb.GetCapacitySource() != SourceKTMSSchedule {
		t.Fatalf("heartbeat sources = %q %q", hb.GetScheduleSource(), hb.GetCapacitySource())
	}
	if _, err := os.Stat(filepath.Join(a.cfg.StateDir, "managed-schedule")); err != nil {
		t.Fatalf("schedule from KTMS not stored: %v", err)
	}

	until := at("10:00")
	reg.setSchedule(&pb.NodeSchedule{Text: "00:00-24:00 30\n", Version: 2, Override: &pb.NodeCapacityOverride{Level: 0, Until: timestamppb.New(until)}})
	waitLevel(t, reg, 0, 5*time.Second)
	waitFor(t, "until in the heartbeat", func() bool {
		hb := reg.lastBeat()
		return hb.GetCapacitySource() == SourceKTMSOverride && hb.GetCapacityUntil().AsTime().Equal(until)
	})

	ctx := context.Background()
	if _, err := c.SetOverride(ctx, OverrideRequest{Level: 70, For: "10m"}); err != nil {
		t.Fatal(err)
	}
	waitLevel(t, reg, 70, time.Second)
	s, err := c.ClearOverride(ctx)
	if err != nil || s.Level.Effective != 0 || s.Level.Source != SourceKTMSOverride || s.Level.KTMS == nil || s.Level.KTMS.Version != 2 {
		t.Fatalf("after resume: %+v, %v", s.Level, err)
	}

	// The override from KTMS ends by the wall clock, like the owner's.
	clock.Set(at("10:01"))
	waitLevel(t, reg, 30, time.Second)

	if _, err := c.PutSchedule(ctx, "00:00-24:00 10\n"); !isStatus(err, 409) {
		t.Fatalf("local edit in ktms mode: %v, want 409", err)
	}
	info, err := c.Schedule(ctx)
	if err != nil || info.Editable || info.Source != ScheduleSourceKTMS || info.Text != "00:00-24:00 30\n" {
		t.Fatalf("schedule in ktms mode = %+v, %v", info, err)
	}
}

func TestSwitchScheduleSourceAtRunTime(t *testing.T) {
	_, reg, _, c := startControlledWith(t, "00:00-24:00 50\n", &fakeClock{t: at("09:00")}, &pb.NodeSchedule{Text: "00:00-24:00 30\n", Version: 1})
	waitLevel(t, reg, 50, 5*time.Second)
	ctx := context.Background()
	if _, err := c.SetScheduleSource(ctx, "ktms"); err != nil {
		t.Fatal(err)
	}
	waitLevel(t, reg, 30, 5*time.Second)
	if s, _ := c.Settings(ctx); s.ScheduleSource != "ktms" {
		t.Fatalf("settings = %+v", s)
	}
	if _, err := c.SetScheduleSource(ctx, "elsewhere"); !isStatus(err, 422) {
		t.Fatalf("bad source: %v", err)
	}
	if _, err := c.SetScheduleSource(ctx, "local"); err != nil {
		t.Fatal(err)
	}
	waitLevel(t, reg, 50, 5*time.Second)
}

func TestKTMSModeRestartsOnLastScheduleWithoutProxyAnswer(t *testing.T) {
	dir, err := os.MkdirTemp("", "kn")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := writeJSON0600(filepath.Join(dir, "settings.json"), Settings{ScheduleSource: "ktms"}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON0600(filepath.Join(dir, "managed-schedule"), managedSchedule{Text: "00:00-24:00 20\n", Version: 4}); err != nil {
		t.Fatal(err)
	}
	// The flag says local and the proxy sends nothing: the stored choice and schedule hold.
	_, reg, _ := startAgent(t, "00:00-24:00 50\n", &fakeClock{t: at("09:00")}, func(c *AgentConfig) { c.StateDir = dir })
	waitLevel(t, reg, 20, 5*time.Second)
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for _, l := range reg.levels {
		if l != 20 {
			t.Fatalf("levels %v: the restart left the last schedule from KTMS", reg.levels)
		}
	}
}

func TestKTMSModeWithoutScheduleRunsFullAndRefusesBadOnes(t *testing.T) {
	_, reg, _, c := startControlledWith(t, "00:00-24:00 50\n", &fakeClock{t: at("09:00")}, &pb.NodeSchedule{}, ktmsMode)
	waitLevel(t, reg, 100, 5*time.Second)
	reg.setSchedule(&pb.NodeSchedule{Text: "00:00-24:00 40\n", Version: 1})
	waitLevel(t, reg, 40, 5*time.Second)
	reg.setSchedule(&pb.NodeSchedule{Text: "10:00-11:00 0\n10:30-12:00 0\n", Version: 2})
	waitFor(t, "the refusal", func() bool {
		s, err := c.Status(context.Background())
		return err == nil && s.ScheduleError != ""
	})
	s, _ := c.Status(context.Background())
	if s.Level.Effective != 40 || s.Level.KTMS.Version != 1 {
		t.Fatalf("after a bad schedule: %+v", s.Level)
	}
}

func isStatus(err error, code int) bool {
	var ce *ControlCallError
	return errors.As(err, &ce) && ce.Status == code
}
