package nodeagent

import (
	"fmt"
	"strings"
	"time"
)

var dayNames = [7]string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

// Window is one stretch of a day at one level; To may be "24:00".
type Window struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Level uint32 `json:"level"`
}

// DayWindows lists the windows rules set on one day; the rest of it runs at 100%.
type DayWindows struct {
	Day     string   `json:"day"`
	Windows []Window `json:"windows"`
}

// Switch is a moment the level changes.
type Switch struct {
	At    time.Time `json:"at"`
	Level uint32    `json:"level"`
}

// Week returns the level of every minute of the week, Monday 00:00 first;
// -1 marks a minute no rule covers. A nil schedule covers none.
func (s *Schedule) Week() []int8 {
	out := make([]int8, minutesPerWeek)
	if s == nil {
		for i := range out {
			out[i] = uncovered
		}
		return out
	}
	copy(out, s.level[:])
	return out
}

// Days splits the week into each day's windows, as an editor shows them.
func (s *Schedule) Days() []DayWindows {
	week := s.Week()
	out := make([]DayWindows, 7)
	for d := range 7 {
		out[d] = DayWindows{Day: dayNames[d], Windows: dayWindows(week, d)}
	}
	return out
}

// Upcoming lists the next n level switches after now.
func (s *Schedule) Upcoming(now time.Time, n int) []Switch {
	var out []Switch
	t := now
	for len(out) < n {
		at, level, ok := s.Next(t)
		if !ok {
			break
		}
		out = append(out, Switch{At: at, Level: level})
		t = at
	}
	return out
}

func dayWindows(week []int8, d int) []Window {
	out := []Window{} // an empty day is [] in JSON, not null
	base := d * minutesPerDay
	for m := 0; m < minutesPerDay; {
		l := week[base+m]
		end := m + 1
		for end < minutesPerDay && week[base+end] == l {
			end++
		}
		if l != uncovered {
			out = append(out, Window{From: clockName(m), To: clockName(end), Level: uint32(l)})
		}
		m = end
	}
	return out
}

func clockName(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

// FormatSchedule writes the rules for a week of levels (-1 = no rule), one
// line per window; days with the same windows share a line. Parsing the
// result gives the same week back.
func FormatSchedule(week []int8) (string, error) {
	if len(week) != minutesPerWeek {
		return "", fmt.Errorf("want %d minutes, got %d", minutesPerWeek, len(week))
	}
	for i, l := range week {
		if l < uncovered || l > 100 {
			return "", fmt.Errorf("bad level %d at %s", l, minuteName(i))
		}
	}
	type group struct {
		days    []int
		windows []Window
	}
	var groups []*group
	byKey := map[string]*group{}
	for d := range 7 {
		ws := dayWindows(week, d)
		if len(ws) == 0 {
			continue
		}
		key := fmt.Sprint(ws)
		g := byKey[key]
		if g == nil {
			g = &group{windows: ws}
			byKey[key] = g
			groups = append(groups, g)
		}
		g.days = append(g.days, d)
	}
	var b strings.Builder
	for _, g := range groups {
		days := daySpec(g.days)
		for _, w := range g.windows {
			if days != "" {
				b.WriteString(days + " ")
			}
			fmt.Fprintf(&b, "%s-%s %d\n", w.From, w.To, w.Level)
		}
	}
	return b.String(), nil
}

// daySpec names ascending days, runs as ranges ("mon-fri,sun"); all seven is "".
func daySpec(days []int) string {
	if len(days) == 7 {
		return ""
	}
	var parts []string
	for i := 0; i < len(days); {
		j := i
		for j+1 < len(days) && days[j+1] == days[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, dayNames[days[i]])
		} else {
			parts = append(parts, dayNames[days[i]]+"-"+dayNames[days[j]])
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}
