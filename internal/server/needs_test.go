package server

import (
	"maps"
	"net/http/httptest"
	"strings"
	"testing"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

func TestChatCompletionsNeeds(t *testing.T) {
	fp, h, key := newTestAPI(t)

	rec := do(h, "POST", "/v1/chat/completions", key, `{"model":"auto:medical=basic,legal=expert:cloud-only","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body)
	}
	if !maps.Equal(fp.text.Needs, map[string]string{"medical": "basic", "legal": "expert"}) || fp.text.Effort != pb.Effort_EFFORT_UNSPECIFIED || !fp.text.EffortSpecified {
		t.Errorf("auto:needs: needs %v, effort %v, specified %v", fp.text.Needs, fp.text.Effort, fp.text.EffortSpecified)
	}

	// reasoning_effort keeps meaning the general need, beside the model's needs.
	rec = do(h, "POST", "/v1/chat/completions", key, `{"model":"auto:medical=basic","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || fp.text.Effort != pb.Effort_EFFORT_HIGH || fp.text.Needs["medical"] != "basic" {
		t.Errorf("reasoning_effort beside needs: %d %v %v", rec.Code, fp.text.Effort, fp.text.Needs)
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"auto:medical=proficient","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-KTMS-Needs", "medical=basic, legal=basic")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !maps.Equal(fp.text.Needs, map[string]string{"medical": "proficient", "legal": "basic"}) {
		t.Errorf("header beside the model: %d %v", rec.Code, fp.text.Needs)
	}

	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-KTMS-Needs", "medical=basic")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || fp.text.Effort != pb.Effort_EFFORT_UNSPECIFIED || !fp.text.EffortSpecified {
		t.Errorf("header on auto must pin the route without a medium default: %d %v %v", rec.Code, fp.text.Effort, fp.text.EffortSpecified)
	}

	for _, model := range []string{"auto:medical=guru", "auto:medical=basic:legal=basic", "auto:medical"} {
		rec = do(h, "POST", "/v1/chat/completions", key, `{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != 400 {
			t.Errorf("%s: code = %d, want 400", model, rec.Code)
		}
	}
}

// An exact model gets no medium default: it is served at its own level.
func TestOverrideHasNoDefaultEffort(t *testing.T) {
	fp, h, key := newTestAPI(t)
	rec := do(h, "POST", "/v1/chat/completions", key, `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 || fp.text.Effort != pb.Effort_EFFORT_UNSPECIFIED || fp.text.ModelOverride != "gpt-4o-mini" {
		t.Fatalf("override: %d effort %v", rec.Code, fp.text.Effort)
	}
}

// /v1/decisions passes needs on to Judge; rollouts refuse them until the RPC takes them.
func TestDecisionsNeeds(t *testing.T) {
	fp, h, key := newTestAPI(t)
	body := `{"model":"auto:legal=proficient","input":"x","questions":[{"name":"q","type":"predicate","instructions":"ok?"}]}`
	req := httptest.NewRequest("POST", "/v1/decisions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-KTMS-Needs", "medical=basic")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !maps.Equal(fp.judgeReq.GetNeeds(), map[string]string{"legal": "proficient", "medical": "basic"}) {
		t.Fatalf("decisions: %d needs %v", rec.Code, fp.judgeReq.GetNeeds())
	}
	roll := `{"model":"auto:legal=basic","state":{"a":1},"candidates":[{"id":"c1","action":{"m":1}},{"id":"c2","action":{"m":2}}],"criteria":[{"name":"q","type":"predicate","instructions":"ok?"}]}`
	if rec := do(h, "POST", "/v1/decisions/rollouts", key, roll); rec.Code != 400 {
		t.Fatalf("rollouts with needs: %d %s", rec.Code, rec.Body)
	}
}
