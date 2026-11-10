package agentrun

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Command runs a program in Dir with the prompt on stdin, or as its last
// argument when PromptArg is set (agy reads no stdin), and returns stdout.
type Command struct {
	Name      string
	Args      []string
	Dir       string
	PromptArg string // e.g. "-p=" for agy; empty passes the prompt on stdin
	Stderr    func(string)
}

func (c Command) Run(ctx context.Context, prompt string) (string, error) {
	args := append([]string(nil), c.Args...)
	if c.PromptArg != "" {
		args = append(args, c.PromptArg+prompt)
	}
	cmd := exec.CommandContext(ctx, c.Name, args...)
	cmd.Dir = c.Dir
	cmd.WaitDelay = 5 * time.Second
	if c.PromptArg == "" {
		cmd.Stdin = strings.NewReader(prompt)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if c.Stderr != nil && errb.Len() > 0 {
		c.Stderr(errb.String())
	}
	res := c.local(out.String())
	if err != nil {
		return res, fmt.Errorf("%s: %w", c.Name, err)
	}
	return res, nil
}

// local makes paths under Dir relative: the answer goes to another person's
// agent, and agy links every file it cites as file:///home/....
func (c Command) local(s string) string {
	if c.Dir == "" || c.Dir == "/" {
		return s
	}
	dir := strings.TrimRight(c.Dir, "/") + "/"
	return strings.ReplaceAll(strings.ReplaceAll(s, "file://"+dir, ""), dir, "")
}

// Presets are read-only runs of the CLI agents: they read and search the
// repository and can change nothing.
var Presets = map[string]Command{
	// Only the read tools are allowed; in print mode anything else is refused
	// without a prompt. No MCP servers, so no tool outside the repository.
	"claude": {Name: "claude", Args: []string{"-p", "--allowedTools", "Read,Grep,Glob", "--strict-mcp-config", "--output-format", "text"}},
	"codex":  {Name: "codex", Args: []string{"exec", "--sandbox", "read-only", "-"}},
	// Plan mode does not edit files; without a terminal agy refuses shell commands.
	"agy": {Name: "agy", Args: []string{"--mode", "plan"}, PromptArg: "-p="},
}

// Preset is a preset by name, run in dir, with model when it is set.
func Preset(name, dir, model string) (Command, error) {
	c, ok := Presets[name]
	if !ok {
		return Command{}, fmt.Errorf("unknown runner %q: want claude, codex, agy or --cmd", name)
	}
	c.Args = append([]string(nil), c.Args...)
	if model != "" {
		// codex takes its flags after the exec subcommand.
		at := 0
		if name == "codex" {
			at = 1
		}
		c.Args = append(c.Args[:at], append([]string{"--model", model}, c.Args[at:]...)...)
	}
	c.Dir = dir
	return c, nil
}
