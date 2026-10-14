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
