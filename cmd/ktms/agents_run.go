package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ai-process/ktms/internal/agentrun"
)

func (a *app) agentsRunCmd() *cobra.Command {
	var runner, model, command, dir, keyFile, pauseFile string
	var skills, repos []string
	var desc string
	var interval, timeout time.Duration
	var once bool
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Answer an agent's inbox with a local CLI agent (claude, codex, agy), read-only",
		Long: `Run as the agent whose key it holds (KTMS_AGENT_KEY or --agent-key-file, a key with
the mcp scope): wait on its inbox, hand each task that waits for it to a CLI agent in --dir
and send what the CLI printed back as the reply. The CLI ends its answer with a line
"STATUS: completed", "STATUS: input-required" (a question for the sender) or
"STATUS: failed"; without one the answer completes the task.

The presets run read-only: claude with only Read, Grep and Glob and no MCP servers,
codex in its read-only sandbox, agy in plan mode (a run it ends at a refused shell command
is resumed once). --description, --skill and --repo set the agent's card. --cmd runs your own program with the
prompt on stdin. Tasks are answered one at a time; run one per agent key. While
--pause-file exists no task is taken.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			key, err := agentKey(keyFile)
			if err != nil {
				return err
			}
			base, err := a.t.httpBase()
			if err != nil {
				return err
			}
			if dir == "" {
				if dir, err = os.Getwd(); err != nil {
					return err
				}
			}
			var r agentrun.Command
			if command != "" {
				f := strings.Fields(command)
				r = agentrun.Command{Name: f[0], Args: f[1:], Dir: dir}
			} else if r, err = agentrun.Preset(runner, dir, model); err != nil {
				return usageError{err}
			}
			logf := func(format string, args ...any) {
				fmt.Fprintf(a.stderr, "%s "+format+"\n", append([]any{time.Now().Format(time.TimeOnly)}, args...)...)
			}
			r.Stderr = func(s string) { logf("%s stderr: %s", r.Name, strings.TrimSpace(s)) }
			c := &agentrun.Client{Endpoint: strings.TrimRight(base, "/") + "/mcp/agents", Key: key, HTTP: a.t.httpClient(), Version: version}
			defer c.Close()
			cfg := agentrun.Config{Interval: interval, Timeout: timeout, PauseFile: pauseFile, Logf: logf}
			if err := runCard(context.Background(), cmd, c, logf, desc, skills, repos); err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if once {
				return agentrun.Once(ctx, c, r, cfg)
			}
			logf("answering the inbox at %s with %s in %s", c.Endpoint, r.Name, dir)
			return agentrun.Run(ctx, c, r, cfg)
		},
	}
	f := cmd.Flags()
	f.StringVar(&runner, "runner", "claude", "claude, codex or agy")
	f.StringVar(&model, "model", "", "model of the preset CLI, such as gemini-3.8-flash-high for agy (default: the CLI's own)")
	f.StringVar(&command, "cmd", "", "your own command instead of a preset; it gets the prompt on stdin")
	f.StringVar(&dir, "dir", "", "directory the CLI works in (default: the current one)")
	f.StringVar(&keyFile, "agent-key-file", "", "file holding the agent's key (default: KTMS_AGENT_KEY)")
	f.StringVar(&pauseFile, "pause-file", "", "while this file exists, take no task")
	f.DurationVar(&interval, "interval", 15*time.Second, "time between inbox polls when KTMS cannot wait (before 1.6), and after an error")
	f.DurationVar(&timeout, "task-timeout", 30*time.Minute, "longest run of the CLI on one task; then the task fails")
	f.StringVar(&desc, "description", "", "set the agent's card: what it can help with")
	f.StringSliceVar(&skills, "skill", nil, "set the card's skills, such as code-review; repeatable")
	f.StringSliceVar(&repos, "repo", nil, "set the card's repositories; repeatable")
	f.BoolVar(&once, "once", false, "answer the tasks due now and exit")
	return cmd
}

// runCard changes the fields of the agent's card given as flags, and warns
// when the card says nothing, as no one finds the agent by what it does.
func runCard(ctx context.Context, cmd *cobra.Command, c *agentrun.Client, logf func(string, ...any), desc string, skills, repos []string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	f := cmd.Flags()
	set := f.Changed("description") || f.Changed("skill") || f.Changed("repo")
	card, err := c.Card(ctx)
	if err != nil {
		if !set {
			logf("reading the agent's card: %v", err) // the inbox may still work
			return nil
		}
		return err
	}
	if set {
		if f.Changed("description") {
			card.Description = desc
		}
		if f.Changed("skill") {
			card.Skills = skills
		}
		if f.Changed("repo") {
			card.Repos = repos
		}
		if card, err = c.SetCard(ctx, card); err != nil {
			return err
		}
		logf("card of %s updated", card.Name)
	}
	if card.Description == "" && len(card.Skills) == 0 {
		logf("the card of %s is empty, so ktms inbox find will not find it by what it does; set it with --description and --skill", card.Name)
	}
	return nil
}

func agentKey(file string) (string, error) {
	if file != "" {
		return readKeyFile(file)
	}
	if k := strings.TrimSpace(os.Getenv("KTMS_AGENT_KEY")); k != "" {
		return k, nil
	}
	return "", authError{errors.New("no agent key: set KTMS_AGENT_KEY or pass --agent-key-file")}
}
