package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func call(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

// isolate points the CLI at an empty contexts file and clears the environment.
func isolate(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("KTMS_CONFIG", p)
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(p))
	for _, k := range []string{"KTMS_ADDR", "KTMS_TLS", "KTMS_HTTP", "KTMS_ADMIN_KEY", "KTMS_API_KEY", "KTMS_CONTEXT"} {
		t.Setenv(k, "")
	}
	return p
}

func TestValidateAndSchema(t *testing.T) {
	isolate(t)
	if code, out, _ := call("config", "validate", "-f", "../../schema/example.yaml"); code != 0 || !strings.Contains(out, "valid") {
		t.Fatalf("validate example = %d %q", code, out)
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("api_version: nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := call("config", "validate", "-f", bad); code != 1 || !strings.Contains(errs, "api_version must be") {
		t.Fatalf("validate bad = %d %q", code, errs)
	}
	if code, out, _ := call("config", "schema"); code != 0 || !strings.Contains(out, `"$schema"`) {
		t.Fatalf("schema = %d", code)
	}
	for _, args := range [][]string{{"nonsense"}, {"keys", "list", "--bogus"}, {"keys", "mint"}, {"models", "list", "-o", "xml"},
		{"keys"}, {"keys", "revok", "bot"}, {"config", "bogus"}, {"keys", "policy"}, {"limits", "set", "key", "a", "day", "NaN"},
		{"node-keys", "mint", "gpu"}} {
		if code, _, _ := call(args...); code != exitUsage {
			t.Errorf("ktms %v = %d, want %d", args, code, exitUsage)
		}
	}
}

func TestCommandsWithoutKeystone(t *testing.T) {
	// A broken contexts file must not stop what never connects.
	p := isolate(t)
	if err := os.WriteFile(p, []byte("{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"version"}, {"completion", "zsh"}, {"config", "schema"}, {"--help"}} {
		if code, _, errs := call(args...); code != 0 {
			t.Errorf("ktms %v = %d: %s", args, code, errs)
		}
	}
	if code, out, _ := call("completion", "bash"); code != 0 || !strings.Contains(out, "ktms") {
		t.Fatalf("completion bash = %d", code)
	}
}

func TestPlanNeedsAnAdminKey(t *testing.T) {
	isolate(t)
	if code, _, errs := call("config", "plan", "-f", "../../schema/example.yaml"); code != exitAuth || !strings.Contains(errs, "KTMS_ADMIN_KEY") {
		t.Fatalf("plan without a key = %d %q", code, errs)
	}
}

func TestPlaintextWarning(t *testing.T) {
	isolate(t)
	t.Setenv("KTMS_ADMIN_KEY", "ktms_x")
	if _, _, errs := call("models", "list", "--timeout", "1ms", "--addr", "keystone.example.com:9090"); !strings.Contains(errs, "without TLS") {
		t.Fatalf("no warning for a remote plaintext address: %q", errs)
	}
	if _, _, errs := call("models", "list", "--timeout", "1ms", "--addr", "127.0.0.1:1"); strings.Contains(errs, "without TLS") {
		t.Fatalf("warning for loopback: %q", errs)
	}
}

func TestContexts(t *testing.T) {
	p := isolate(t)
	keyFile := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(keyFile, []byte("ktms_from_file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := call("context", "set", "prod", "--addr", "ktms.example.com:443", "--tls", "--key", "file:"+keyFile); code != 0 {
		t.Fatalf("context set = %d %s", code, errs)
	}
	if code, _, errs := call("context", "set", "local", "--addr", "localhost:9090", "--key", "env:LOCAL_KEY"); code != 0 {
		t.Fatalf("context set local = %d %s", code, errs)
	}
	if code, _, _ := call("context", "set", "x", "--key", "vault:y"); code != exitUsage {
		t.Fatalf("bad key source = %d", code)
	}
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("contexts file: %v %v", info.Mode(), err)
	}
	// The first context became current.
	_, out, _ := call("context", "list")
	if !strings.Contains(out, "*  prod") || !strings.Contains(out, "file:"+keyFile) {
		t.Fatalf("context list:\n%s", out)
	}

	tg := target{}
	if err := tg.resolve(); err != nil {
		t.Fatal(err)
	}
	if tg.addr != "ktms.example.com:443" || !tg.tls || tg.http != "https://ktms.example.com:8080" {
		t.Fatalf("resolved %+v", &tg)
	}
	if k, err := tg.adminKey(); err != nil || k != "ktms_from_file" {
		t.Fatalf("admin key = %q %v", k, err)
	}

	// Flags beat the environment, which beats the current context.
	t.Setenv("KTMS_ADDR", "env.example.com:9090")
	t.Setenv("KTMS_ADMIN_KEY", "ktms_env")
	tg = target{}
	if err := tg.resolve(); err != nil {
		t.Fatal(err)
	}
	if k, _ := tg.adminKey(); tg.addr != "env.example.com:9090" || k != "ktms_env" {
		t.Fatalf("env over the current context: %s %q", tg.addr, k)
	}
	tg = target{context: "local", addr: "flag.example.com:9090"}
	if err := tg.resolve(); err != nil {
		t.Fatal(err)
	}
	if tg.addr != "flag.example.com:9090" || tg.tls {
		t.Fatalf("flags: %+v", &tg)
	}
	if _, err := tg.adminKey(); err == nil || !strings.Contains(err.Error(), "LOCAL_KEY") {
		t.Fatalf("the named context's key source is not used: %v", err)
	}
	t.Setenv("KTMS_ADDR", "")

	// A context asked for by name beats the shell's address and key.
	t.Setenv("KTMS_ADMIN_KEY", "ktms_env")
	tg = target{context: "prod"}
	if k, _ := tg.adminKey(); k != "ktms_from_file" || tg.addr != "ktms.example.com:443" {
		t.Fatalf("named context: addr %s key %q", tg.addr, k)
	}
	t.Setenv("KTMS_ADMIN_KEY", "")
	t.Setenv("KTMS_TLS", "yes please")
	if code, _, _ := call("models", "list"); code != exitUsage {
		t.Fatalf("bad KTMS_TLS = %d, want %d", code, exitUsage)
	}
	t.Setenv("KTMS_TLS", "")

	if code, _, _ := call("context", "use", "nope"); code == 0 {
		t.Fatal("use of an unknown context succeeded")
	}
	if code, _, _ := call("context", "use", "local"); code != 0 {
		t.Fatal("use local failed")
	}
	var out2, errb bytes.Buffer
	if code := run([]string{"context", "store-key", "local", "--file"}, strings.NewReader("ktms_stored\n"), &out2, &errb); code != 0 {
		t.Fatalf("store-key = %d %s", code, errb.String())
	}
	stored := filepath.Join(filepath.Dir(p), "ktms", "local.key")
	if b, err := os.ReadFile(stored); err != nil || strings.TrimSpace(string(b)) != "ktms_stored" {
		t.Fatalf("stored key %q %v", b, err)
	}
	tg = target{}
	_ = tg.resolve()
	if k, err := tg.adminKey(); err != nil || k != "ktms_stored" {
		t.Fatalf("key after store-key = %q %v", k, err)
	}
	if code, _, _ := call("context", "delete", "local"); code != 0 {
		t.Fatal("delete failed")
	}
	if _, out, _ := call("context", "list", "-o", "json"); strings.Contains(out, `"local"`) || !strings.Contains(out, `"prod"`) {
		t.Fatalf("after delete: %s", out)
	}
}

func TestChatAndHealth(t *testing.T) {
	isolate(t)
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_, _ = w.Write([]byte(`{"status":"ok","version":"1.2.3"}`))
		case "/readyz":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"not ready","error":"database unavailable"}`))
		case "/v1/chat/completions":
			if r.Header.Get("Authorization") != "Bearer ktms_client" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
				return
			}
			_ = jsonDecode(r, &got)
			w.Header().Set("X-KTMS-Rule", "default")
			w.Header().Set("X-KTMS-Model", "gpt-4o-mini")
			w.Header().Set("X-KTMS-Vendor", "openai")
			_, _ = w.Write([]byte(`{"model":"gpt-4o-mini","choices":[{"message":{"content":"hi there"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
		}
	}))
	defer srv.Close()

	code, out, errs := call("health", "--http", srv.URL)
	if code != exitRetry || !strings.Contains(out, "version 1.2.3") || !strings.Contains(out, "503") {
		t.Fatalf("health = %d\n%s%s", code, out, errs)
	}

	t.Setenv("KTMS_API_KEY", "ktms_client")
	code, out, errs = call("chat", "--http", srv.URL, "--effort", "high", "--attr", "tier=pro", "say hi")
	if code != 0 || out != "hi there\n" || !strings.Contains(errs, "rule: default") || !strings.Contains(errs, "vendor: openai") {
		t.Fatalf("chat = %d %q %q", code, out, errs)
	}
	if got["reasoning_effort"] != "high" || got["model"] != "auto" || got["metadata"].(map[string]any)["tier"] != "pro" {
		t.Fatalf("request body = %v", got)
	}
	if code, out, _ := call("chat", "--http", srv.URL, "-o", "json", "say hi"); code != 0 || !strings.Contains(out, `"rule": "default"`) {
		t.Fatalf("chat -o json = %d %s", code, out)
	}
	t.Setenv("KTMS_API_KEY", "ktms_wrong")
	if code, _, errs := call("chat", "--http", srv.URL, "say hi"); code != exitAuth || !strings.Contains(errs, "invalid api key") {
		t.Fatalf("chat with a wrong key = %d %q", code, errs)
	}
}

func TestExitCodes(t *testing.T) {
	cases := map[error]int{
		status.Error(codes.Unauthenticated, ""):    exitAuth,
		status.Error(codes.PermissionDenied, ""):   exitAuth,
		status.Error(codes.NotFound, ""):           exitNotFound,
		status.Error(codes.InvalidArgument, ""):    exitRejected,
		status.Error(codes.FailedPrecondition, ""): exitRejected,
		status.Error(codes.Unavailable, ""):        exitRetry,
		status.Error(codes.ResourceExhausted, ""):  exitRetry,
		status.Error(codes.Internal, ""):           exitFailed,
		httpError{status: 429}:                     exitRetry,
		httpError{status: 400}:                     exitRejected,
		httpError{status: 403}:                     exitAuth,
		errors.New("x"):                            exitFailed,
	}
	for err, want := range cases {
		if got := exitCode(err); got != want {
			t.Errorf("exitCode(%v) = %d, want %d", err, got, want)
		}
	}
}

func jsonDecode(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func TestNoHomeAndUnreachableHTTP(t *testing.T) {
	isolate(t)
	t.Setenv("KTMS_CONFIG", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("KTMS_API_KEY", "ktms_x")
	// Without a home directory the environment still names the target.
	code, _, errs := call("chat", "--http", "http://127.0.0.1:1", "hi")
	if code != exitRetry || strings.Contains(errs, "HOME") {
		t.Fatalf("chat to a closed port = %d %q, want %d", code, errs, exitRetry)
	}
}
