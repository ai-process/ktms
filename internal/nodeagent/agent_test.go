package nodeagent

import "testing"

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
