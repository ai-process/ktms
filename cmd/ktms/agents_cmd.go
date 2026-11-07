package main

import (
	"context"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

func (a *app) agentsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "agents", Short: "Agents talking through the MCP server agents, and their conversations"}

	var format, owner string
	ls := &cobra.Command{
		Use: "list", Short: "List agents and their cards", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				res, err := c.ListAgents(ctx, &pb.ListAgentsRequest{Owner: owner})
				if err != nil {
					return err
				}
				if !p.table() {
					return p.data(messages(res.GetAgents()))
				}
				var rows [][]string
				for _, ag := range res.GetAgents() {
					rows = append(rows, []string{ag.GetName(), ag.GetApiKeyName(), orDash(ag.GetOwner()), list(ag.GetSkills()),
						list(ag.GetAcceptFrom()), orDash(ag.GetRecording()), when(ag.GetUpdatedAt())})
				}
				return p.rows([]string{"NAME", "KEY", "OWNER", "SKILLS", "ACCEPTS FROM", "RECORDING", "UPDATED"}, rows)
			})
		},
	}
	outputFlag(ls, &format)
	ls.Flags().StringVar(&owner, "owner", "", "only this person's agents")

	var tFormat string
	var open bool
	var lim int32
	tasks := &cobra.Command{
		Use: "tasks AGENT", Short: "Tasks an agent sent or received, most recent first", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := newPrinter(a.stdout, tFormat)
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				res, err := c.ListAgentTasks(ctx, &pb.ListAgentTasksRequest{Agent: args[0], OpenOnly: open, Limit: lim})
				if err != nil {
					return err
				}
				if !p.table() {
					return p.data(messages(res.GetTasks()))
				}
				var rows [][]string
				for _, t := range res.GetTasks() {
					rows = append(rows, []string{t.GetTaskId(), t.GetFrom(), t.GetTo(), t.GetStatus(), strconv.Itoa(int(t.GetDepth())),
						orDash(t.GetStatusReason()), when(t.GetUpdatedAt())})
				}
				return p.rows([]string{"TASK", "FROM", "TO", "STATUS", "DEPTH", "REASON", "UPDATED"}, rows)
			})
		},
	}
	outputFlag(tasks, &tFormat)
	tasks.Flags().BoolVar(&open, "open", false, "only tasks that have not ended")
	tasks.Flags().Int32Var(&lim, "limit", 100, "most tasks to show (at most 500)")

	var sFormat string
	show := &cobra.Command{
		Use: "task ID", Short: "A task with its messages", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := newPrinter(a.stdout, sFormat)
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				t, err := c.GetAgentTask(ctx, &pb.GetAgentTaskRequest{TaskId: args[0]})
				if err != nil {
					return err
				}
				if !p.table() {
					return p.data(t)
				}
				fmt.Fprintf(a.stdout, "%s → %s  %s", t.GetFrom(), t.GetTo(), t.GetStatus())
				if t.GetStatusReason() != "" {
					fmt.Fprintf(a.stdout, " (%s)", t.GetStatusReason())
				}
				fmt.Fprintf(a.stdout, "\ncontext %s, depth %d\n", t.GetContextId(), t.GetDepth())
				for i, m := range t.GetMessages() {
					fmt.Fprintf(a.stdout, "\n%s  %s:\n%s\n", when(m.GetCreatedAt()), m.GetFrom(), m.GetText())
					if i == 0 && t.GetOmittedMessages() > 0 {
						fmt.Fprintf(a.stdout, "\n… %d messages left out …\n", t.GetOmittedMessages())
					}
				}
				return nil
			})
		},
	}
	outputFlag(show, &sFormat)

	rec := &cobra.Command{
		Use: "recording NAME off|metadata|full|default", Short: "Set how much of an agent's conversations the journal keeps", Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			level := args[1]
			if level == "default" {
				level = ""
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				if _, err := c.SetAgentRecording(ctx, &pb.SetAgentRecordingRequest{Name: args[0], Recording: level}); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Recording of %s set to %s.\n", args[0], args[1])
				return nil
			})
		},
	}

	cmd.AddCommand(ls, tasks, show, rec)
	return cmd
}
