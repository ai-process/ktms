package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/ai-process/ktms/internal/nodeagent"
)

// controlCommands talk to the running agent over its control socket.
var controlCommands = map[string]func(args []string, stdout, stderr io.Writer) int{
	"status": runStatus,
	"pause":  runPause,
	"level":  runLevel,
	"resume": runResume,
}

func controlFlags(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	socket := fs.String("control-socket", nodeagent.DefaultControlSocket(), "The agent's control socket")
	return fs, socket
}

// controlFailed explains a failed call; exit code 1.
func controlFailed(stderr io.Writer, cmd string, err error) int {
	if errors.Is(err, nodeagent.ErrAgentDown) {
		fmt.Fprintf(stderr, "%s: %v.\nStart the node, or run `ktms-node doctor` to see why it is not running.\n", cmd, err)
		return 1
	}
	fmt.Fprintf(stderr, "%s: %v\n", cmd, err)
	return 1
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	fs, socket := controlFlags("status", stderr)
	asJSON := fs.Bool("json", false, "Print the status as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s, err := nodeagent.NewControlClient(*socket).Status(context.Background())
	if err != nil {
		return controlFailed(stderr, "status", err)
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(s)
		return 0
	}
	printStatus(stdout, s, time.Now())
	return 0
}

// runPause is `level 0`: pause [DURATION] [--until-next].
func runPause(args []string, stdout, stderr io.Writer) int {
	fs, socket := controlFlags("pause", stderr)
	untilNext := fs.Bool("until-next", false, "Pause until the schedule's next switch")
	warm := fs.Bool("warm", false, "Load the models again before the pause ends")
	now := fs.Bool("now", false, "Also cut the requests running now (their callers get an error)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: ktms-node pause [DURATION | --until-next] [--now] [--warm]   (default 10m)")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return 2
	}
	req := nodeagent.OverrideRequest{Level: 0, Warm: *warm, AbortInflight: *now, UntilNextWindow: *untilNext}
	switch {
	case fs.NArg() > 1:
		fs.Usage()
		return 2
	case fs.NArg() == 1 && *untilNext:
		fmt.Fprintln(stderr, "pause: give a duration or --until-next, not both")
		return 2
	case fs.NArg() == 1:
		req.For = fs.Arg(0)
	case !*untilNext:
		req.For = "10m"
	}
	return setOverride(stdout, stderr, "pause", *socket, req)
}

// runLevel sets a level by hand: level PERCENT --for DURATION | --until-next.
func runLevel(args []string, stdout, stderr io.Writer) int {
	fs, socket := controlFlags("level", stderr)
	dur := fs.String("for", "", "How long the level holds, e.g. 1h")
	untilNext := fs.Bool("until-next", false, "Hold it until the schedule's next switch")
	warm := fs.Bool("warm", false, "At 0%: load the models again before it ends")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: ktms-node level PERCENT (--for DURATION | --until-next)")
		fs.PrintDefaults()
	}
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 1 || (*dur == "") == !*untilNext {
		fs.Usage()
		return 2
	}
	pct, err := strconv.Atoi(strings.TrimSuffix(fs.Arg(0), "%"))
	if err != nil || pct < 0 || pct > 100 {
		fmt.Fprintf(stderr, "level: %q is not a level from 0 to 100\n", fs.Arg(0))
		return 2
	}
	req := nodeagent.OverrideRequest{Level: uint32(pct), For: *dur, UntilNextWindow: *untilNext, Warm: *warm}
	return setOverride(stdout, stderr, "level", *socket, req)
}

func setOverride(stdout, stderr io.Writer, cmd, socket string, req nodeagent.OverrideRequest) int {
	res, err := nodeagent.NewControlClient(socket).SetOverride(context.Background(), req)
	if err != nil {
		return controlFailed(stderr, cmd, err)
	}
	if res.Aborted > 0 {
		fmt.Fprintf(stdout, "Cut %d running request(s).\n", res.Aborted)
	}
	printStatus(stdout, res.Status, time.Now())
	return 0
}

func runResume(args []string, stdout, stderr io.Writer) int {
	fs, socket := controlFlags("resume", stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s, err := nodeagent.NewControlClient(*socket).ClearOverride(context.Background())
	if err != nil {
		return controlFailed(stderr, "resume", err)
	}
	printStatus(stdout, s, time.Now())
	if s.Level.Effective == 0 && s.Level.Next != nil {
		fmt.Fprintf(stdout, "The schedule pauses the node now, until %s.\n", clock(s.Level.Next.At, time.Now()))
	}
	return 0
}

// reorder moves flags ahead of positional arguments: `pause 10m --now` reads
// like `pause --now 10m`, which is all the flag package accepts.
func reorder(fs *flag.FlagSet, args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			rest = append(rest, a)
			continue
		}
		flags = append(flags, a)
		name, _, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		f := fs.Lookup(name)
		if f == nil || hasValue {
			continue
		}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); !(ok && b.IsBoolFlag()) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, rest...)
}

func printStatus(w io.Writer, s *nodeagent.Status, now time.Time) {
	conn := "registered"
	switch {
	case !s.Connection.Registered:
		conn = "not registered yet"
	case s.Connection.LastError != "":
		conn = "proxy unreachable: " + s.Connection.LastError
	case !s.Connection.LastHeartbeatAt.IsZero():
		conn = fmt.Sprintf("registered, heartbeat %s ago", now.Sub(s.Connection.LastHeartbeatAt).Round(time.Second))
	}
	fmt.Fprintf(w, "Node %s -> %s (%s)\n", s.NodeID, s.Proxy, conn)
	fmt.Fprintf(w, "Level %d%% %s\n", s.Level.Effective, levelOrigin(s, now))
	fmt.Fprintf(w, "State %s", s.State)
	if s.State == nodeagent.StatePausing && s.Requests.ProxyActive > 0 {
		fmt.Fprintf(w, ": waiting for %d proxy request(s) to finish", s.Requests.ProxyActive)
	}
	fmt.Fprintln(w)
	if len(s.Models) > 0 {
		var ms []string
		for _, m := range s.Models {
			if m.Warm {
				ms = append(ms, m.ID+" (loaded)")
			} else {
				ms = append(ms, m.ID)
			}
		}
		fmt.Fprintf(w, "Models %s\n", strings.Join(ms, ", "))
	}
	if g := s.GPU; g != nil {
		fmt.Fprintf(w, "GPU %.0f%%, %d°C, VRAM %.1f/%.1f GB free\n", g.UtilPercent, g.TempC, float64(g.VRAMFreeMB)/1024, float64(g.VRAMTotalMB)/1024)
	}
	if s.ScheduleError != "" {
		fmt.Fprintf(w, "Schedule file invalid, the previous one holds: %s\n", s.ScheduleError)
	}
	if !s.CapacityApplied && s.Connection.Registered && s.Level.Effective < 100 {
		fmt.Fprintln(w, "The proxy predates levels and routes here at 100%; at 0% the gateway still refuses requests.")
	}
}

// levelOrigin says where the level comes from and what follows it.
func levelOrigin(s *nodeagent.Status, now time.Time) string {
	var out string
	switch s.Level.Source {
	case nodeagent.SourceOverride:
		out = "(set by hand"
		if o := s.Level.Override; o != nil {
			out += " until " + clock(o.Until, now)
		}
	case nodeagent.SourceDefault:
		out = "(no schedule"
	default:
		out = "(schedule"
	}
	if n := s.Level.Next; n != nil {
		out += fmt.Sprintf(", then %d%% at %s", n.Level, clock(n.At, now))
	}
	return out + ")"
}

func clock(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if t.YearDay() != now.YearDay() || t.Year() != now.Year() {
		return t.Format("Mon 15:04")
	}
	return t.Format("15:04")
}
