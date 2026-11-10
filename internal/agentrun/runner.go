package agentrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
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
	// Agy: stdout is agy's --output-format json result, and a run that ended
	// on a refused tool is resumed once to answer without it.
	Agy    bool
	Stderr func(string)
}

// ErrRefused is a run that ended without an answer because the CLI refused
// a tool it needed, such as a shell command; errors.As finds the *Refused.
var ErrRefused = errors.New("the agent's CLI refused a tool it needed and gave no answer")

// Refused names the tools the CLI refused, as agy reports them.
type Refused struct{ Actions []string }

func (r *Refused) Error() string { return ErrRefused.Error() + ": " + strings.Join(r.Actions, ", ") }
func (r *Refused) Unwrap() error { return ErrRefused }

// resume is the prompt that continues an agy run cut short by a refused tool.
const resume = "Shell commands and any other refused tool stay refused in this run; do not try them again. Finish the task with what you have read, in the format asked for."

func (c Command) Run(ctx context.Context, prompt string) (string, error) {
	if !c.Agy {
		return c.run(ctx, prompt)
	}
	out, err := c.run(ctx, prompt)
	r := agyResult(out)
	if err == nil && r.Response == "" && len(r.Denied) > 0 && r.ConversationID != "" {
		// In print mode agy ends the run at the first refused tool, after
		// however long it had read; the conversation keeps that reading.
		resumed := c
		resumed.Args = append(append([]string(nil), c.Args...), "--conversation", r.ConversationID)
		out, err = resumed.run(ctx, resume)
		r = agyResult(out)
	}
	if err == nil && r.Response == "" && len(r.Denied) > 0 {
		ref := &Refused{}
		for _, d := range r.Denied {
			if !slices.Contains(ref.Actions, d.Action) {
				ref.Actions = append(ref.Actions, d.Action)
			}
		}
		return "", ref
	}
	return c.local(r.Response), err
}

type agyOut struct {
	ConversationID string `json:"conversation_id"`
	Response       string `json:"response"`
	Denied         []struct {
		Action string `json:"action"`
	} `json:"denied_actions"`
}

// agyResult reads agy's JSON result, the whole output or its last line when
// something else was printed first; any other output is the answer.
func agyResult(out string) agyOut {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, s := range []string{out, lines[len(lines)-1]} {
		var r agyOut
		if json.Unmarshal([]byte(s), &r) == nil && r.ConversationID != "" {
			return r
		}
	}
	return agyOut{Response: out}
}

func (c Command) run(ctx context.Context, prompt string) (string, error) {
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
	res := out.String()
	if !c.Agy {
		res = c.local(res)
	}
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
	// Plan mode does not edit files; without a terminal agy refuses shell
	// commands, which could read files outside the repository.
	"agy": {Name: "agy", Args: []string{"--mode", "plan", "--output-format", "json"}, PromptArg: "-p=", Agy: true},
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
