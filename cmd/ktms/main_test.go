package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func call(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

func TestValidateAndSchema(t *testing.T) {
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
	if code, _, _ := call("nonsense"); code != 2 {
		t.Fatalf("unknown command = %d, want 2", code)
	}
}

func TestPlanNeedsAnAdminKey(t *testing.T) {
	t.Setenv("KTMS_ADMIN_KEY", "")
	if code, _, errs := call("config", "plan", "-f", "../../schema/example.yaml"); code != 1 || !strings.Contains(errs, "KTMS_ADMIN_KEY") {
		t.Fatalf("plan without a key = %d %q", code, errs)
	}
}

func TestPlaintextWarning(t *testing.T) {
	t.Setenv("KTMS_ADMIN_KEY", "")
	if _, _, errs := call("config", "plan", "-f", "../../schema/example.yaml", "--addr", "keystone.example.com:9090"); !strings.Contains(errs, "without TLS") {
		t.Fatalf("no warning for a remote plaintext address: %q", errs)
	}
	if _, _, errs := call("config", "plan", "-f", "../../schema/example.yaml", "--addr", "127.0.0.1:9090"); strings.Contains(errs, "without TLS") {
		t.Fatalf("warning for loopback: %q", errs)
	}
}
