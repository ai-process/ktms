package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ai-process/ktms/internal/agentrun"
	"github.com/ai-process/ktms/internal/proxydb"
)

// inboxCmd acts as one agent, with its key, over the MCP server agents: what
// the agent's person uses from a terminal. `ktms agents` is the operator's view.
func (a *app) inboxCmd() *cobra.Command {
	var keyFile string
	cmd := &cobra.Command{
		Use:   "inbox",
		Short: "Your agent's inbox with its key: tasks to answer, tasks you sent, other agents",
		Long: `Act as the agent whose key it holds (KTMS_AGENT_KEY or --agent-key-file, a key with the
mcp scope), over the MCP server agents at --http. Without a subcommand it lists the open tasks
sent to the agent. ktms agents is the operator's view with an admin key.`,
		Args: cobra.NoArgs,
	}
	cmd.PersistentFlags().StringVar(&keyFile, "agent-key-file", "", "file holding the agent's key (default: KTMS_AGENT_KEY)")

	// with runs fn with a client of the agents server, until ctx ends or an interrupt.
	with := func(d time.Duration, fn func(ctx context.Context, c *agentrun.Client) error) error {
		key, err := agentKey(keyFile)
		if err != nil {
			return err
		}
		base, err := a.t.httpBase()
		if err != nil {
			return err
		}
		c := &agentrun.Client{Endpoint: strings.TrimRight(base, "/") + "/mcp/agents", Key: key, HTTP: a.t.httpClient(), Version: version}
		defer c.Close()
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		ctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		return fn(ctx, c)
	}
	call := func(ctx context.Context, c *agentrun.Client, tool string, args map[string]any, out any) error {
		return c.Call(ctx, tool, args, out)
	}

	listFlags := func(c *cobra.Command, all *bool, ref *string, pinned *bool, wait *time.Duration, format *string) {
		if all != nil {
			c.Flags().BoolVar(all, "all", false, "tasks that have ended too")
		}
		c.Flags().StringVar(ref, "ref", "", "only tasks tied to this external_ref, such as PROJ-123")
		c.Flags().BoolVar(pinned, "pinned", false, "only pinned tasks")
		c.Flags().DurationVar(wait, "wait", 0, "when there is no task yet, wait up to this long (at most 50s) for one")
		outputFlag(c, format)
	}
	listArgs := func(ref string, pinned bool, wait time.Duration) map[string]any {
		args := map[string]any{"limit": 100}
		if ref != "" {
			args["external_ref"] = ref
		}
		if pinned {
			args["pinned"] = true
		}
		if wait > 0 {
			args["wait"] = int(min(wait, 50*time.Second).Seconds())
		}
		return args
	}
	printTasks := func(format, other string, ts []agentrun.Task) error {
		p, err := newPrinter(a.stdout, format)
		if err != nil {
			return err
		}
		if !p.table() {
			return p.data(map[string]any{"tasks": ts})
		}
		var rows [][]string
		for _, t := range ts {
			who := t.From
			if other == "TO" {
				who = t.To
			}
			row := []string{t.TaskID, who, t.Status, orDash(t.ReasonCode), orDash(t.ExternalRef), list(t.PinnedBy), t.Updated.UTC().Format(time.RFC3339)}
			if other == "FROM" { // the outbox comes without messages
				row = append(row, firstLine(t))
			}
			rows = append(rows, row)
		}
		head := []string{"TASK", other, "STATUS", "WAITS ON", "REF", "PINNED", "UPDATED"}
		if other == "FROM" {
			head = append(head, "LAST MESSAGE")
		}
		return p.rows(head, rows)
	}

	var iFormat, iRef string
	var iAll, iPinned bool
	var iWait time.Duration
	cmd.RunE = func(*cobra.Command, []string) error {
		return with(a.timeout+iWait, func(ctx context.Context, c *agentrun.Client) error {
			args := listArgs(iRef, iPinned, iWait)
			if iAll {
				args["all"] = true
			}
			var out struct {
				Tasks []agentrun.Task `json:"tasks"`
			}
			if err := call(ctx, c, "inbox", args, &out); err != nil {
				return err
			}
			return printTasks(iFormat, "FROM", out.Tasks)
		})
	}
	listFlags(cmd, &iAll, &iRef, &iPinned, &iWait, &iFormat)

	var oFormat, oRef string
	var oPinned bool
	var oWait time.Duration
	outbox := &cobra.Command{
		Use: "outbox", Short: "Tasks your agent sent, most recently changed first", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return with(a.timeout+oWait, func(ctx context.Context, c *agentrun.Client) error {
				var out struct {
					Tasks []agentrun.Task `json:"tasks"`
				}
				if err := call(ctx, c, "outbox", listArgs(oRef, oPinned, oWait), &out); err != nil {
					return err
				}
				return printTasks(oFormat, "TO", out.Tasks)
			})
		},
	}
	listFlags(outbox, nil, &oRef, &oPinned, &oWait, &oFormat)

	var sFormat string
	show := &cobra.Command{
		Use: "show ID", Short: "A task with all its messages", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return with(a.timeout, func(ctx context.Context, c *agentrun.Client) error {
				var t agentrun.Task
				if err := call(ctx, c, "get_task", map[string]any{"task_id": args[0]}, &t); err != nil {
					return err
				}
				return a.printTask(sFormat, t)
			})
		},
	}
	outputFlag(show, &sFormat)

	var ref, contextID, nFormat string
	var wait time.Duration
	send := &cobra.Command{
		Use:   "send AGENT TEXT|-",
		Short: "Send a task to another agent; - reads the text from stdin",
		Long: `Send a task to another agent. With --wait the command stays until the task ends or
asks a question, for up to that long, and prints it; it exits 0 when the task completed.`,
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			text, err := a.argText(args[1])
			if err != nil {
				return err
			}
			return with(a.timeout+wait, func(ctx context.Context, c *agentrun.Client) error {
				in := map[string]any{"agent": args[0], "text": text}
				if ref != "" {
					in["external_ref"] = ref
				}
				if contextID != "" {
					in["context_id"] = contextID
				}
				var t agentrun.Task
				if err := call(ctx, c, "send_message", in, &t); err != nil {
					return err
				}
				fmt.Fprintf(a.stderr, "Sent task %s to %s.\n", t.TaskID, t.To)
				if wait <= 0 {
					return a.printTask(nFormat, t)
				}
				if t, err = waitTask(ctx, c, t, time.Now().Add(wait)); err != nil {
					return err
				}
				if !ended(t) {
					fmt.Fprintf(a.stderr, "Still %s after %s.\n", t.Status, wait)
				}
				// The request is the caller's own: print what came back.
				if len(t.Messages) > 0 && t.Messages[0].Role == "user" {
					t.Messages = t.Messages[1:]
				}
				if err := a.printTask(nFormat, t); err != nil {
					return err
				}
				if t.Status != proxydb.TaskCompleted {
					return codeError{exitFailed}
				}
				return nil
			})
		},
	}
	send.Flags().StringVar(&ref, "ref", "", "the issue it is about elsewhere, such as PROJ-123 or a URL")
	send.Flags().StringVar(&contextID, "context", "", "continue the conversation of this context_id")
	send.Flags().DurationVar(&wait, "wait", 0, "wait up to this long for the answer, such as 20m")
	outputFlag(send, &nFormat)

	var status string
	var needsOwner, correction bool
	reply := &cobra.Command{
		Use:   "reply ID TEXT|-",
		Short: "Answer a task, ask its sender a question, or correct an ended answer; - reads stdin",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			text, err := a.argText(args[1])
			if err != nil {
				return err
			}
			in := map[string]any{"task_id": args[0], "text": text}
			if status != "" {
				in["status"] = status
			}
			if needsOwner {
				in["needs_owner"] = true
			}
			if correction {
				in["correction"] = true
			}
			return with(a.timeout, func(ctx context.Context, c *agentrun.Client) error {
				var t agentrun.Task
				if err := call(ctx, c, "reply", in, &t); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Task %s is %s.\n", t.TaskID, t.Status)
				return nil
			})
		},
	}
	reply.Flags().StringVar(&status, "status", "", "completed (default for a recipient), working, input-required, failed or canceled")
	reply.Flags().BoolVar(&needsOwner, "needs-owner", false, "with input-required: the question is for your own person")
	reply.Flags().BoolVar(&correction, "correction", false, "amend your answer to a task that has ended")

	var off bool
	pin := &cobra.Command{
		Use: "pin ID", Short: "Keep a task past the retention of old tasks; --off lets it go", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return with(a.timeout, func(ctx context.Context, c *agentrun.Client) error {
				var t agentrun.Task
				if err := call(ctx, c, "pin_task", map[string]any{"task_id": args[0], "pinned": !off}, &t); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Task %s pinned by: %s.\n", t.TaskID, list(t.PinnedBy))
				return nil
			})
		},
	}
	pin.Flags().BoolVar(&off, "off", false, "unpin")

	var fFormat string
	find := &cobra.Command{
		Use: "find [WORDS...]", Short: "Agents you can send tasks to; with words, those whose card holds all of them",
		RunE: func(_ *cobra.Command, args []string) error {
			return with(a.timeout, func(ctx context.Context, c *agentrun.Client) error {
				var out struct {
					Agents []struct {
						Name        string   `json:"name"`
						Owner       string   `json:"owner"`
						Description string   `json:"description"`
						Skills      []string `json:"skills"`
						Repos       []string `json:"repos"`
						You         bool     `json:"you"`
						AcceptsYou  *bool    `json:"accepts_you"`
					} `json:"agents"`
				}
				if err := call(ctx, c, "list_agents", map[string]any{"query": strings.Join(args, " ")}, &out); err != nil {
					return err
				}
				p, err := newPrinter(a.stdout, fFormat)
				if err != nil {
					return err
				}
				if !p.table() {
					return p.data(out)
				}
				var rows [][]string
				for _, ag := range out.Agents {
					takes := "yes"
					switch {
					case ag.You:
						takes = "you"
					case ag.AcceptsYou != nil && !*ag.AcceptsYou:
						takes = "no"
					}
					rows = append(rows, []string{ag.Name, orDash(ag.Owner), list(ag.Skills), list(ag.Repos), takes, orDash(ag.Description)})
				}
				return p.rows([]string{"NAME", "OWNER", "SKILLS", "REPOS", "TAKES YOURS", "DESCRIPTION"}, rows)
			})
		},
	}
	outputFlag(find, &fFormat)

	var desc string
	var skills, repos, accept []string
	register := &cobra.Command{
		Use: "register", Short: "Describe your agent to the others: what it does, skills, repositories, who may send it tasks", Args: cobra.NoArgs,
		Long: `Change your agent's card. Only the fields you pass change; --accept-from "" lets anyone
who sees the agent send it tasks.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return with(a.timeout, func(ctx context.Context, c *agentrun.Client) error {
				card, err := c.Card(ctx)
				if err != nil {
					return err
				}
				f := cmd.Flags()
				if f.Changed("description") {
					card.Description = desc
				}
				if f.Changed("skill") {
					card.Skills = skills
				}
				if f.Changed("repo") {
					card.Repos = repos
				}
				if f.Changed("accept-from") {
					card.AcceptFrom = accept
				}
				out, err := c.SetCard(ctx, card)
				if err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Card of %s updated.\n", out.Name)
				return nil
			})
		},
	}
	register.Flags().StringVar(&desc, "description", "", "what your agent can help with")
	register.Flags().StringSliceVar(&skills, "skill", nil, "a skill, such as code-review; repeatable")
	register.Flags().StringSliceVar(&repos, "repo", nil, "a repository it works in; repeatable")
	register.Flags().StringSliceVar(&accept, "accept-from", nil, `who may send it tasks: "name", "owner:EMAIL" or "team:ID"; empty for anyone who sees it`)

	cmd.AddCommand(outbox, show, send, reply, pin, find, register)
	return cmd
}

// ended: the task is over or asks its sender, so waiting for it is done.
func ended(t agentrun.Task) bool {
	return proxydb.TaskTerminal(t.Status) || t.Status == proxydb.TaskInputRequired && t.ReasonCode == proxydb.ReasonQuestion
}

// waitTask waits on the outbox until the task ends or asks its sender, or
// until deadline, then reads it with its messages.
func waitTask(ctx context.Context, c *agentrun.Client, t agentrun.Task, deadline time.Time) (agentrun.Task, error) {
	since := t.Updated
	for !ended(t) {
		left := time.Until(deadline)
		if left < time.Second {
			break
		}
		var out struct {
			Tasks []agentrun.Task `json:"tasks"`
		}
		start := time.Now()
		err := c.Call(ctx, "outbox", map[string]any{"since": since.Format(time.RFC3339Nano), "wait": int(min(left, 50*time.Second).Seconds()), "limit": 100}, &out)
		if err != nil {
			return t, err
		}
		for _, o := range out.Tasks {
			if o.Updated.After(since) {
				since = o.Updated
			}
			if o.TaskID == t.TaskID {
				t = o
			}
		}
		if len(out.Tasks) == 0 && time.Since(start) < time.Second {
			// A server before 1.6 answers at once; do not spin on it.
			select {
			case <-ctx.Done():
				return t, ctx.Err()
			case <-time.After(min(5*time.Second, time.Until(deadline))):
			}
		}
	}
	var full agentrun.Task
	err := c.Call(ctx, "get_task", map[string]any{"task_id": t.TaskID}, &full)
	return full, err
}

func (a *app) printTask(format string, t agentrun.Task) error {
	p, err := newPrinter(a.stdout, format)
	if err != nil {
		return err
	}
	if !p.table() {
		return p.data(t)
	}
	fmt.Fprintf(a.stdout, "%s → %s  %s", t.From, t.To, t.Status)
	if t.ReasonCode != "" {
		fmt.Fprintf(a.stdout, " (%s)", t.ReasonCode)
	}
	if t.AnsweredBy == proxydb.AuthorOwner {
		fmt.Fprint(a.stdout, ", answered by the owner")
	}
	fmt.Fprintf(a.stdout, "\ntask %s, context %s", t.TaskID, t.ContextID)
	if t.ExternalRef != "" {
		fmt.Fprintf(a.stdout, ", ref %s", t.ExternalRef)
	}
	if len(t.PinnedBy) > 0 {
		fmt.Fprintf(a.stdout, ", pinned by %s", strings.Join(t.PinnedBy, ", "))
	}
	fmt.Fprintln(a.stdout)
	for _, m := range t.Messages {
		who := m.From
		if m.Author == proxydb.AuthorOwner {
			who += "'s owner"
		}
		if m.Correction {
			who += ", correction"
		}
		fmt.Fprintf(a.stdout, "\n%s  %s:\n%s\n", m.At.UTC().Format(time.RFC3339), who, m.Text)
	}
	return nil
}

// argText is the text argument, or stdin for "-".
func (a *app) argText(arg string) (string, error) {
	if arg != "-" {
		return arg, nil
	}
	b, err := io.ReadAll(a.stdin)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", usageError{errors.New("no text on stdin")}
	}
	return string(b), nil
}

func firstLine(t agentrun.Task) string {
	if len(t.Messages) == 0 {
		return "-"
	}
	s := strings.TrimSpace(t.Messages[len(t.Messages)-1].Text)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	if r := []rune(s); len(r) > 60 {
		s = string(r[:60]) + "…"
	}
	return s
}
