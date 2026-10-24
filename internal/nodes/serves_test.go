package nodes

import (
	"context"
	"testing"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/registry"
)

type listed []*NodeSnapshot

func (l listed) ListNodes(context.Context) ([]*NodeSnapshot, error) { return l, nil }

// Serves answers as SelectNode filters: a node that offers a model only as a
// judge, or is draining, does not take generation calls for it.
func TestServes(t *testing.T) {
	node := func(state pb.NodeState, caps ...string) *NodeSnapshot {
		return &NodeSnapshot{State: state, Meta: &NodeMeta{Models: []*pb.NodeModelSpec{{ModelId: "m", Capabilities: caps}}}}
	}
	for name, tc := range map[string]struct {
		nodes listed
		want  bool
	}{
		"healthy":    {listed{node(pb.NodeState_NODE_STATE_HEALTHY)}, true},
		"suspect":    {listed{node(pb.NodeState_NODE_STATE_SUSPECT)}, true},
		"draining":   {listed{node(pb.NodeState_NODE_STATE_DRAINING)}, false},
		"judge only": {listed{node(pb.NodeState_NODE_STATE_HEALTHY, registry.CapabilityJudge)}, false},
		"none":       {nil, false},
	} {
		if got, err := Serves(context.Background(), tc.nodes, "m"); err != nil || got != tc.want {
			t.Errorf("%s: Serves = %v %v, want %v", name, got, err, tc.want)
		}
	}
}
