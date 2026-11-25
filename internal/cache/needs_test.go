package cache

import (
	"testing"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

// Two requests that differ only in needs may route to different models: they
// must not share a cached answer. A request without needs keeps its old key.
func TestKeyIncludesNeeds(t *testing.T) {
	msg := []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "hi"}}
	basic := &pb.GenerateTextRequest{Messages: msg, Needs: map[string]string{"medical": "basic"}}
	expert := &pb.GenerateTextRequest{Messages: msg, Needs: map[string]string{"medical": "expert"}}
	if ComputeKey("k", 1, basic) == ComputeKey("k", 1, expert) {
		t.Fatal("medical=basic and medical=expert share a cache key")
	}
	plain := &pb.GenerateTextRequest{Messages: msg}
	empty := &pb.GenerateTextRequest{Messages: msg, Needs: map[string]string{}}
	if ComputeKey("k", 1, plain) != ComputeKey("k", 1, empty) {
		t.Fatal("empty needs changed the key")
	}
}
