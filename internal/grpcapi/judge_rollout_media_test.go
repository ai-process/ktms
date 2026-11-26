package grpcapi

import (
	"bytes"
	"image"
	"image/png"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

func pngPart(t *testing.T, w, h int) *pb.MediaPart {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return &pb.MediaPart{Part: &pb.MediaPart_Image{Image: &pb.ImagePart{Data: b.Bytes(), MimeType: "image/png"}}}
}

func rolloutWith(state []*pb.MediaPart, actions ...[]*pb.MediaPart) *pb.EvaluateRolloutsRequest {
	req := &pb.EvaluateRolloutsRequest{
		StateMedia: state,
		Criteria:   map[string]*pb.JudgeQuestion{"ok": {Type: pb.JudgeType_JUDGE_TYPE_NOUL, Instructions: "usable?"}},
	}
	for i, a := range actions {
		req.Candidates = append(req.Candidates, &pb.RolloutCandidate{Id: string(rune('a' + i)), ActionMedia: a, ActionJson: `{}`})
	}
	return req
}

// Candidate media is the caller's to fix: bad_request naming the candidate
// and the part, checked before any vendor call.
func TestRolloutCandidateMediaIsBadRequest(t *testing.T) {
	fake := &fakeRolloutServer{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	s := newJudgeServer(t, []*proxydb.Model{judgeModel("jev-rollout", srv.URL)}, []*proxydb.Rule{judgeRule("jev-rollout")}, nil)

	many := make([]*pb.MediaPart, 20)
	for i := range many {
		many[i] = textPart("t")
	}
	big := textPart(strings.Repeat("x", 9<<20))
	named := textPart("t")
	named.Name = "road"
	cases := map[string]struct {
		req  *pb.EvaluateRolloutsRequest
		want string
	}{
		"bad part":     {rolloutWith(nil, nil, []*pb.MediaPart{textPart("")}), "candidates[1].action_media[0]: text is empty"},
		"parts":        {rolloutWith(many, many), "candidates[0]: state_media and action_media have 40 parts together"},
		"bytes":        {rolloutWith([]*pb.MediaPart{big}, []*pb.MediaPart{big}), "candidates[0]: state_media and action_media exceed"},
		"shared names": {rolloutWith([]*pb.MediaPart{named}, []*pb.MediaPart{named}), `candidates[0].action_media[0]: name "road" is used in state_media too`},
	}
	for name, c := range cases {
		_, err := s.EvaluateRollouts(testIdentity(), c.req)
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "bad_request: "+c.want) {
			t.Errorf("%s: err = %v, want bad_request %q", name, err, c.want)
		}
	}
	if fake.calls != 0 {
		t.Errorf("vendor called %d times", fake.calls)
	}
}

// TypeSafe's image limits apply to every candidate before the first one is
// sent, and the refusal points at the candidate's own part.
func TestRolloutTypeSafeImageCheckedBeforeAnyCall(t *testing.T) {
	fake := &fakeRolloutServer{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	m := judgeModel("jev-rollout", srv.URL)
	m.Capabilities = append(m.Capabilities, registry.CapabilityVision)
	s := newJudgeServer(t, []*proxydb.Model{m}, []*proxydb.Rule{judgeRule("jev-rollout")}, nil)

	state := []*pb.MediaPart{pngPart(t, 640, 480)}
	_, err := s.EvaluateRollouts(testIdentity(), rolloutWith(state,
		[]*pb.MediaPart{pngPart(t, 640, 480)},
		[]*pb.MediaPart{pngPart(t, 640, 480), pngPart(t, 1920, 1080)}))
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "candidates[1].action_media[1]: image is 1920x1080") {
		t.Fatalf("err = %v", err)
	}
	if fake.calls != 0 {
		t.Errorf("vendor called %d times", fake.calls)
	}

	_, err = s.EvaluateRollouts(testIdentity(), rolloutWith([]*pb.MediaPart{pngPart(t, 100, 100)}, []*pb.MediaPart{pngPart(t, 640, 480)}))
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "state_media[0]: image is 100x100") {
		t.Errorf("state image err = %v", err)
	}
}
