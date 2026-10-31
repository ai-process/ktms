package nodetray

import (
	"context"
	"os/exec"
	"runtime"
	"time"

	"github.com/ai-process/ktms/internal/nodeagent"
)

// silentAfter is how long the agent may not answer before the owner hears of it;
// a restart takes a few seconds and is not worth a notification.
const silentAfter = 30 * time.Second

// Notifier turns status changes into the notifications the owner asked for.
type Notifier struct {
	prev        *nodeagent.Status
	silentSince time.Time
	toldSilent  bool
}

// Next returns the messages the change from the last status to this one raises.
func (n *Notifier) Next(s *nodeagent.Status, err error, now time.Time) []string {
	var out []string
	if err != nil || s == nil {
		if n.silentSince.IsZero() {
			n.silentSince = now
		}
		if !n.toldSilent && n.prev != nil && now.Sub(n.silentSince) >= silentAfter {
			n.toldSilent = true
			out = append(out, "The node agent does not answer. Requests may not reach this machine.")
		}
		return out
	}
	if n.toldSilent {
		out = append(out, "The node agent answers again.")
	}
	n.silentSince, n.toldSilent = time.Time{}, false
	if p := n.prev; p != nil {
		// Only a pause that ran out: the owner who pressed Resume needs no news.
		if o := p.Level.Override; o != nil && p.Level.Effective == 0 && !now.Before(o.Until.Add(-5*time.Second)) &&
			s.Level.Source != nodeagent.SourceOverride && s.Level.Effective > 0 {
			out = append(out, "The pause is over: the node takes requests again.")
		}
		if p.ScheduleError == "" && s.ScheduleError != "" {
			out = append(out, "The schedule is invalid; the previous one stays in force. "+shorten(s.ScheduleError, 120))
		}
	}
	n.prev = s
	return out
}

// Notify shows a system notification; a desktop without a notifier stays quiet.
func Notify(ctx context.Context, msg string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		// Arguments, not script text: the message needs no AppleScript quoting.
		cmd = exec.CommandContext(ctx, "osascript", "-e", "on run argv", "-e",
			"display notification (item 1 of argv) with title (item 2 of argv)", "-e", "end run", msg, "Keystone node")
	default:
		cmd = exec.CommandContext(ctx, "notify-send", "--app-name=Keystone node", "Keystone node", msg)
	}
	return cmd.Run()
}
