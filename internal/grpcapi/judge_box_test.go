package grpcapi

import (
	"slices"
	"strings"
	"testing"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/router"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestBoxNarrowsTheChainAndNeedsAnImage(t *testing.T) {
	img := &pb.MediaPart{Name: "road", Part: &pb.MediaPart_Image{Image: &pb.ImagePart{Data: []byte("x"), MimeType: "image/png"}}}
	req, err := judgeRequestFromProto(&pb.JudgeRequest{
		StateMedia: []*pb.MediaPart{img},
		Questions:  map[string]*pb.JudgeQuestion{"crack": {Type: pb.JudgeType_JUDGE_TYPE_BOX, Instructions: "the crack in `road`"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.StateMedia[0].Name != "road" || req.Questions["crack"].Type != llm.JudgeBox {
		t.Fatalf("converted = %+v", req)
	}
	caps := judgeCapabilities(req)
	if !slices.Contains(caps, registry.CapabilityBox) || !slices.Contains(caps, registry.CapabilityVision) {
		t.Errorf("caps = %v, want judge, box and vision", caps)
	}

	_, err = judgeRequestFromProto(&pb.JudgeRequest{
		StateJson: `{"a":1}`,
		Questions: map[string]*pb.JudgeQuestion{"crack": {Type: pb.JudgeType_JUDGE_TYPE_BOX, Instructions: "?"}},
	})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "needs an image") {
		t.Errorf("box without an image: %v", err)
	}

	dup := &pb.MediaPart{Name: "road", Part: &pb.MediaPart_Text{Text: &pb.TextPart{Text: "t"}}}
	if _, err := judgeRequestFromProto(&pb.JudgeRequest{StateMedia: []*pb.MediaPart{img, dup},
		Questions: map[string]*pb.JudgeQuestion{"q": {Type: pb.JudgeType_JUDGE_TYPE_NOUL, Instructions: "?"}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("duplicate name: %v", err)
	}
}

// A bad image is the caller's error: bad_request with the reason, not vendor_error.
func TestInvalidMediaIsBadRequest(t *testing.T) {
	err := router.ToStatus(&llm.InvalidMediaError{Msg: "state_media[0]: image is 1920x1080 px"})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "1920x1080") {
		t.Fatalf("status = %v", err)
	}
}

func TestCriteriaJSONChecks(t *testing.T) {
	ask := func(q *pb.JudgeQuestion) error {
		_, err := judgeRequestFromProto(&pb.JudgeRequest{StateJson: `{}`, Questions: map[string]*pb.JudgeQuestion{"q": q}})
		return err
	}
	ok := &pb.JudgeQuestion{Type: pb.JudgeType_JUDGE_TYPE_SCORE, InstructionsJson: `{"text":"how worn?"}`, CriteriaJson: `["new","worn","broken"]`}
	if err := ask(ok); err != nil {
		t.Fatalf("valid structured score: %v", err)
	}
	for name, q := range map[string]*pb.JudgeQuestion{
		"mixed":      {Type: pb.JudgeType_JUDGE_TYPE_CHOICE, Instructions: "?", Options: map[string]string{"a": "1", "b": "2"}, CriteriaJson: `{"a":"1","b":"2"}`},
		"one option": {Type: pb.JudgeType_JUDGE_TYPE_CHOICE, Instructions: "?", CriteriaJson: `{"a":"1"}`},
		"noul keys":  {Type: pb.JudgeType_JUDGE_TYPE_NOUL, Instructions: "?", CriteriaJson: `{"yes":"1"}`},
		"bad json":   {Type: pb.JudgeType_JUDGE_TYPE_NOUL, InstructionsJson: `{`},
	} {
		if status.Code(ask(q)) != codes.InvalidArgument {
			t.Errorf("%s: want bad_request", name)
		}
	}
}

// A score in criteria_json still has its levels as text (legends, composite
// scores, guardrails); bad combinations are refused.
func TestStructuredQuestionsAreReadAsText(t *testing.T) {
	req, err := judgeRequestFromProto(&pb.JudgeRequest{StateJson: `{}`, Questions: map[string]*pb.JudgeQuestion{
		"wear": {Type: pb.JudgeType_JUDGE_TYPE_SCORE, Instructions: "how worn?", CriteriaJson: `["new", {"description":"worn","examples":["ex"]}]`}}})
	if err != nil {
		t.Fatal(err)
	}
	if lv := req.Questions["wear"].Levels; len(lv) != 2 || lv[0] != "new" || lv[1] != `{"description":"worn","examples":["ex"]}` {
		t.Fatalf("levels = %q", lv)
	}
	for name, q := range map[string]*pb.JudgeQuestion{
		"empty noul": {Type: pb.JudgeType_JUDGE_TYPE_NOUL, Instructions: "?", CriteriaJson: `{}`},
		"both":       {Type: pb.JudgeType_JUDGE_TYPE_NOUL, Instructions: "?", InstructionsJson: `["?"]`},
	} {
		if _, err := judgeRequestFromProto(&pb.JudgeRequest{StateJson: `{}`, Questions: map[string]*pb.JudgeQuestion{"q": q}}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: want bad_request, got %v", name, err)
		}
	}
	_, err = evaluateRolloutsRequestFromProto(&pb.EvaluateRolloutsRequest{StateJson: `{}`,
		Candidates: []*pb.RolloutCandidate{{Id: "a", ActionJson: `{}`}, {Id: "b", ActionJson: `{}`}},
		Criteria:   map[string]*pb.JudgeQuestion{"q": {Type: pb.JudgeType_JUDGE_TYPE_NOUL, Instructions: "?", CriteriaJson: `{"true":"y"}`}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("rollouts with criteria_json: %v", err)
	}
}

// Guardrails see every string of the JSON forms, never the option keys.
func TestMapJSONStrings(t *testing.T) {
	out, err := llm.MapJSONStrings([]byte(`{"pothole":{"description":"call 555-0100","examples":["ex"]}}`),
		func(s string) (string, error) { return strings.ReplaceAll(s, "555-0100", "[phone]"), nil })
	if err != nil || string(out) != `{"pothole":{"description":"call [phone]","examples":["ex"]}}` {
		t.Fatalf("out = %s, %v", out, err)
	}
}
