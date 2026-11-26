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
