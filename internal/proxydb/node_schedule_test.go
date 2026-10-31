package proxydb

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNodeScheduleVersionsBump(t *testing.T) {
	d := setupTestDB(t)
	ctx := context.Background()
	id := "sched-test-node"
	_ = d.DeleteNodeSchedule(ctx, id)
	t.Cleanup(func() { _ = d.DeleteNodeSchedule(ctx, id) })

	if _, err := d.GetNodeSchedule(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing schedule: %v", err)
	}
	s, err := d.SetNodeScheduleText(ctx, id, "mon-fri 09:00-18:00 0\n")
	if err != nil || s.Version != 1 || s.OverrideLevel != nil {
		t.Fatalf("first set = %+v, %v", s, err)
	}
	lvl, until := int32(30), time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	s, err = d.SetNodeOverride(ctx, id, &lvl, &until)
	if err != nil || s.Version != 2 || *s.OverrideLevel != 30 || !s.OverrideUntil.Equal(until) || s.Text != "mon-fri 09:00-18:00 0\n" {
		t.Fatalf("override = %+v, %v", s, err)
	}
	s, err = d.SetNodeScheduleText(ctx, id, "")
	if err != nil || s.Version != 3 || s.OverrideLevel == nil {
		t.Fatalf("text change dropped the override or the version: %+v, %v", s, err)
	}
	if s, err = d.SetNodeOverride(ctx, id, nil, nil); err != nil || s.Version != 4 || s.OverrideLevel != nil {
		t.Fatalf("clear = %+v, %v", s, err)
	}
	other := "sched-test-node-2"
	t.Cleanup(func() { _ = d.DeleteNodeSchedule(ctx, other) })
	if s, err = d.SetNodeOverride(ctx, other, &lvl, &until); err != nil || s.Text != "" || s.Version != 1 {
		t.Fatalf("override on a node without rules = %+v, %v", s, err)
	}
	all, err := d.ListNodeSchedules(ctx)
	if err != nil || len(all) < 2 {
		t.Fatalf("list = %d, %v", len(all), err)
	}
	if err := d.DeleteNodeSchedule(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteNodeSchedule(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}
