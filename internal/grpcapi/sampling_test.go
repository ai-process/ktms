package grpcapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

func samplingRequest() *pb.GenerateTextRequest {
	return &pb.GenerateTextRequest{
		Effort:      pb.Effort_EFFORT_LOW,
		Messages:    []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "hi"}},
		Temperature: proto.Float64(0.2),
		TopP:        proto.Float64(0.9),
		Stop:        []string{"END"},
		Seed:        proto.Int64(7),
	}
}

func TestSamplingReachesTheModel(t *testing.T) {
	for _, c := range []struct {
		model   string
		sent    []string
		ignored []string
	}{
		{"gpt-4.1-mini", []string{"temperature", "top_p", "stop", "seed"}, nil},
		{"o4-mini", nil, []string{"temperature", "top_p", "stop", "seed"}},
	} {
		vendor := &fakeVendor{reply: "ok"}
		srv := httptest.NewServer(http.HandlerFunc(vendor.handler))
		models := []*proxydb.Model{testModel(c.model, registry.VendorOpenAI, srv.URL, registry.EffortLow)}
		s := newTestServer(t, models, catchAllRules(c.model), nil)

		resp, err := s.GenerateText(testIdentity(), samplingRequest())
		srv.Close()
		if err != nil {
			t.Fatalf("%s: %v", c.model, err)
		}
		var body map[string]any
		_ = json.Unmarshal(vendor.body(), &body)
		var sent []string
		for _, k := range []string{"temperature", "top_p", "stop", "seed"} {
			if _, ok := body[k]; ok {
				sent = append(sent, k)
			}
		}
		if !slices.Equal(sent, c.sent) || !slices.Equal(resp.GetIgnoredParameters(), c.ignored) {
			t.Errorf("%s: sent %v ignored %v", c.model, sent, resp.GetIgnoredParameters())
		}
	}
}

func TestSamplingIgnoredOnTheFirstChunk(t *testing.T) {
	vendor := &fakeVendor{reply: "ok"}
	srv := httptest.NewServer(http.HandlerFunc(vendor.handler))
	defer srv.Close()
	s := newTestServer(t, []*proxydb.Model{testModel("o4-mini", registry.VendorOpenAI, srv.URL, registry.EffortLow)}, catchAllRules("o4-mini"), nil)

	stream := &mockStreamServer{ctx: testIdentity()}
	if err := s.GenerateTextStream(&pb.GenerateTextStreamRequest{Request: samplingRequest()}, stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.chunks) == 0 || len(stream.chunks[0].GetIgnoredParameters()) != 4 {
		t.Fatalf("first chunk: %v", stream.chunks)
	}
}

func TestSamplingValidation(t *testing.T) {
	s := newTestServer(t, []*proxydb.Model{testModel("m1", registry.VendorOpenAI, "http://127.0.0.1:1", registry.EffortLow)}, catchAllRules("m1"), nil)
	for name, mutate := range map[string]func(*pb.GenerateTextRequest){
		"temperature above 2": func(r *pb.GenerateTextRequest) { r.Temperature = proto.Float64(2.5) },
		"negative top_p":      func(r *pb.GenerateTextRequest) { r.TopP = proto.Float64(-0.1) },
		"empty stop":          func(r *pb.GenerateTextRequest) { r.Stop = []string{""} },
		"too many stops":      func(r *pb.GenerateTextRequest) { r.Stop = make([]string, maxStopSequences+1) },
	} {
		req := samplingRequest()
		mutate(req)
		if _, err := s.GenerateText(testIdentity(), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}
