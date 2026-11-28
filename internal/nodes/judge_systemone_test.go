package nodes

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

// systemOneNode registers a node whose engine answers handler, serving one
// System One judge model, and returns a pool adapter for it.
func systemOneNode(t *testing.T, handler http.HandlerFunc) *PoolAdapter {
	t.Helper()
	pa, _ := systemOneNodeAt(t, "/v1", handler)
	return pa
}

func systemOneNodeAt(t *testing.T, suffix string, handler http.HandlerFunc) (*PoolAdapter, *Manager) {
	t.Helper()
	ctx := context.Background()
	mgr, _ := setupTestManager(t)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	meta := &NodeMeta{
		NodeID: "node-decide", Name: "Decide", Endpoint: srv.URL + suffix, Engine: "ollama",
		AuthToken: "node-token", BaseWeight: 100, MaxConcurrent: 4,
		Models: []*pb.NodeModelSpec{{
			ModelId: "pplx-flash", RemoteModelName: "pplx-decider-flash:4b", ContextLength: 32768, IsWarm: true,
			Capabilities: []string{registry.CapabilityJudge},
		}},
	}
	if err := mgr.Register(ctx, meta); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, _, err := mgr.Heartbeat(ctx, &NodeHeartbeatData{NodeID: meta.NodeID, WarmModels: []string{"pplx-flash"}}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	model := &proxydb.Model{ID: "pplx-flash", Vendor: registry.VendorLocal,
		Capabilities: []string{registry.CapabilityJudge, registry.CapabilitySystemOne, registry.CapabilityVision}}
	return NewPoolAdapter(NewSelector(mgr), nil, model, 30*time.Second, nil), mgr
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 320, 240))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// A model declaring systemone is judged on the node's /v1/systemone with the
// node token, images as bare base64.
func TestPoolAdapter_Judge_SystemOne(t *testing.T) {
	img := testPNG(t)
	var got struct {
		Model     string          `json:"model"`
		State     json.RawMessage `json:"state"`
		Images    []string        `json:"images"`
		Questions map[string]any  `json:"questions"`
		Assets    map[string]any  `json:"assets"`
	}
	pa := systemOneNode(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer node-token" {
			http.Error(w, "wrong route or token", http.StatusTeapot)
			return
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = io.WriteString(w, `{"model":"pplx-decider-flash:4b","answers":{"ok":{"type":"noul","noul":0.7}},"usage":{"input_tokens":300,"output_tokens":1}}`)
	})
	meta := map[string]string{}
	res, err := pa.Judge(context.Background(), &llm.JudgeRequest{
		StateJSON:  `{"camera":"front"}`,
		StateMedia: []*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, Data: img}},
		Questions:  map[string]*llm.JudgeQuestion{"ok": {Type: llm.JudgeNoul, Instructions: "is a road visible?"}},
	}, "u", meta)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if res.Answers["ok"].Noul != 0.7 || res.Usage.Input != 300 || meta["node_id"] != "node-decide" {
		t.Errorf("result %+v meta %v", res, meta)
	}
	if got.Model != "pplx-decider-flash:4b" || len(got.Images) != 1 || got.Images[0] != base64.StdEncoding.EncodeToString(img) || got.Assets != nil {
		t.Errorf("request model %q images %d assets %v", got.Model, len(got.Images), got.Assets)
	}
	if string(got.State) != `{"camera":"front"}` {
		t.Errorf("state = %s", got.State)
	}
	// No rollouts route in System One: the caller judges candidates one by one.
	if _, err := pa.EvaluateRollouts(context.Background(), &llm.EvaluateRolloutsRequest{}, "u", nil); !errors.Is(err, llm.ErrJudgmentUnsupported) {
		t.Errorf("EvaluateRollouts: %v", err)
	}
}

// An engine without /v1/systemone (an older Ollama) yields to the next model.
func TestPoolAdapter_Judge_SystemOneNotFoundYields(t *testing.T) {
	pa := systemOneNode(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	_, err := pa.Judge(context.Background(), &llm.JudgeRequest{StateJSON: `{}`,
		Questions: map[string]*llm.JudgeQuestion{"ok": {Type: llm.JudgeNoul, Instructions: "?"}}}, "u", nil)
	if !errors.Is(err, llm.ErrJudgmentUnsupported) {
		t.Fatalf("err = %v", err)
	}
}

// However the node wrote its endpoint, the call lands on /v1/systemone.
func TestPoolAdapter_Judge_SystemOneEndpointForms(t *testing.T) {
	for _, suffix := range []string{"", "/", "/v1", "/v1/", "/v1//"} {
		var path string
		pa, _ := systemOneNodeAt(t, suffix, func(w http.ResponseWriter, r *http.Request) {
			path = r.URL.Path
			_, _ = io.WriteString(w, `{"answers":{"ok":{"type":"noul","noul":0.5}}}`)
		})
		if _, err := pa.Judge(context.Background(), &llm.JudgeRequest{StateJSON: `{}`,
			Questions: map[string]*llm.JudgeQuestion{"ok": {Type: llm.JudgeNoul, Instructions: "?"}}}, "u", nil); err != nil {
			t.Fatalf("%q: %v", suffix, err)
		}
		if path != "/v1/systemone" {
			t.Errorf("endpoint %q called %q", suffix, path)
		}
	}
}

// Requests the engine refuses and a missing route leave the node's circuit
// closed; engine failures open it.
func TestPoolAdapter_Judge_SystemOneCircuit(t *testing.T) {
	ctx := context.Background()
	var status atomic.Int32
	pa, mgr := systemOneNodeAt(t, "/v1", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, `{"error":"simulated"}`)
	})
	req := &llm.JudgeRequest{StateJSON: `{}`, Questions: map[string]*llm.JudgeQuestion{"ok": {Type: llm.JudgeNoul, Instructions: "?"}}}
	broken := func() bool {
		nodes, err := mgr.ListNodes(ctx)
		if err != nil || len(nodes) == 0 {
			t.Fatalf("ListNodes: %v", err)
		}
		return nodes[0].State == pb.NodeState_NODE_STATE_CIRCUIT_BROKEN
	}
	for _, code := range []int32{http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity, http.StatusNotFound} {
		status.Store(code)
		for i := 0; i < 6; i++ {
			if _, err := pa.Judge(ctx, req, "u", nil); err == nil {
				t.Fatalf("%d: no error", code)
			}
		}
		if broken() {
			t.Fatalf("%d tripped the node's circuit", code)
		}
	}
	status.Store(http.StatusInternalServerError)
	for i := 0; i < 6 && !broken(); i++ {
		_, _ = pa.Judge(ctx, req, "u", nil)
	}
	if !broken() {
		t.Error("engine 500s did not open the node's circuit")
	}
}

// A named image's name goes with the state, in the images' order.
func TestPoolAdapter_Judge_SystemOneNamedImage(t *testing.T) {
	var state string
	pa := systemOneNode(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			State json.RawMessage `json:"state"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		state = string(body.State)
		_, _ = io.WriteString(w, `{"answers":{"ok":{"type":"noul","noul":0.5}}}`)
	})
	_, err := pa.Judge(context.Background(), &llm.JudgeRequest{StateJSON: `{"camera":"front"}`,
		StateMedia: []*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, Name: "road", Data: testPNG(t)}},
		Questions:  map[string]*llm.JudgeQuestion{"ok": {Type: llm.JudgeNoul, Instructions: "is `road` usable?"}}}, "u", nil)
	if err != nil {
		t.Fatal(err)
	}
	if state != `{"images":["road"],"state":{"camera":"front"}}` {
		t.Errorf("state = %s", state)
	}
}

// After keepalives the gateway sends its error inside the 200 it committed:
// the engine's status still decides, so a 400 leaves the circuit closed and a 404 yields.
func TestPoolAdapter_Judge_SystemOneGatewayErrorIn200(t *testing.T) {
	ctx := context.Background()
	var status atomic.Int32
	pa, mgr := systemOneNodeAt(t, "/v1", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, `   {"error":{"message":"engine returned %d","type":"node_gateway_error","status":%d}}`, status.Load(), status.Load())
	})
	req := &llm.JudgeRequest{StateJSON: `{}`, Questions: map[string]*llm.JudgeQuestion{"ok": {Type: llm.JudgeNoul, Instructions: "?"}}}
	status.Store(http.StatusNotFound)
	if _, err := pa.Judge(ctx, req, "u", nil); !errors.Is(err, llm.ErrJudgmentUnsupported) {
		t.Fatalf("404 in a 200: %v", err)
	}
	status.Store(http.StatusBadRequest)
	for i := 0; i < 6; i++ {
		if _, err := pa.Judge(ctx, req, "u", nil); err == nil {
			t.Fatal("400 in a 200 read as an answer")
		}
	}
	nodes, err := mgr.ListNodes(ctx)
	if err != nil || nodes[0].State == pb.NodeState_NODE_STATE_CIRCUIT_BROKEN {
		t.Errorf("400s in a 200 tripped the node's circuit (%v)", err)
	}
}

// A rollout's media is checked against the node's limits before any slot is
// taken, and such a model judges candidates one at a time.
func TestPoolAdapter_SystemOneRolloutChecks(t *testing.T) {
	pa := systemOneNode(t, func(w http.ResponseWriter, r *http.Request) { t.Error("engine called") })
	if !pa.JudgesSerially() {
		t.Error("a System One node model must judge serially")
	}
	gif := []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")
	if err := pa.CheckJudgeMedia([]*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, Data: gif}}, nil); !errors.Is(err, llm.ErrInvalidMedia) {
		t.Errorf("gif on the node: %v", err)
	}
	if err := pa.CheckJudgeMedia([]*llm.JudgeMediaPart{{Type: llm.JudgeMediaImage, Data: testPNG(t)}}, nil); err != nil {
		t.Errorf("png on the node: %v", err)
	}
}
