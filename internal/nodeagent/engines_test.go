package nodeagent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeEngine answers /v1/models with ids and echoes which engine took a POST.
func fakeEngine(t *testing.T, name string, ids ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			data := []map[string]string{}
			for _, id := range ids {
				data = append(data, map[string]string{"id": id, "object": "model"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
			return
		}
		_, _ = io.WriteString(w, `{"engine":"`+name+`"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer access-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestGatewayRoutesByModel(t *testing.T) {
	chat, mt := fakeEngine(t, "vllm", "gemma"), fakeEngine(t, "mt", "opus-mt-de")
	gw := NewMultiGateway("vllm", chat.URL, []EngineSpec{{Name: "mt", URL: mt.URL}}, "access-key", "")
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	gw.SetRoutes(map[string]string{"gemma": "vllm", "opus-mt-de": "mt"}, nil)

	for _, tc := range []struct{ path, body, want string }{
		{"/v1/chat/completions", `{"model":"gemma"}`, "vllm"},
		{"/v1/translate", `{"model":"opus-mt-de","texts":["a"]}`, "mt"},
		{"/v1/translate", `{"model":"unknown"}`, "mt"},          // translate without a known model: the mt engine
		{"/v1/chat/completions", `{"model":"unknown"}`, "vllm"}, // anything else: the primary
	} {
		if code, body := post(t, srv.URL, tc.path, tc.body); code != 200 || !strings.Contains(body, tc.want) {
			t.Errorf("%s %s = %d %s, want engine %s", tc.path, tc.body, code, body, tc.want)
		}
	}

	gw.SetRoutes(map[string]string{"gemma": "vllm", "opus-mt-de": "mt"}, []string{"mt"})
	if code, _ := post(t, srv.URL, "/v1/translate", `{"model":"opus-mt-de"}`); code != http.StatusServiceUnavailable {
		t.Errorf("paused engine: status %d, want 503", code)
	}
	if code, _ := post(t, srv.URL, "/v1/chat/completions", `{"model":"gemma"}`); code != 200 {
		t.Errorf("the other engine keeps serving: status %d", code)
	}

	gw.SetRoutes(map[string]string{"gemma": "vllm", "opus-mt-de": "mt"}, nil)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer access-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	if len(list.Data) != 2 {
		t.Errorf("/v1/models lists %v, want both engines' models", list.Data)
	}
}

func TestInspectEnginesAndPause(t *testing.T) {
	chat, mt := fakeEngine(t, "vllm", "gemma"), fakeEngine(t, "mt", "opus-mt-de")
	a := NewAgent(AgentConfig{
		Engine: "vllm", EngineURL: chat.URL,
		ExtraEngines: []EngineSpec{{Name: "mt", URL: mt.URL}},
		StateDir:     t.TempDir(),
	})
	ids := func() []string {
		var out []string
		for _, m := range a.inspectEngines(context.Background()) {
			out = append(out, m.GetModelId())
		}
		return out
	}
	if got := ids(); !slices.Equal(got, []string{"gemma", "opus-mt-de"}) {
		t.Fatalf("models = %v", got)
	}
	if a.engineOf("opus-mt-de") != "mt" || !a.primaryEngineModel("gemma") || a.primaryEngineModel("opus-mt-de") {
		t.Errorf("engine map wrong: %+v", a.engines.byModel)
	}

	if err := a.setEnginePaused("mt", true); err != nil {
		t.Fatal(err)
	}
	if got := ids(); !slices.Equal(got, []string{"gemma"}) {
		t.Errorf("paused engine still reported: %v", got)
	}
	st := a.engineStates()
	if len(st) != 2 || !st[1].Paused || !slices.Equal(st[1].Models, []string{"opus-mt-de"}) {
		t.Errorf("states = %+v", st)
	}
	if err := a.setEnginePaused("vllm", true); err == nil {
		t.Error("pausing every engine must be refused")
	}
	if err := a.setEnginePaused("nope", true); err == nil {
		t.Error("an unknown engine must be refused")
	}

	// A restart keeps the pause.
	b := NewAgent(a.cfg)
	b.loadEnginePauses()
	if !slices.Equal(b.pausedEngines(), []string{"mt"}) {
		t.Errorf("after restart paused = %v (file %s)", b.pausedEngines(), filepath.Join(a.cfg.StateDir, "engines.json"))
	}
	if err := b.setEnginePaused("mt", false); err != nil || len(b.pausedEngines()) != 0 {
		t.Errorf("resume: %v, paused %v", err, b.pausedEngines())
	}
}

func TestParseEngineSpec(t *testing.T) {
	if e, err := ParseEngineSpec("MT=http://127.0.0.1:11436"); err != nil || e.Name != "mt" || e.URL != "http://127.0.0.1:11436" {
		t.Errorf("got %+v %v", e, err)
	}
	for _, bad := range []string{"mt", "=http://x", "mt=", "mt=localhost:11436"} {
		if _, err := ParseEngineSpec(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
