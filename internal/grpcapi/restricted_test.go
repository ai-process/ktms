package grpcapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/router"
)

// countingServer answers chat completions, or fails them with fail, and
// counts every request it gets.
func countingServer(t *testing.T, fail bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if fail {
			http.Error(w, "node down", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func restrictedServer(t *testing.T, nodeFails bool, extra ...*proxydb.Model) (*ProxyServer, *atomic.Int32, *atomic.Int32, *fakeTracker) {
	t.Helper()
	cloud, cloudCalls := countingServer(t, false)
	node, nodeCalls := countingServer(t, nodeFails)
	models := append([]*proxydb.Model{
		testModel("cloud-m", registry.VendorOpenAI, cloud.URL, registry.EffortMedium),
		testModel("node-m", registry.VendorOllama, node.URL, registry.EffortMedium),
	}, extra...)
	rules := []*proxydb.Rule{catchAllRule("r-med", registry.EffortMedium, "cloud-m", "node-m"), judgeRule("cloud-judge")}
	keys := map[string]map[string]string{
		"key1": {registry.VendorOpenAI: "sk-test", registry.VendorOllama: "ollama-test", registry.VendorTypeSafe: "ts-test"},
	}
	tr := &fakeTracker{}
	return NewProxyServer(fixedSnapshot{registry.NewSnapshotForTest(models, rules, keys, tr)}, nil), cloudCalls, nodeCalls, tr
}

func restrictedCtx() context.Context {
	return apikeys.WithIdentity(context.Background(), &apikeys.Identity{
		KeyID: "key1", Name: "clinic", Scopes: []string{apikeys.ScopeGenerate}, TeamDataClassification: apikeys.DataRestricted,
	})
}

func hi(class string) *pb.GenerateTextRequest {
	return &pb.GenerateTextRequest{
		Effort:             pb.Effort_EFFORT_MEDIUM,
		Messages:           []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "patient notes"}},
		DataClassification: class,
		EnableCache:        true,
	}
}

func TestRestrictedStaysOnNodes(t *testing.T) {
	s, cloudCalls, nodeCalls, tr := restrictedServer(t, false)
	resp, err := s.GenerateText(testIdentity(), hi(apikeys.DataRestricted))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetResolvedModel() != "node-m" || resp.GetPlacement() != router.PlacementNodesOnly || cloudCalls.Load() != 0 || nodeCalls.Load() != 1 {
		t.Fatalf("model %s placement %s cloud %d node %d", resp.GetResolvedModel(), resp.GetPlacement(), cloudCalls.Load(), nodeCalls.Load())
	}
	ev := tr.list()
	if len(ev) != 1 || ev[0].meta[DataClassificationAttribute] != apikeys.DataRestricted {
		t.Fatalf("usage events %+v", ev)
	}
}

func TestRestrictedNodeDownNeverFallsBackToCloud(t *testing.T) {
	s, cloudCalls, nodeCalls, _ := restrictedServer(t, true)
	_, err := s.GenerateText(testIdentity(), hi(apikeys.DataRestricted))
	if status.Code(err) != codes.Unavailable || cloudCalls.Load() != 0 || nodeCalls.Load() == 0 {
		t.Fatalf("err %v cloud %d node %d", err, cloudCalls.Load(), nodeCalls.Load())
	}
	// Unrestricted, the same failure falls back.
	if _, err := s.GenerateText(testIdentity(), hi("")); err != nil || cloudCalls.Load() != 1 {
		t.Fatalf("unrestricted: %v cloud %d", err, cloudCalls.Load())
	}
}

func TestRestrictedKeyOrTeamOverridesTheRequest(t *testing.T) {
	s, cloudCalls, _, _ := restrictedServer(t, false)
	for _, placement := range []string{"", router.PlacementCloudFirst, router.PlacementNodesFirst} {
		req := hi("")
		req.Placement = placement
		resp, err := s.GenerateText(restrictedCtx(), req)
		if err != nil || resp.GetResolvedModel() != "node-m" {
			t.Fatalf("placement %q: %v %v", placement, resp, err)
		}
	}
	if cloudCalls.Load() != 0 {
		t.Fatalf("cloud called %d times", cloudCalls.Load())
	}
	req := hi("")
	req.Placement = router.PlacementCloudOnly
	if _, err := s.GenerateText(restrictedCtx(), req); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("cloud_only: %v", err)
	}
	if _, err := s.GenerateText(testIdentity(), hi("secret")); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unknown class: %v", err)
	}
}

func TestRestrictedWithoutNodeFails(t *testing.T) {
	cloud, cloudCalls := countingServer(t, false)
	models := []*proxydb.Model{testModel("cloud-m", registry.VendorOpenAI, cloud.URL, registry.EffortMedium)}
	rules := []*proxydb.Rule{catchAllRule("r-med", registry.EffortMedium, "cloud-m")}
	keys := map[string]map[string]string{"key1": {registry.VendorOpenAI: "sk-test"}}
	s := NewProxyServer(fixedSnapshot{registry.NewSnapshotForTest(models, rules, keys, nil)}, nil)

	_, err := s.GenerateText(restrictedCtx(), hi(""))
	if status.Code(err) != codes.FailedPrecondition || cloudCalls.Load() != 0 {
		t.Fatalf("err %v cloud %d", err, cloudCalls.Load())
	}
	req := hi("")
	req.ModelOverride = "cloud-m"
	if _, err := s.GenerateText(restrictedCtx(), req); status.Code(err) != codes.FailedPrecondition || cloudCalls.Load() != 0 {
		t.Fatalf("override: %v cloud %d", err, cloudCalls.Load())
	}
}

// The auto-effort judge reads the prompt, so a cloud judge must not see a restricted one.
func TestRestrictedSkipsCloudJudge(t *testing.T) {
	judge, judgeCalls := countingServer(t, false)
	s, cloudCalls, _, _ := restrictedServer(t, false, judgeModel("cloud-judge", judge.URL))
	req := hi(apikeys.DataRestricted)
	req.Effort, req.AutoRoute = pb.Effort_EFFORT_UNSPECIFIED, true
	resp, err := s.GenerateText(testIdentity(), req)
	if err != nil || resp.GetRouteMode() != router.RouteModeAutoDefault || resp.GetResolvedModel() != "node-m" {
		t.Fatalf("%v %v", resp, err)
	}
	if judgeCalls.Load() != 0 || cloudCalls.Load() != 0 {
		t.Fatalf("judge %d cloud %d", judgeCalls.Load(), cloudCalls.Load())
	}
}

func TestRestrictedKeyMediaNeedsANode(t *testing.T) {
	judge, judgeCalls := countingServer(t, false)
	s, _, _, _ := restrictedServer(t, false, judgeModel("cloud-judge", judge.URL))
	_, err := s.Judge(restrictedCtx(), &pb.JudgeRequest{StateJson: `{"a":1}`, Questions: nounQuestion()})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "restricted") || judgeCalls.Load() != 0 {
		t.Fatalf("err %v judge %d", err, judgeCalls.Load())
	}
}

func TestRestrictedBatchNeedsANode(t *testing.T) {
	server, _, adapter := batchServerFixture()
	ctx := apikeys.WithIdentity(context.Background(), &apikeys.Identity{KeyID: "key-1", Name: "clinic", KeyDataClassification: apikeys.DataRestricted})
	_, err := server.SubmitBatch(ctx, &pb.SubmitBatchRequest{Effort: pb.Effort_EFFORT_MEDIUM,
		Items: []*pb.BatchItemRequest{{CustomId: "c1", Request: &pb.GenerateTextRequest{Messages: []*pb.ChatMessage{{Text: "x"}}}}}})
	if status.Code(err) != codes.FailedPrecondition || len(adapter.submitted) > 0 {
		t.Fatalf("err %v submitted %d", err, len(adapter.submitted))
	}
	_, err = server.SubmitBatch(authCtx("key-1", "c"), &pb.SubmitBatchRequest{Effort: pb.Effort_EFFORT_MEDIUM,
		Items: []*pb.BatchItemRequest{{CustomId: "c1", Request: &pb.GenerateTextRequest{Messages: []*pb.ChatMessage{{Text: "x"}}, DataClassification: apikeys.DataRestricted}}}})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("restricted item: %v", err)
	}
	_, err = server.SubmitBatch(authCtx("key-1", "c"), &pb.SubmitBatchRequest{Effort: pb.Effort_EFFORT_MEDIUM,
		Items: []*pb.BatchItemRequest{{CustomId: "c1", Request: &pb.GenerateTextRequest{Messages: []*pb.ChatMessage{{Text: "x"}}, DataClassification: "secret"}}}})
	if msg := status.Convert(err).Message(); status.Code(err) != codes.InvalidArgument || strings.Count(msg, "bad_request:") != 1 {
		t.Fatalf("invalid item: %v", err)
	}
}

func TestClassify(t *testing.T) {
	ctx := context.Background()
	req := hi("")
	if err := classify(ctx, &apikeys.Identity{KeyDataClassification: apikeys.DataRestricted}, req); err != nil ||
		req.GetDataClassification() != apikeys.DataRestricted || req.GetEnableCache() {
		t.Fatalf("%v %v", req, err)
	}
	req = hi("")
	if err := classify(apikeys.WithRestricted(ctx), &apikeys.Identity{}, req); err != nil || req.GetDataClassification() != apikeys.DataRestricted {
		t.Fatalf("header: %v %v", req, err)
	}
	req = hi("Restricted")
	if err := classify(ctx, &apikeys.Identity{}, req); err != nil || req.GetDataClassification() != apikeys.DataRestricted {
		t.Fatalf("case: %v %v", req, err)
	}
	req = hi("")
	if err := classify(ctx, &apikeys.Identity{}, req); err != nil || req.GetDataClassification() != "" || !req.GetEnableCache() {
		t.Fatalf("unrestricted: %v %v", req, err)
	}
	if err := classify(ctx, &apikeys.Identity{}, hi(apikeys.DataUnrestricted)); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a request cannot say unrestricted: %v", err)
	}
}

// A header-restricted call reaches media RPCs through the context.
func TestRestrictedHeaderMedia(t *testing.T) {
	judge, judgeCalls := countingServer(t, false)
	s, _, _, _ := restrictedServer(t, false, judgeModel("cloud-judge", judge.URL))
	_, err := s.Judge(apikeys.WithRestricted(testIdentity()), &pb.JudgeRequest{StateJson: `{"a":1}`, Questions: nounQuestion()})
	if status.Code(err) != codes.FailedPrecondition || judgeCalls.Load() != 0 {
		t.Fatalf("err %v judge %d", err, judgeCalls.Load())
	}
	if _, err := s.Judge(testIdentity(), &pb.JudgeRequest{StateJson: `{"a":1}`, Questions: nounQuestion()}); judgeCalls.Load() != 1 {
		t.Fatalf("unrestricted judge: %v %d", err, judgeCalls.Load())
	}
}

func TestExplainRouteRestricted(t *testing.T) {
	s, _, _, _ := restrictedServer(t, false)
	id := &apikeys.Identity{KeyID: "key1", Name: "clinic", Scopes: []string{apikeys.ScopeGenerate}}
	res := s.ExplainRoute(context.Background(), id, &pb.ExplainRouteRequest{Effort: pb.Effort_EFFORT_MEDIUM, DataClassification: apikeys.DataRestricted})
	if res.GetError() != "" || strings.Join(res.GetModels(), ",") != "node-m" {
		t.Fatalf("%v", res)
	}
}

// A caller cannot mark its own cloud call restricted in the usage report.
func TestRestrictedAttributeIsNotTheCallers(t *testing.T) {
	s, cloudCalls, _, tr := restrictedServer(t, false)
	req := hi("")
	req.Placement = router.PlacementCloudFirst
	req.Attributes = map[string]string{DataClassificationAttribute: apikeys.DataRestricted}
	if _, err := s.GenerateText(testIdentity(), req); err != nil || cloudCalls.Load() != 1 {
		t.Fatalf("%v cloud %d", err, cloudCalls.Load())
	}
	if ev := tr.list(); len(ev) != 1 || ev[0].meta[DataClassificationAttribute] != "" {
		t.Fatalf("usage events %+v", ev)
	}
}
