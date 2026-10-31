package declarative

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

const nodeScheduleExample = `node_schedules:
  - node_id: mac-studio
    schedule: |
      mon-fri 09:00-18:00 0
      mon-fri 18:00-23:00 50
  - node_id: gpu-box
    schedule: ""
`

func TestPlanNodeSchedules(t *testing.T) {
	c := mustParse(t, example+nodeScheduleExample)
	envs := env(map[string]string{"TEST_OPENAI_KEY": "sk-1"})
	live := liveOf(t, c, "sk-1")
	live.NodeSchedules = []*pb.NodeScheduleInfo{
		{NodeId: "mac-studio", Version: 3, Text: "mon-fri 09:00-18:00 0\n"},
		{NodeId: "old", Version: 1, Text: "00:00-24:00 0\n"},
		{NodeId: "unset", Version: 0},
		{NodeId: "override-only", Version: 2, Override: &pb.NodeCapacityOverride{Level: 0}},
	}
	p, err := Build(c, live, Options{Env: envs, Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	p.Write(&buf)
	out := buf.String()
	for _, want := range []string{"~ node schedule mac-studio", "- node schedule old"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "override-only") || strings.Contains(out, "unset") || strings.Contains(out, "gpu-box") {
		t.Errorf("a schedule without rules is a change:\n%s", out)
	}

	live.NodeSchedules, live.NodeSchedulesErr = nil, errors.New("Unimplemented")
	if _, err := Build(c, live, Options{Env: envs}); err == nil || !strings.Contains(err.Error(), "node_schedules") {
		t.Fatalf("an old Keystone: %v", err)
	}
	if _, err := Parse([]byte("api_version: " + APIVersion + "\nnode_schedules:\n  - node_id: a\n    schedule: \"10:00-11:00 0\\n10:30-12:00 0\\n\"\n")); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("overlapping rules: %v", err)
	}
	if _, err := Parse([]byte("api_version: " + APIVersion + "\nnode_schedules:\n  - node_id: a\n    schedule: ''\n  - node_id: a\n    schedule: ''\n")); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate node: %v", err)
	}
}

func TestExportNodeSchedulesParsesBack(t *testing.T) {
	live := emptyState()
	live.NodeSchedules = []*pb.NodeScheduleInfo{{NodeId: "mac", Version: 2, Text: "mon-fri 09:00-18:00 0\nsat 10:00-12:00 50\n"}, {NodeId: "none", Version: 0}, {NodeId: "held", Version: 1}}
	var buf bytes.Buffer
	if err := Export(live, &buf); err != nil {
		t.Fatal(err)
	}
	c, err := Parse(buf.Bytes())
	if err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if len(c.NodeSchedules) != 1 || c.NodeSchedules[0].Schedule != live.NodeSchedules[0].Text || !strings.Contains(buf.String(), "schedule: |") {
		t.Fatalf("parsed back %+v\n%s", c.NodeSchedules, buf.String())
	}
}
