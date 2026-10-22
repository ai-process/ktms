package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestEnvVarDocumentationConsistency(t *testing.T) {
	rootDir := filepath.Join("..", "..")

	configSrc, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}

	envExample, err := os.ReadFile(filepath.Join(rootDir, "env.example"))
	if err != nil {
		t.Fatalf("read env.example: %v", err)
	}

	readme, err := os.ReadFile(filepath.Join(rootDir, "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}

	reEnv := regexp.MustCompile(`get(?:Bool|Int|Float|Duration|Retention)?Env\("([A-Z0-9_]+)"`)
	matches := reEnv.FindAllStringSubmatch(string(configSrc), -1)

	var foundVars []string
	seen := make(map[string]bool)
	for _, m := range matches {
		name := m[1]
		if !seen[name] {
			seen[name] = true
			foundVars = append(foundVars, name)
		}
	}

	if len(foundVars) == 0 {
		t.Fatal("no environment variables found in config.go")
	}

	envExampleStr := string(envExample)
	readmeStr := string(readme)

	for _, v := range foundVars {
		if !strings.Contains(envExampleStr, v+"=") {
			t.Errorf("env var %q used in config.go is missing from env.example", v)
		}
		if !strings.Contains(readmeStr, "`"+v+"`") {
			t.Errorf("env var %q used in config.go is missing from README.md", v)
		}
	}
}

func TestLegacyEncryptionKeysWarns(t *testing.T) {
	t.Setenv("USAGE_COLLECTOR_HOST", "") // keeps the usage-project notice out
	t.Setenv("KTMS_ENCRYPTION_KEYS", "")
	t.Setenv("LLMPROXY_ENCRYPTION_KEYS", "1:abc")
	c := Load()
	if c.EncryptionKeys != "1:abc" || len(c.Notices) != 1 {
		t.Fatalf("keys %q, deprecations %v: the old name must still work and warn", c.EncryptionKeys, c.Notices)
	}
	t.Setenv("KTMS_ENCRYPTION_KEYS", "1:def")
	if c := Load(); c.EncryptionKeys != "1:def" || len(c.Notices) != 0 {
		t.Fatalf("keys %q, deprecations %v: the new name wins without a warning", c.EncryptionKeys, c.Notices)
	}
}

func TestUsageProjectNoticeOnlyWhenReportingWithDefault(t *testing.T) {
	t.Setenv("KTMS_ENCRYPTION_KEYS", "1:x")
	t.Setenv("USAGE_COLLECTOR_HOST", "tm:443")
	t.Setenv("USAGE_PROJECT_ID", "")
	if c := Load(); c.UsageProjectID != "ktms" || len(c.Notices) != 1 {
		t.Fatalf("project %q, notices %v: want default ktms and one notice", c.UsageProjectID, c.Notices)
	}
	t.Setenv("USAGE_PROJECT_ID", "llm-proxy")
	if c := Load(); len(c.Notices) != 0 {
		t.Fatalf("notices %v: an explicit project needs no notice", c.Notices)
	}
}
