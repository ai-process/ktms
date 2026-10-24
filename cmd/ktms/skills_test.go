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

// tokenmeterTools are light-tokenmeter's MCP tools, read from its source when
// TOKENMETER_MCP_SRC names it, as CI does; this list is the fallback.
var tokenmeterTools = []string{"usage_summary", "top_spenders", "cost_per_action", "usage_by_meta", "health"}

var (
	codeSpan = regexp.MustCompile("`([^`]+)`")
	mdLink   = regexp.MustCompile(`\]\((https://github\.com/ai-process/(ktms|light-tokenmeter)/blob/main/README\.md)#([^)]+)\)`)
	// A tool name: a verb these servers use and more words, or health.
	toolName = regexp.MustCompile(`^((list|get|config|mint|revoke|set|delete|drain|explain|usage|top|cost)_[a-z_]+|health)$`)
	// Fields the skills name that look like tool names.
	notTools  = []string{"cost_usd", "cost_nanousd", "top_p", "list_price"}
	tmToolDef = regexp.MustCompile(`read\("([a-z_]+)"`)
)

// TestSkills fails on a skill that names a command, flag, MCP tool or README
// section this version lacks: an assistant would follow it into an error.
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
	tmTools := tokenmeterTools
	if p := os.Getenv("TOKENMETER_MCP_SRC"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		tmTools = nil
		for _, m := range tmToolDef.FindAllStringSubmatch(string(b), -1) {
			tmTools = append(tmTools, m[1])
		}
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
			span := strings.Join(strings.Fields(m[1]), " ")
			switch {
			case strings.HasPrefix(span, "ktms "):
				if err := checkKtms(span); err != "" {
					t.Errorf("%s: `%s`: %s", dir, span, err)
				}
			case strings.HasPrefix(span, "tokenmeter ") && tmBin != "":
				if err := checkTokenmeter(tmBin, span); err != "" {
					t.Errorf("%s: `%s`: %s", dir, span, err)
				}
			case toolName.MatchString(span) && !slices.Contains(notTools, span):
				if !slices.Contains(ktmsTools, span) && !slices.Contains(tmTools, span) {
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
		f, short, ok := flagName(w)
		if !ok {
			continue
		}
		if short && cmd.Flags().ShorthandLookup(f) == nil && cmd.InheritedFlags().ShorthandLookup(f) == nil {
			return "no flag -" + f + " on " + cmd.CommandPath()
		}
		if !short && cmd.Flags().Lookup(f) == nil && cmd.InheritedFlags().Lookup(f) == nil {
			return "no flag --" + f + " on " + cmd.CommandPath()
		}
	}
	return ""
}

// flagName reads --name, --name=value and -x.
func flagName(w string) (name string, short, ok bool) {
	switch {
	case strings.HasPrefix(w, "--") && len(w) > 2:
		f, _, _ := strings.Cut(w[2:], "=")
		return f, false, true
	case strings.HasPrefix(w, "-") && len(w) == 2 && w[1] != '-':
		return w[1:], true, true
	}
	return "", false, false
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
		f, short, ok := flagName(w)
		if !ok {
			continue
		}
		pat := `(?m)^\s+--` + regexp.QuoteMeta(f) + `\b`
		if short {
			pat = `(?m)^\s+-` + regexp.QuoteMeta(f) + `, --`
		}
		if !regexp.MustCompile(pat).MatchString(help) {
			return "no flag " + w
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
		"ktms config validate -f FILE":                   true,
		"ktms config validate -x FILE":                   false,
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
