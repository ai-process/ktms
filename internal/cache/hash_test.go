package cache

import (
	"testing"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

func TestComputeKey_Determinism(t *testing.T) {
	req1 := &pb.GenerateTextRequest{
		Effort: pb.Effort_EFFORT_MEDIUM,
		Attributes: map[string]string{
			"env":  "production",
			"tier": "gold",
		},
		Messages: []*pb.ChatMessage{
			{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "hello world"},
		},
		BaseSystemInstruction: "you are helpful",
		MaxOutputTokens:       100,
	}

	req2 := &pb.GenerateTextRequest{
		Effort: pb.Effort_EFFORT_MEDIUM,
		Attributes: map[string]string{
			"tier": "gold",
			"env":  "production",
		},
		Messages: []*pb.ChatMessage{
			{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "hello world"},
		},
		BaseSystemInstruction: "you are helpful",
		MaxOutputTokens:       100,
	}

	key1 := ComputeKey("key-123", 1, req1)
	key2 := ComputeKey("key-123", 1, req2)

	if key1 != key2 {
		t.Fatalf("expected identical keys regardless of map iteration order, got %s != %s", key1, key2)
	}
}

func TestComputeKey_TenantIsolation(t *testing.T) {
	req := &pb.GenerateTextRequest{
		Messages: []*pb.ChatMessage{
			{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "same prompt"},
		},
	}

	keyTenantA := ComputeKey("tenant-A", 1, req)
	keyTenantB := ComputeKey("tenant-B", 1, req)

	if keyTenantA == keyTenantB {
		t.Fatal("expected different keys for different tenants")
	}
}

func TestComputeKey_ConfigVersionInvalidation(t *testing.T) {
	req := &pb.GenerateTextRequest{
		Messages: []*pb.ChatMessage{
			{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "test prompt"},
		},
	}

	keyV1 := ComputeKey("key-1", 1, req)
	keyV2 := ComputeKey("key-1", 2, req)

	if keyV1 == keyV2 {
		t.Fatal("expected different keys when routing config version changes")
	}
}

func TestComputeKey_ParameterSensitivity(t *testing.T) {
	baseReq := func() *pb.GenerateTextRequest {
		return &pb.GenerateTextRequest{
			Effort: pb.Effort_EFFORT_MEDIUM,
			Messages: []*pb.ChatMessage{
				{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "prompt"},
			},
		}
	}

	baseKey := ComputeKey("key", 1, baseReq())

	diffEffort := baseReq()
	diffEffort.Effort = pb.Effort_EFFORT_HIGH
	if ComputeKey("key", 1, diffEffort) == baseKey {
		t.Error("expected different key on effort change")
	}

	diffOverride := baseReq()
	diffOverride.ModelOverride = "gpt-4o"
	if ComputeKey("key", 1, diffOverride) == baseKey {
		t.Error("expected different key on model_override change")
	}

	diffMaxTokens := baseReq()
	diffMaxTokens.MaxOutputTokens = 500
	if ComputeKey("key", 1, diffMaxTokens) == baseKey {
		t.Error("expected different key on max_output_tokens change")
	}

	diffSearch := baseReq()
	diffSearch.EnableGoogleSearch = true
	if ComputeKey("key", 1, diffSearch) == baseKey {
		t.Error("expected different key on enable_google_search change")
	}
}
