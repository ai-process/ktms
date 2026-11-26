package adapters

import (
	"context"
	"testing"

	"github.com/ai-process/ktms/internal/llm"
)

type stubJudge struct{ *stubAdapter }

func (stubJudge) Judge(context.Context, *llm.JudgeRequest, string, map[string]string) (*llm.JudgeResult, error) {
	return &llm.JudgeResult{Answers: map[string]*llm.JudgeAnswer{}, Usage: llm.TokensUsage{Input: 900}}, nil
}

// Image tokens have no formula yet: usage meta counts the images per call.
func TestTrackedJudgeCountsImages(t *testing.T) {
	tracker := &capturingTracker{}
	a := NewTrackingAdapter(stubJudge{&stubAdapter{}}, tracker, "typesafe", "jev-1.14.0")
	_, err := a.Judge(context.Background(), &llm.JudgeRequest{
		StateMedia: []*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage}, {Type: llm.JudgeMediaText}, {Type: llm.JudgeMediaImage}},
		Questions:  map[string]*llm.JudgeQuestion{"q": {Type: llm.JudgeNoul}},
	}, "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tracker.meta["images"] != "2" {
		t.Fatalf("meta = %v", tracker.meta)
	}
}
