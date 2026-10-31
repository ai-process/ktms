// Package nodetray is the desktop tray's logic, kept apart from the GUI so it
// is tested without one: what the menu bar shows for an agent status, the
// notifications state changes raise, the icons and the settings page.
package nodetray

import (
	"fmt"
	"strings"
	"time"

	"github.com/ai-process/ktms/internal/nodeagent"
)

// IconKind picks the menu bar icon.
type IconKind int

const (
	IconFull    IconKind = iota // 100%
	IconPartial                 // 1-99%
	IconPaused
	IconWaiting // paused, proxy requests still running
	IconWarn    // agent silent or proxy unreachable
)

// View is everything the menu shows for one status.
type View struct {
	Icon    IconKind
	Title   string // next to the icon: "80%", "⏸ 9:41", "⏸ until 07:00", "⏳ 2"
	Tooltip string
	Header  string // "Keystone node · mac-studio"
	Level   string // "Running at 80% (schedule) · 18:00 → 0%"
	Detail  string // "2 requests · qwen3:32b loaded · GPU 87%"
	Notice  string // a problem worth a line of its own; "" for none

	Connected    bool   // the agent answers: the controls apply
	UntilNext    string // "Pause until the next switch (18:00)"; "" hides it
	StopNow      string // "Stop now (cuts 2 requests)"; "" hides it
	CanResume    bool   // a manual level is in force
	ScheduleKTMS bool   // the "Schedule set by KTMS" checkbox
	Ticking      bool   // the title counts down: redraw every second
}

// StopNow is the request behind "Stop now": it cuts the running requests and
// keeps a manual pause that is already in force until its own end.
func StopNow(s *nodeagent.Status, now time.Time) nodeagent.OverrideRequest {
	req := nodeagent.OverrideRequest{Level: 0, For: "10m", AbortInflight: true}
	if o := s.Level.Override; s.Level.Source == nodeagent.SourceOverride && o != nil && o.Level == 0 && o.Until.Sub(now) > nodeagent.MinOverride {
		req.For, req.Until = "", o.Until
	}
	return req
}

// Build renders a status; err is the last failure to reach the agent.
func Build(s *nodeagent.Status, err error, now time.Time) View {
	v := View{Header: "Keystone node"}
	if err != nil || s == nil {
		v.Icon, v.Title = IconWarn, ""
		v.Level = "The node agent does not answer"
		v.Tooltip = v.Level
		if err != nil {
			v.Detail = shorten(err.Error(), 80)
		}
		return v
	}
	v.Connected = true
	v.Header += " · " + s.NodeID
	v.ScheduleKTMS = s.ScheduleSource == nodeagent.ScheduleSourceKTMS
	lvl := s.Level.Effective

	switch {
	case lvl == 0 && s.Requests.ProxyActive > 0:
		v.Icon, v.Title = IconWaiting, fmt.Sprintf("⏳ %d", s.Requests.ProxyActive)
	case lvl == 0 && s.Level.Source == nodeagent.SourceOverride && s.Level.Override != nil:
		v.Icon, v.Title, v.Ticking = IconPaused, "⏸ "+countdown(s.Level.Override.Until.Sub(now)), true
	case lvl == 0 && s.Level.Next != nil:
		v.Icon, v.Title = IconPaused, "⏸ until "+clock(s.Level.Next.At, now)
	case lvl == 0:
		v.Icon, v.Title = IconPaused, "⏸"
	case lvl < 100:
		v.Icon, v.Title = IconPartial, fmt.Sprintf("%d%%", lvl)
	default:
		v.Icon, v.Title = IconFull, "100%"
	}
	if !s.Connection.Registered || s.Connection.LastError != "" {
		v.Icon = IconWarn
		v.Notice = "No connection to KTMS"
		if e := s.Connection.LastError; e != "" {
			v.Notice += ": " + shorten(e, 60)
		}
	}

	v.Level = levelLine(s, now)
	v.Detail = detailLine(s)
	v.Tooltip = v.Header + "\n" + v.Level
	if s.ScheduleError != "" && v.Notice == "" {
		v.Notice = "Schedule invalid, the previous one holds"
	}
	if !s.CapacityApplied && s.Connection.Registered && lvl < 100 && v.Notice == "" {
		v.Notice = "KTMS predates levels and routes here at 100%"
	}

	if n := s.Level.Next; n != nil && (lvl > 0 || s.Level.Source == nodeagent.SourceOverride) {
		v.UntilNext = "Pause until the next switch (" + clock(n.At, now) + ")"
		if n.At.Sub(now) > nodeagent.MaxOverride {
			v.UntilNext = "Pause for 24 hours" // the agent caps it there
		}
	}
	if s.CanAbort && s.Requests.ProxyActive > 0 {
		v.StopNow = fmt.Sprintf("Stop now (cuts %d request%s)", s.Requests.ProxyActive, plural(int(s.Requests.ProxyActive)))
	}
	v.CanResume = s.Level.Override != nil
	return v
}

func levelLine(s *nodeagent.Status, now time.Time) string {
	lvl := s.Level.Effective
	var what string
	switch {
	case lvl == 0 && s.Requests.ProxyActive > 0:
		what = fmt.Sprintf("Pausing: waiting for %d request%s", s.Requests.ProxyActive, plural(int(s.Requests.ProxyActive)))
	case lvl == 0:
		what = "Paused"
	default:
		what = fmt.Sprintf("Running at %d%%", lvl)
	}
	var src string
	switch s.Level.Source {
	case nodeagent.SourceOverride:
		src = "by hand"
		if o := s.Level.Override; o != nil {
			src += " until " + clock(o.Until, now)
		}
	case nodeagent.SourceKTMSOverride:
		src = "set in KTMS"
	case nodeagent.SourceKTMSSchedule:
		src = "KTMS schedule"
	case nodeagent.SourceDefault:
		src = "no schedule"
	default:
		src = "schedule"
	}
	out := what + " (" + src + ")"
	if n := s.Level.Next; n != nil {
		out += fmt.Sprintf(" · %s → %d%%", clock(n.At, now), n.Level)
	}
	return out
}

func detailLine(s *nodeagent.Status) string {
	var parts []string
	if n := s.Requests.ProxyActive; n > 0 {
		parts = append(parts, fmt.Sprintf("%d request%s", n, plural(int(n))))
	} else {
		parts = append(parts, "idle")
	}
	var warm []string
	for _, m := range s.Models {
		if m.Warm {
			warm = append(warm, m.ID)
		}
	}
	switch {
	case len(warm) > 0:
		parts = append(parts, strings.Join(warm, ", ")+" loaded")
	case len(s.Models) > 0:
		parts = append(parts, "models unloaded")
	}
	if g := s.GPU; g != nil {
		gpu := fmt.Sprintf("GPU %.0f%%", g.UtilPercent)
		if g.VRAMTotalMB > 0 {
			gpu += fmt.Sprintf(", VRAM %.1f/%.0f GB free", float64(g.VRAMFreeMB)/1024, float64(g.VRAMTotalMB)/1024)
		}
		parts = append(parts, gpu)
	}
	return strings.Join(parts, " · ")
}

// countdown renders a remaining time as m:ss, or h:mm:ss past an hour.
func countdown(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	h, m, sec := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	return fmt.Sprintf("%d:%02d", m, sec)
}

func clock(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if t.YearDay() != now.YearDay() || t.Year() != now.Year() {
		return t.Format("Mon 15:04")
	}
	return t.Format("15:04")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func shorten(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
