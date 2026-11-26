package server

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

// sseData returns the data of each event of a stream, in order.
func sseData(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(body, "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var m map[string]any
			if err := json.Unmarshal([]byte(data), &m); err != nil {
				t.Fatalf("event data %q: %v", data, err)
			}
			out = append(out, m)
		}
	}
	return out
}

func TestResponsesMapsRequest(t *testing.T) {
	fp, h, key := newTestAPI(t)
	png := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n"))
	rec := do(h, "POST", "/v1/responses", key, `{
	  "model":"auto","instructions":"You are Codex.","reasoning":{"effort":"xhigh","summary":"auto"},
	  "max_output_tokens":2000,"temperature":0.3,"user":"u1","metadata":{"action_id":"edit"},
	  "store":false,"stream":false,"parallel_tool_calls":true,"include":["reasoning.encrypted_content"],
	  "prompt_cache_key":"abc","text":{"verbosity":"low","format":{"type":"json_schema","name":"r","schema":{"type":"object","properties":{"ok":{"type":"boolean"}}}}},
	  "tools":[
	    {"type":"function","name":"shell","description":"Run a command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}},"strict":false},
	    {"type":"custom","name":"apply_patch","description":"Edit files","format":{"type":"grammar","syntax":"lark","definition":"start: patch"}},
	    {"type":"web_search","external_web_access":false}],
	  "tool_choice":"auto",
	  "input":[
	    {"type":"message","role":"developer","content":[{"type":"input_text","text":"Sandbox: read-only."}]},
	    {"role":"user","content":[{"type":"input_text","text":"list files"},{"type":"input_image","image_url":"`+png+`"}]},
	    {"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"xxx"},
	    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"Listing."}]},
	    {"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"},
	    {"type":"custom_tool_call","call_id":"call_2","name":"apply_patch","input":"*** Begin Patch"},
	    {"type":"function_call_output","call_id":"call_1","output":[{"type":"input_text","text":"a.go"},{"type":"input_image","image_url":"`+png+`"}]},
	    {"type":"custom_tool_call_output","call_id":"call_2","output":"Done!"},
	    {"type":"message","role":"user","content":"thanks"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	r := fp.text
	if !r.AutoRoute || r.Effort != pb.Effort_EFFORT_HIGH || !r.EffortSpecified || r.MaxOutputTokens != 2000 || r.GetTemperature() != 0.3 {
		t.Errorf("routing: %+v", r)
	}
	if r.BaseSystemInstruction != "You are Codex.\n\nSandbox: read-only." || r.Attributes["user_id"] != "u1" || r.ActionId != "edit" {
		t.Errorf("system %q attrs %v action %q", r.BaseSystemInstruction, r.Attributes, r.ActionId)
	}
	if r.ResponseSchema.GetProperties()["ok"].GetType() != "boolean" {
		t.Errorf("schema: %v", r.ResponseSchema)
	}
	if len(r.Tools) != 2 || r.Tools[0].Function.Name != "shell" || r.Tools[1].Function.Name != "apply_patch" ||
		r.Tools[1].Function.Parameters.Properties["input"].Type != "string" ||
		!strings.Contains(r.Tools[1].Function.Description, "lark grammar:\nstart: patch") || r.ToolChoice.GetMode() != pb.ToolChoice_MODE_AUTO {
		t.Errorf("tools %v choice %v", r.Tools, r.ToolChoice)
	}
	if len(r.Messages) != 6 {
		t.Fatalf("messages: %v", r.Messages)
	}
	user, asst, out1, out2, media, last := r.Messages[0], r.Messages[1], r.Messages[2], r.Messages[3], r.Messages[4], r.Messages[5]
	if user.Text != "list files" || len(user.Parts) != 2 || user.Parts[1].GetImage().GetMimeType() != "image/png" {
		t.Errorf("user: %v", user)
	}
	// Both calls join the assistant message before them.
	if asst.Text != "Listing." || len(asst.ToolCalls) != 2 || asst.ToolCalls[0].Id != "call_1" ||
		asst.ToolCalls[1].Function.Arguments != `{"input":"*** Begin Patch"}` {
		t.Errorf("assistant: %v", asst)
	}
	if out1.Role != pb.MessageRole_MESSAGE_ROLE_TOOL || out1.ToolCallId != "call_1" || out1.Text != "a.go" ||
		out2.ToolCallId != "call_2" || out2.Text != "Done!" {
		t.Errorf("outputs: %v %v", out1, out2)
	}
	if media.Role != pb.MessageRole_MESSAGE_ROLE_USER || len(media.Parts) != 1 || media.Parts[0].GetImage() == nil || last.Text != "thanks" {
		t.Errorf("media %v last %v", media, last)
	}
	want := "include,prompt_cache_key,reasoning.summary,text.verbosity,tools.web_search"
	if got := rec.Header().Get("X-KTMS-Ignored-Params"); got != want {
		t.Errorf("ignored %q, want %q", got, want)
	}
}

func TestResponsesStringInput(t *testing.T) {
	fp, h, key := newTestAPI(t)
	rec := do(h, "POST", "/v1/responses", key, `{"model":"gpt-x","input":"hi","reasoning":{"effort":"minimal"}}`)
	if rec.Code != 200 {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	if r := fp.text; r.ModelOverride != "gpt-x" || r.Effort != pb.Effort_EFFORT_LOW || len(r.Messages) != 1 || r.Messages[0].Text != "hi" {
		t.Errorf("request: %+v", r)
	}
	if rec.Header().Get("X-KTMS-Ignored-Params") != "" {
		t.Errorf("ignored: %q", rec.Header().Get("X-KTMS-Ignored-Params"))
	}
}

func TestResponsesResponse(t *testing.T) {
	fp, h, key := newTestAPI(t)
	fp.respDetails = []*pb.Choice{{
		Text: "Patching.",
		ToolCalls: []*pb.ToolCall{
			{Id: "call_9", Function: &pb.FunctionCall{Name: "shell", Arguments: `{"cmd":"ls"}`}},
			{Function: &pb.FunctionCall{Name: "apply_patch", Arguments: `{"input":"*** Begin Patch\n"}`}},
			{Function: &pb.FunctionCall{Name: "noargs"}},
		},
		FinishReason: "tool_calls",
	}}
	rec := do(h, "POST", "/v1/responses", key, `{"model":"m1","input":"go","tools":[{"type":"custom","name":"apply_patch"}],"metadata":{"k":"v"}}`)
	if rec.Code != 200 {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		ID         string           `json:"id"`
		Object     string           `json:"object"`
		Status     string           `json:"status"`
		Model      string           `json:"model"`
		Output     []map[string]any `json:"output"`
		Usage      map[string]any   `json:"usage"`
		Store      bool             `json:"store"`
		Metadata   map[string]any   `json:"metadata"`
		Incomplete any              `json:"incomplete_details"`
		Tools      []any            `json:"tools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.ID, "resp_") || out.Object != "response" || out.Status != "completed" || out.Model != "m1" ||
		out.Store || out.Metadata["k"] != "v" || out.Incomplete != nil || len(out.Tools) != 1 || len(out.Output) != 4 {
		t.Fatalf("response: %s", rec.Body)
	}
	msg, fc, ctc, empty := out.Output[0], out.Output[1], out.Output[2], out.Output[3]
	if msg["type"] != "message" || msg["role"] != "assistant" || msg["status"] != "completed" ||
		msg["content"].([]any)[0].(map[string]any)["text"] != "Patching." {
		t.Errorf("message: %v", msg)
	}
	if fc["type"] != "function_call" || fc["call_id"] != "call_9" || fc["arguments"] != `{"cmd":"ls"}` || !strings.HasPrefix(fc["id"].(string), "fc_") {
		t.Errorf("function call: %v", fc)
	}
	if ctc["type"] != "custom_tool_call" || ctc["input"] != "*** Begin Patch\n" || !strings.HasPrefix(ctc["call_id"].(string), "call_") {
		t.Errorf("custom call: %v", ctc)
	}
	if empty["arguments"] != "{}" {
		t.Errorf("no arguments: %v", empty)
	}
	if out.Usage["input_tokens"] != 3.0 || out.Usage["output_tokens"] != 2.0 || out.Usage["total_tokens"] != 5.0 {
		t.Errorf("usage: %v", out.Usage)
	}
	if rec.Header().Get("X-KTMS-Vendor") != "openai" || rec.Header().Get("X-KTMS-Rule") != "default" {
		t.Errorf("headers: %v", rec.Header())
	}
}

func TestResponsesIncomplete(t *testing.T) {
	fp, h, key := newTestAPI(t)
	fp.respDetails = []*pb.Choice{{Text: "cut", FinishReason: "length"}}
	rec := do(h, "POST", "/v1/responses", key, `{"input":"go","max_output_tokens":5}`)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["status"] != "incomplete" || out["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" || out["max_output_tokens"] != 5.0 {
		t.Errorf("response: %s", rec.Body)
	}
}

func TestResponsesErrors(t *testing.T) {
	fp, h, key := newTestAPI(t)
	for _, c := range []struct{ body, want string }{
		{`{"input":"x","previous_response_id":"resp_1"}`, "previous_response_id"},
		{`{"input":"x","conversation":"conv_1"}`, "conversation"},
		{`{"input":"x","background":true}`, "background"},
		{`{"input":"x","prompt":{"id":"pmpt_1"}}`, "prompt"},
		{`{"input":[{"type":"item_reference","id":"msg_1"}]}`, "input[0]: item_reference"},
		{`{"input":[{"type":"local_shell_call","call_id":"c"}]}`, `input[0]: item type "local_shell_call"`},
		{`{"input":[{"role":"user","content":[{"type":"input_image","file_id":"file_1"}]}]}`, "input[0]: input_image needs image_url"},
		{`{"input":[{"role":"user","content":[{"type":"input_file","file_url":"https://x/a.pdf"}]}]}`, "input[0]: input_file needs file_data"},
		{`{"input":[{"role":"tool","content":"x"}]}`, `role "tool"`},
		{`{"input":"x","tool_choice":{"type":"web_search"}}`, `tool_choice.type "web_search"`},
		{`{"input":"x","text":{"format":{"type":"json_schema"}}}`, "text.format.schema"},
		{`{"input":"x","tools":[{"name":"f"}]}`, "tools[0].type"},
		{`{"input":[]}`, "input: at least one message"},
		{`{"model":"auto"}`, "input is required"},
		{`{"input":"x","reasoning":{"effort":"max"}}`, "unknown effort"},
	} {
		rec := do(h, "POST", "/v1/responses", key, c.body)
		if e := errBody(t, rec); rec.Code != 400 || !strings.Contains(e["message"].(string), c.want) {
			t.Errorf("%s: %d %v", c.body, rec.Code, e)
		}
	}

	rec := do(h, "GET", "/v1/responses/resp_1", key, "")
	if e := errBody(t, rec); rec.Code != 404 || !strings.Contains(e["message"].(string), "not stored") {
		t.Errorf("get: %d %v", rec.Code, e)
	}
	if rec := do(h, "POST", "/v1/responses", "", `{"input":"x"}`); rec.Code != 401 {
		t.Errorf("no key: %d", rec.Code)
	}
	fp.textErr = status.Error(codes.ResourceExhausted, "throttled:rpm")
	rec = do(h, "POST", "/v1/responses", key, `{"input":"x"}`)
	if e := errBody(t, rec); rec.Code != 429 || e["code"] != "throttled:rpm" || rec.Header().Get("Retry-After") == "" {
		t.Errorf("throttled: %d %v", rec.Code, e)
	}
}

func TestResponsesStream(t *testing.T) {
	fp, h, key := newTestAPI(t)
	fp.streamChunks = []*pb.GenerateTextChunk{
		{Delta: "Checking", ResolvedModel: "m1", ResolvedVendor: "openai", MatchedRule: "default", IgnoredParameters: []string{"temperature"}},
		{Delta: " now."},
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 0, Id: "call_a", Name: "shell", ArgumentsDelta: `{"cmd":`}}},
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 0, ArgumentsDelta: `"ls"}`}}},
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 1, Name: "apply_", ArgumentsDelta: ""}}},
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 1, Name: "patch", ArgumentsDelta: `{"input":"*** Begin`}}},
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 1, ArgumentsDelta: ` Patch"}`}}},
		{FinishReason: "tool_calls", Usage: &pb.TokensUsage{Input: 50, Output: 9, CacheRead: 40}},
	}
	rec := do(h, "POST", "/v1/responses", key, `{"model":"auto","stream":true,"input":"x","temperature":0.5,"prompt_cache_key":"k",
	  "tools":[{"type":"function","name":"shell"},{"type":"custom","name":"apply_patch"}]}`)
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" || rec.Header().Get("X-KTMS-Vendor") != "openai" ||
		rec.Header().Get("X-KTMS-Ignored-Params") != "temperature,prompt_cache_key" {
		t.Fatalf("headers %v: %s", rec.Header(), rec.Body)
	}
	b := rec.Body.String()
	want := []string{"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.output_item.added", "response.function_call_arguments.delta", "response.function_call_arguments.delta",
		"response.function_call_arguments.done", "response.output_item.done",
		"response.output_item.added", "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done", "response.output_item.done",
		"response.completed"}
	if got := sseEvents(b); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events %v\n%s", got, b)
	}
	events := sseData(t, b)
	for i, e := range events {
		if e["sequence_number"] != float64(i) || e["type"] != want[i] {
			t.Fatalf("event %d: %v", i, e)
		}
	}
	if item := events[2]["item"].(map[string]any); item["type"] != "message" || item["status"] != "in_progress" {
		t.Errorf("message added: %v", item)
	}
	if item := events[9]["item"].(map[string]any); item["name"] != "shell" || item["call_id"] != "call_a" || item["arguments"] != "" {
		t.Errorf("function added: %v", item)
	}
	if item := events[14]["item"].(map[string]any); item["type"] != "custom_tool_call" || item["name"] != "apply_patch" {
		t.Errorf("custom added: %v", item)
	}
	if events[15]["delta"] != "*** Begin Patch" {
		t.Errorf("custom input: %v", events[15])
	}
	resp := events[len(events)-1]["response"].(map[string]any)
	output := resp["output"].([]any)
	if resp["status"] != "completed" || resp["model"] != "m1" || len(output) != 3 {
		t.Fatalf("completed: %v", resp)
	}
	if text := output[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; text != "Checking now." {
		t.Errorf("text: %v", text)
	}
	if fc := output[1].(map[string]any); fc["arguments"] != `{"cmd":"ls"}` || fc["status"] != "completed" {
		t.Errorf("function: %v", fc)
	}
	if ctc := output[2].(map[string]any); ctc["input"] != "*** Begin Patch" {
		t.Errorf("custom: %v", ctc)
	}
	if u := resp["usage"].(map[string]any); u["input_tokens"] != 50.0 || u["input_tokens_details"].(map[string]any)["cached_tokens"] != 40.0 {
		t.Errorf("usage: %v", u)
	}
}

func TestResponsesStreamToolWithoutArguments(t *testing.T) {
	fp, h, key := newTestAPI(t)
	fp.streamChunks = []*pb.GenerateTextChunk{
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 0, Id: "c1", Name: "list", ArgumentsDelta: "null"}}},
		{FinishReason: "tool_calls"},
	}
	rec := do(h, "POST", "/v1/responses", key, `{"stream":true,"input":"x"}`)
	events := sseData(t, rec.Body.String())
	done := events[len(events)-2]
	if done["type"] != "response.output_item.done" || done["item"].(map[string]any)["arguments"] != "{}" {
		t.Errorf("events: %s", rec.Body)
	}
}

func TestResponsesStreamErrors(t *testing.T) {
	fp, h, key := newTestAPI(t)
	fp.textErr = status.Error(codes.Unavailable, "no_capable_model")
	rec := do(h, "POST", "/v1/responses", key, `{"stream":true,"input":"x"}`)
	if e := errBody(t, rec); rec.Code != 503 || e["code"] != "no_capable_model" {
		t.Errorf("before the stream: %d %v", rec.Code, e)
	}

	fp.textErr = nil
	fp.emptyStream = true
	rec = do(h, "POST", "/v1/responses", key, `{"stream":true,"input":"x"}`)
	if got := strings.Join(sseEvents(rec.Body.String()), ","); got != "response.created,response.in_progress,response.completed" {
		t.Errorf("no chunks: %s", got)
	}

	fp.emptyStream = false
	fp.streamChunks = []*pb.GenerateTextChunk{{Delta: "par", ResolvedModel: "m1"}}
	fp.streamErr = status.Error(codes.Unavailable, "vendor_error")
	rec = do(h, "POST", "/v1/responses", key, `{"stream":true,"input":"x"}`)
	events := sseData(t, rec.Body.String())
	last := events[len(events)-1]
	resp := last["response"].(map[string]any)
	if last["type"] != "response.failed" || resp["status"] != "failed" || resp["error"].(map[string]any)["code"] != "vendor_error" {
		t.Errorf("mid-stream: %s", rec.Body)
	}

	fp.streamErr = nil
	fp.streamChunks = []*pb.GenerateTextChunk{
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 0, Id: "a", Name: "Read"}}},
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 1, Id: "b", Name: "Glob"}}},
		{ToolCallChunks: []*pb.ToolCallChunk{{Index: 0, ArgumentsDelta: "{}"}}},
	}
	rec = do(h, "POST", "/v1/responses", key, `{"stream":true,"input":"x"}`)
	if ev := sseEvents(rec.Body.String()); ev[len(ev)-1] != "response.failed" {
		t.Errorf("out of order: %v", ev)
	}
}
