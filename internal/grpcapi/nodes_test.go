package grpcapi

import (
	"context"
	"reflect"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/apikeys"
)

func specs(ids ...string) []*pb.NodeModelSpec {
	var out []*pb.NodeModelSpec
	for _, id := range ids {
		out = append(out, &pb.NodeModelSpec{ModelId: id})
	}
	return out
}

func TestScreenModels(t *testing.T) {
	for _, c := range []struct {
		name     string
		allowed  []string
		offered  []string
		kept     []string
		dropped  []string
		wantCode codes.Code
	}{
		{"no restriction keeps all", nil, []string{"a", "b"}, []string{"a", "b"}, nil, codes.OK},
		{"all allowed", []string{"a", "b"}, []string{"a", "b"}, []string{"a", "b"}, nil, codes.OK},
		{"extra model is dropped, node still registers", []string{"a"}, []string{"a", "gemma4:latest"}, []string{"a"}, []string{"gemma4:latest"}, codes.OK},
		{"nothing allowed is refused", []string{"a"}, []string{"x", "y"}, nil, []string{"x", "y"}, codes.PermissionDenied},
		{"no models offered is fine", []string{"a"}, nil, nil, nil, codes.OK},
	} {
		kept, dropped, err := screenModels(&apikeys.Identity{AllowedModels: c.allowed}, "n1", specs(c.offered...))
		if got := status.Code(err); got != c.wantCode {
			t.Errorf("%s: code = %v, want %v (%v)", c.name, got, c.wantCode, err)
		}
		var keptIDs []string
		for _, m := range kept {
			keptIDs = append(keptIDs, m.GetModelId())
		}
		if !reflect.DeepEqual(keptIDs, c.kept) || !reflect.DeepEqual(dropped, c.dropped) {
			t.Errorf("%s: kept %v dropped %v, want kept %v dropped %v", c.name, keptIDs, dropped, c.kept, c.dropped)
		}
	}
}

func TestValidateNode(t *testing.T) {
	s := &NodeRegistryServer{}
	ctx := apikeys.WithIdentity(context.Background(), &apikeys.Identity{NodeID: "node-1", AllowedModels: []string{"tiny:1b"}})

	resp, err := s.ValidateNode(ctx, &pb.ValidateNodeRequest{NodeId: "node-1", Models: specs("tiny:1b", "gemma4:latest")})
	if err != nil {
		t.Fatalf("ValidateNode: %v", err)
	}
	if !reflect.DeepEqual(resp.GetAcceptedModels(), []string{"tiny:1b"}) || !reflect.DeepEqual(resp.GetDroppedModels(), []string{"gemma4:latest"}) {
		t.Errorf("accepted %v dropped %v", resp.GetAcceptedModels(), resp.GetDroppedModels())
	}

	if _, err := s.ValidateNode(ctx, &pb.ValidateNodeRequest{NodeId: "node-1", Models: specs("gemma4:latest")}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("only forbidden models: code = %v, want PermissionDenied", status.Code(err))
	}
	if _, err := s.ValidateNode(ctx, &pb.ValidateNodeRequest{NodeId: "other"}); status.Code(err) != codes.PermissionDenied {
		t.Errorf("wrong node id: code = %v, want PermissionDenied", status.Code(err))
	}
	if _, err := s.ValidateNode(context.Background(), &pb.ValidateNodeRequest{NodeId: "node-1"}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("no identity: code = %v, want Unauthenticated", status.Code(err))
	}
}
