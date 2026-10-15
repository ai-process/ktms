package nodeagent

import (
	"testing"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
)

func TestDialCredentials(t *testing.T) {
	for _, tc := range []struct {
		tls  bool
		want string
	}{
		{tls: false, want: "insecure"},
		{tls: true, want: "tls"},
	} {
		got := NewAgent(AgentConfig{TLS: tc.tls}).dialCredentials().Info().SecurityProtocol
		if got != tc.want {
			t.Errorf("TLS=%v: security protocol = %q, want %q", tc.tls, got, tc.want)
		}
	}
}

func TestResolveURLs(t *testing.T) {
	for _, c := range []struct{ engine, endpoint, poll, report string }{
		{"", "", DefaultEngineURL, DefaultEngineURL},
		{"http://127.0.0.1:8000", "", "http://127.0.0.1:8000", "http://127.0.0.1:8000"},
		{"http://127.0.0.1:11434", "https://x.trycloudflare.com", "http://127.0.0.1:11434", "https://x.trycloudflare.com"},
		{"", "https://gpu-box:8000", "https://gpu-box:8000", "https://gpu-box:8000"},
	} {
		poll, report := ResolveURLs(c.engine, c.endpoint)
		if poll != c.poll || report != c.report {
			t.Errorf("ResolveURLs(%q, %q) = %q, %q; want %q, %q", c.engine, c.endpoint, poll, report, c.poll, c.report)
		}
	}
}

func TestFilterModels(t *testing.T) {
	all := []*pb.NodeModelSpec{
		{ModelId: "tiny:1b", RemoteModelName: "tiny:1b"},
		{ModelId: "gemma4:26b", RemoteModelName: "gemma4:26b"},
		{ModelId: "llama3:latest", RemoteModelName: "llama3:latest"},
	}
	ids := func(ms []*pb.NodeModelSpec) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.GetModelId())
		}
		return out
	}
	for _, c := range []struct {
		name  string
		allow []string
		want  []string
	}{
		{"no filter keeps all", nil, []string{"tiny:1b", "gemma4:26b", "llama3:latest"}},
		{"one model", []string{"tiny:1b"}, []string{"tiny:1b"}},
		{"repeatable", []string{"tiny:1b", "gemma4:26b"}, []string{"tiny:1b", "gemma4:26b"}},
		{"latest suffix is implied", []string{"llama3"}, []string{"llama3:latest"}},
		{"absent model reports nothing", []string{"missing:7b"}, nil},
	} {
		got := ids(FilterModels(all, c.allow))
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			}
		}
	}
}

func TestTagJudgeModels(t *testing.T) {
	models := []*pb.NodeModelSpec{
		{ModelId: "gemma4:26b", RemoteModelName: "gemma4:26b"},
		{ModelId: "jepa-fast", RemoteModelName: "jepa-fast"},
	}
	tagged := TagJudgeModels(models, []string{"jepa-fast"})
	if len(tagged[0].Capabilities) != 0 {
		t.Errorf("gemma should have no capabilities, got %v", tagged[0].Capabilities)
	}
	if len(tagged[1].Capabilities) != 1 || tagged[1].Capabilities[0] != "judge" {
		t.Errorf("jepa-fast should have [judge] capability, got %v", tagged[1].Capabilities)
	}
}

