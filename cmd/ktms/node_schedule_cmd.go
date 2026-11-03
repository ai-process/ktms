package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

// nodeScheduleCmd manages the schedules KTMS sends to nodes whose agents
// run with --schedule-source=ktms.
func (a *app) nodeScheduleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Schedules KTMS sends to nodes that hand theirs to it",
		Long: `Schedules KTMS keeps for nodes whose agent runs with --schedule-source=ktms (the
tray's "Schedule set by KTMS"). Other nodes follow their own file and ignore these.`,
	}
	var format string
	show := &cobra.Command{
		Use: "show NODE_ID", Short: "Print a node's schedule and override", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			p, err := newPrinter(a.stdout, format)
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				info, err := c.GetNodeSchedule(ctx, &pb.GetNodeScheduleRequest{NodeId: args[0]})
				if err != nil {
					return err
				}
				if !p.table() {
					return p.data(messages([]*pb.NodeScheduleInfo{info})[0])
				}
				printNodeSchedule(a.stdout, info)
				return nil
			})
		},
	}
	outputFlag(show, &format)

	var lsFormat string
	ls := &cobra.Command{
		Use: "list", Short: "List the nodes KTMS keeps a schedule for", Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			p, err := newPrinter(a.stdout, lsFormat)
			if err != nil {
				return err
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				res, err := c.ListNodeSchedules(ctx, &pb.ListNodeSchedulesRequest{})
				if err != nil {
					return err
				}
				if !p.table() {
					return p.data(messages(res.GetSchedules()))
				}
				var rows [][]string
				for _, s := range res.GetSchedules() {
					rows = append(rows, []string{s.GetNodeId(), strconv.Itoa(len(ruleLines(s.GetText()))), overrideCell(s.GetOverride()),
						strconv.FormatInt(s.GetVersion(), 10), when(s.GetUpdatedAt())})
				}
				return p.rows([]string{"NODE", "RULES", "OVERRIDE", "VERSION", "UPDATED"}, rows)
			})
		},
	}
	outputFlag(ls, &lsFormat)

	var file string
	set := &cobra.Command{
		Use:   "set NODE_ID --file FILE",
		Short: "Replace a node's rules (\"[days] HH:MM-HH:MM level\" per line; - reads stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if file == "" {
				return usageError{fmt.Errorf("--file is required (- for stdin)")}
			}
			var data []byte
			var err error
			if file == "-" {
				data, err = io.ReadAll(a.stdin)
			} else {
				data, err = os.ReadFile(file)
			}
			if err != nil {
				return usageError{err}
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				info, err := c.SetNodeSchedule(ctx, &pb.SetNodeScheduleRequest{NodeId: args[0], Text: string(data)})
				if err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Schedule of %s set (version %d); the node applies it on its next heartbeat.\n", args[0], info.GetVersion())
				return nil
			})
		},
	}
	set.Flags().StringVar(&file, "file", "", "rules file, or - for stdin")

	del := &cobra.Command{
		Use: "delete NODE_ID", Short: "Drop a node's schedule and override; the node then runs at 100%", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				if _, err := c.DeleteNodeSchedule(ctx, &pb.DeleteNodeScheduleRequest{NodeId: args[0]}); err != nil {
					return err
				}
				fmt.Fprintf(a.stdout, "Schedule of %s deleted.\n", args[0])
				return nil
			})
		},
	}

	var level int
	var dur string
	var clear bool
	override := &cobra.Command{
		Use:   "override NODE_ID (--level N --for DURATION | --clear)",
		Short: "Hold a node at a level for a while, over its schedule",
		Long: `Hold a node at a level for 1 minute to 24 hours, over the schedule KTMS sends it.
The owner's own pause on the machine still wins.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			req := &pb.SetNodeCapacityOverrideRequest{NodeId: args[0], Clear: clear}
			if !clear {
				if dur == "" || level < 0 || level > 100 {
					return usageError{fmt.Errorf("give --level 0-100 and --for, or --clear")}
				}
				d, err := time.ParseDuration(dur)
				if err != nil {
					return usageError{fmt.Errorf("--for: %w", err)}
				}
				req.Level, req.Until = uint32(level), timestamppb.New(time.Now().Add(d))
			}
			return a.call(func(ctx context.Context, c pb.AdminServiceClient) error {
				info, err := c.SetNodeCapacityOverride(ctx, req)
				if err != nil {
					return err
				}
				if clear {
					fmt.Fprintf(a.stdout, "Override of %s cleared.\n", args[0])
				} else {
					fmt.Fprintf(a.stdout, "%s held at %s.\n", args[0], overrideCell(info.GetOverride()))
				}
				return nil
			})
		},
	}
	override.Flags().IntVar(&level, "level", -1, "level, 0-100")
	override.Flags().StringVar(&dur, "for", "", "how long, e.g. 1h")
	override.Flags().BoolVar(&clear, "clear", false, "remove the override")

	cmd.AddCommand(show, ls, set, del, override)
	return cmd
}

func printNodeSchedule(w io.Writer, s *pb.NodeScheduleInfo) {
	if s.GetVersion() == 0 {
		fmt.Fprintf(w, "No schedule kept for %s: a node that hands its schedule to KTMS runs at 100%%.\n", s.GetNodeId())
		return
	}
	fmt.Fprintf(w, "Node %s, version %d, updated %s\n", s.GetNodeId(), s.GetVersion(), when(s.GetUpdatedAt()))
	fmt.Fprintf(w, "Override: %s\n", overrideCell(s.GetOverride()))
	if strings.TrimSpace(s.GetText()) == "" {
		fmt.Fprintln(w, "Rules: none (100%)")
		return
	}
	fmt.Fprintf(w, "Rules:\n%s", s.GetText())
	if !strings.HasSuffix(s.GetText(), "\n") {
		fmt.Fprintln(w)
	}
}

func overrideCell(o *pb.NodeCapacityOverride) string {
	if o == nil {
		return "-"
	}
	return fmt.Sprintf("%d%% until %s", o.GetLevel(), when(o.GetUntil()))
}

func ruleLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if l, _, _ = strings.Cut(l, "#"); strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// levelCell is a node's level and what set it: "50% schedule", "0% ktms_override".
func levelCell(n *pb.NodeInfo) string {
	out := fmt.Sprintf("%d%%", n.GetCapacityPercent())
	if src := n.GetCapacitySource(); src != "" {
		out += " " + src
	}
	if n.GetScheduleSource() == "ktms" && !strings.HasPrefix(n.GetCapacitySource(), "ktms") {
		out += " (ktms)"
	}
	return out
}
