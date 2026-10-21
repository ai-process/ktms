package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ai-process/ktms/internal/llm"
	"github.com/ai-process/ktms/internal/proxydb"
)

// The router owns retries and fallback; an SDK retrying underneath turns one
// attempt into several vendor calls and stretches the per-attempt deadline.
func TestVendorSDKsDoNotRetry(t *testing.T) {
	cases := []struct {
		vendor string
		key    string
	}{
		{VendorOpenAI, "sk-test"},
		{VendorGroq, "gsk-test"},
		{VendorAnthropic, "sk-ant-test"},
		{VendorBedrock, "AKIDTEST:secret:us-east-1"},
	}
	for _, tc := range cases {
		t.Run(tc.vendor, func(t *testing.T) {
			var hits atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"message":"boom","type":"server_error"}}`))
			}))
			defer srv.Close()

			adapter, err := buildAdapter(&proxydb.Model{ID: "m", Vendor: tc.vendor, Endpoint: srv.URL}, tc.key)
			if err != nil {
				t.Fatalf("buildAdapter: %v", err)
			}
			chat := llm.NewChatContext()
			chat.AddUserMessage("hi")
			if _, err := adapter.GenerateText(context.Background(), chat); err == nil {
				t.Fatal("expected the 500 to surface")
			}
			if n := hits.Load(); n != 1 {
				t.Fatalf("one GenerateText made %d HTTP calls, want 1", n)
			}
		})
	}
}

func TestVendorKeyFingerprint(t *testing.T) {
	snap := &Snapshot{vendorKeys: credsFrom(map[string]map[string]string{
		"a": {VendorOpenAI: "sk-shared"},
		"b": {VendorOpenAI: "sk-shared"},
		"c": {VendorOpenAI: "sk-other"},
	})}
	fa, fb, fc := snap.VendorKeyFingerprint("a", VendorOpenAI), snap.VendorKeyFingerprint("b", VendorOpenAI), snap.VendorKeyFingerprint("c", VendorOpenAI)
	if fa == "" || fa != fb {
		t.Fatalf("clients sharing a vendor key must share a fingerprint: %q %q", fa, fb)
	}
	if fa == fc {
		t.Fatal("different vendor keys share a fingerprint")
	}
	if fa == "sk-shared" || len(fa) != 16 {
		t.Fatalf("fingerprint %q must not reveal the key", fa)
	}
	if got := snap.VendorKeyFingerprint("missing", VendorOpenAI); got != "" {
		t.Fatalf("unknown client fingerprint = %q", got)
	}
}
