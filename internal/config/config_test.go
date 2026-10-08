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

	reEnv := regexp.MustCompile(`get(?:Bool|Int|Duration|Retention)?Env\("([A-Z0-9_]+)"`)
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
