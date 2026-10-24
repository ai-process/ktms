package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// tokenmeterTools are light-tokenmeter's MCP tools, which live in its own
// repository; TestSkills fails on a tool name that is in neither server.
var tokenmeterTools = []string{"usage_summary", "top_spenders", "cost_per_action", "usage_by_meta", "health"}

var (
	codeSpan = regexp.MustCompile("`([^`\n]+)`")
	mdLink   = regexp.MustCompile(`\]\((https://github\.com/ai-process/(ktms|light-tokenmeter)/blob/main/README\.md)#([^)]+)\)`)
	// A tool name: a verb these servers use, an underscore, more words; amounts
	// such as cost_usd are fields, not tools.
	toolName = regexp.MustCompile(`^(list|get|config|mint|revoke|set|delete|drain|explain|usage|top|cost)_[a-z_]{4,}$`)
)

// TestSkills keeps the plugin's skills true to this version: every command
// and flag they show exists, every MCP tool they name is served, every README
// link has its section. light-tokenmeter's commands and README are checked
// when TOKENMETER_BIN and TOKENMETER_README are set, as the skills CI job does.
func TestSkills(t *testing.T) {
	dirs, err := filepath.Glob("../../plugin/skills/*/SKILL.md")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no skills found: %v", err)
	}
	ktmsTools := mcpToolNames(t)
	anchors := map[string]map[string]bool{"ktms": readmeAnchors(t, "../../README.md")}
	if p := os.Getenv("TOKENMETER_README"); p != "" {
		anchors["light-tokenmeter"] = readmeAnchors(t, p)
	}
	tmBin := os.Getenv("TOKENMETER_BIN")
	if tmBin == "" || anchors["light-tokenmeter"] == nil {
		t.Log("TOKENMETER_BIN or TOKENMETER_README unset: light-tokenmeter commands and links are not checked")
	}

	for _, path := range dirs {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		dir := filepath.Base(filepath.Dir(path))
		name, desc := frontmatter(text)
		if name != dir || desc == "" || len(desc) > 1024 {
			t.Errorf("%s: frontmatter name %q (want %q), description of %d characters", path, name, dir, len(desc))
		}
		for _, m := range codeSpan.FindAllStringSubmatch(text, -1) {
			span := strings.TrimSpace(m[1])
			switch {
			case strings.HasPrefix(span, "ktms "):
				if err := checkKtms(span); err != "" {
					t.Errorf("%s: `%s`: %s", dir, span, err)
				}
			case strings.HasPrefix(span, "tokenmeter ") && tmBin != "":
				if err := checkTokenmeter(tmBin, span); err != "" {
					t.Errorf("%s: `%s`: %s", dir, span, err)
				}
			case toolName.MatchString(span) && !strings.HasSuffix(span, "usd"):
				if !slices.Contains(ktmsTools, span) && !slices.Contains(tokenmeterTools, span) {
					t.Errorf("%s: MCP tool `%s` is served by neither ktms mcp nor tokenmeter mcp", dir, span)
				}
			}
		}
		for _, m := range mdLink.FindAllStringSubmatch(text, -1) {
			if a := anchors[m[2]]; a != nil && !a[m[3]] {
				t.Errorf("%s: %s#%s: no such section", dir, m[1], m[3])
			}
		}
	}

	var mcpJSON struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	b, err := os.ReadFile("../../plugin/.mcp.json")
	if err != nil || json.Unmarshal(b, &mcpJSON) != nil {
		t.Fatalf("plugin/.mcp.json: %v", err)
	}
	if s := mcpJSON.MCPServers["ktms"]; s.Command != "ktms" || !slices.Equal(s.Args, []string{"mcp"}) {
		t.Errorf("plugin/.mcp.json ktms = %+v", s)
	}
	if s := mcpJSON.MCPServers["tokenmeter"]; s.Command != "tokenmeter" || !slices.Equal(s.Args, []string{"mcp"}) {
		t.Errorf("plugin/.mcp.json tokenmeter = %+v", s)
	}
}

func frontmatter(text string) (name, desc string) {
	if !strings.HasPrefix(text, "---\n") {
		return "", ""
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return "", ""
	}
	for _, line := range strings.Split(text[4:4+end], "\n") {
		if v, ok := strings.CutPrefix(line, "name: "); ok {
			name = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "description: "); ok {
			desc = strings.TrimSpace(v)
		}
	}
	return name, desc
}

// checkKtms finds the command a span shows and checks its flags.
func checkKtms(span string) string {
	words := strings.Fields(span)[1:]
	a := &app{stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard}
	root := a.root()
	root.InitDefaultCompletionCmd()
	cmd, rest, err := root.Find(words)
	if err != nil {
		return err.Error()
	}
	if cmd.HasSubCommands() && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		return "no subcommand " + rest[0] + " of " + cmd.CommandPath()
	}
	if cmd == root {
		return "not a command"
	}
	for _, w := range words {
		if f, ok := flagName(w); ok && cmd.Flags().Lookup(f) == nil && cmd.InheritedFlags().Lookup(f) == nil {
			return "no flag --" + f + " on " + cmd.CommandPath()
		}
	}
	return ""
}

func flagName(w string) (string, bool) {
	if !strings.HasPrefix(w, "--") || len(w) < 3 {
		return "", false
	}
	f, _, _ := strings.Cut(w[2:], "=")
	return f, true
}

// checkTokenmeter asks the built tokenmeter for the command's help.
func checkTokenmeter(bin, span string) string {
	var path []string
	for _, w := range strings.Fields(span)[1:] {
		if strings.HasPrefix(w, "-") || strings.ToUpper(w) == w {
			break
		}
		path = append(path, w)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, append(path, "--help")...).CombinedOutput()
	if err != nil {
		return "tokenmeter " + strings.Join(path, " ") + " --help: " + err.Error()
	}
	help := string(out)
	if !strings.Contains(help, "tokenmeter "+strings.Join(path, " ")) {
		return "no command tokenmeter " + strings.Join(path, " ")
	}
	for _, w := range strings.Fields(span) {
		if f, ok := flagName(w); ok && !strings.Contains(help, "--"+f) {
			return "no flag --" + f
		}
	}
	return ""
}

func mcpToolNames(t *testing.T) []string {
	t.Helper()
	a := &app{stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard, timeout: time.Second}
	st, ct := mcp.NewInMemoryTransports()
	go func() { _ = a.mcpServer(true).Run(context.Background(), st) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "skills"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

// readmeAnchors are the section ids GitHub gives a Markdown file's headings.
func readmeAnchors(t *testing.T, path string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	inCode := false
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "```") {
			inCode = !inCode
		}
		if inCode || !strings.HasPrefix(line, "#") {
			continue
		}
		h := strings.TrimSpace(strings.TrimLeft(line, "#"))
		var s strings.Builder
		for _, r := range strings.ToLower(h) {
			switch {
			case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
				s.WriteRune(r)
			case r == ' ':
				s.WriteByte('-')
			}
		}
		out[s.String()] = true
	}
	return out
}


func TestSkillChecksCatchStaleCommands(t *testing.T) {
	for span, ok := range map[string]bool{
		"ktms route explain KEY --effort low --attr k=v": true,
		"ktms completion zsh":                            true,
		"ktms keys mnt NAME":                             false,
		"ktms keys mint NAME --colour red":               false,
		"ktms nonsense":                                  false,
	} {
		if got := checkKtms(span); (got == "") != ok {
			t.Errorf("checkKtms(%q) = %q", span, got)
		}
	}
	a := readmeAnchors(t, "../../README.md")
	if !a["5-spend-limits-optional"] || !a["command-line-tool"] || a["no-such-section"] {
		t.Error("README anchors are not computed as GitHub does")
	}
}
