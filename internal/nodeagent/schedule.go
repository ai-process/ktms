package nodeagent

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	minutesPerDay  = 24 * 60
	minutesPerWeek = 7 * minutesPerDay
	uncovered      = -1
)

var weekdays = map[string]int{"mon": 0, "tue": 1, "wed": 2, "thu": 3, "fri": 4, "sat": 5, "sun": 6}

// Schedule is the node's capacity level for every minute of the week, in the
// machine's local time. Minutes no rule covers run at 100%.
type Schedule struct {
	level [minutesPerWeek]int8
	loc   *time.Location // nil = the machine's zone; tests pin one
}

// ParseSchedule reads rules of the form "[days] HH:MM-HH:MM level", one per
// line; "#" starts a comment. A window past midnight belongs to the day it
// starts on. Overlapping rules are an error.
func ParseSchedule(r io.Reader) (*Schedule, error) {
	s := &Schedule{}
	var owner [minutesPerWeek]int // line that set each minute, for overlap errors
	for i := range s.level {
		s.level[i] = uncovered
	}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		days := []int{0, 1, 2, 3, 4, 5, 6}
		if len(fields) == 3 {
			var err error
			if days, err = parseDays(fields[0]); err != nil {
				return nil, fmt.Errorf("line %d: %w", n, err)
			}
			fields = fields[1:]
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: want \"[days] HH:MM-HH:MM level\", got %q", n, strings.TrimSpace(line))
		}
		start, end, err := parseWindow(fields[0])
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		level, err := parseLevel(fields[1])
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		for _, d := range days {
			for m := start; m < end; m++ {
				i := (d*minutesPerDay + m) % minutesPerWeek
				if s.level[i] != uncovered {
					return nil, fmt.Errorf("line %d overlaps line %d at %s", n, owner[i], minuteName(i))
				}
				s.level[i] = int8(level)
				owner[i] = n
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return s, nil
}

// LoadSchedule parses the file at path. A missing file is no schedule: nil, nil.
func LoadSchedule(path string) (*Schedule, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s, err := ParseSchedule(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func parseDays(spec string) ([]int, error) {
	var out []int
	seen := map[int]bool{}
	for _, part := range strings.Split(strings.ToLower(spec), ",") {
		from, to, isRange := strings.Cut(part, "-")
		a, ok := weekdays[from]
		if !ok {
			return nil, fmt.Errorf("unknown day %q (want mon..sun)", from)
		}
		b := a
		if isRange {
			if b, ok = weekdays[to]; !ok {
				return nil, fmt.Errorf("unknown day %q (want mon..sun)", to)
			}
		}
		for d := a; ; d = (d + 1) % 7 { // fri-mon wraps over the weekend
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
			if d == b {
				break
			}
		}
	}
	return out, nil
}

// parseWindow returns minutes from the start day's midnight; end may run
// into the next day.
func parseWindow(spec string) (start, end int, err error) {
	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, fmt.Errorf("want HH:MM-HH:MM, got %q", spec)
	}
	if start, err = parseClock(a, false); err != nil {
		return 0, 0, err
	}
	if end, err = parseClock(b, true); err != nil {
		return 0, 0, err
	}
	if end == start {
		return 0, 0, fmt.Errorf("window %q is empty (a whole day is 00:00-24:00)", spec)
	}
	if end < start {
		end += minutesPerDay
	}
	return start, end, nil
}

func parseClock(s string, allow24 bool) (int, error) {
	h, m, ok := strings.Cut(s, ":")
	if !ok || len(h) == 0 || len(h) > 2 || len(m) != 2 {
		return 0, fmt.Errorf("bad time %q (want HH:MM)", s)
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("bad time %q (want HH:MM)", s)
	}
	if hh == 24 && mm == 0 && allow24 {
		return minutesPerDay, nil
	}
	if hh > 23 {
		return 0, fmt.Errorf("bad time %q (hours run 00-23, or 24:00 as an end)", s)
	}
	return hh*60 + mm, nil
}

func parseLevel(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSuffix(s, "%"))
	if err != nil || n < 0 || n > 100 {
		return 0, fmt.Errorf("bad level %q (want 0-100)", s)
	}
	return n, nil
}

func minuteName(i int) string {
	names := [7]string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}
	m := i % minutesPerDay
	return fmt.Sprintf("%s %02d:%02d", names[i/minutesPerDay], m/60, m%60)
}

// weekMinute places t on the schedule by its local wall clock, so DST shifts
// move the switch with the clock rather than an hour off it.
func (s *Schedule) weekMinute(t time.Time) int {
	t = s.local(t)
	day := (int(t.Weekday()) + 6) % 7 // Monday first
	return day*minutesPerDay + t.Hour()*60 + t.Minute()
}

// Level is the capacity at t; a nil schedule is always 100%.
func (s *Schedule) Level(t time.Time) uint32 {
	if s == nil {
		return 100
	}
	if l := s.level[s.weekMinute(t)]; l != uncovered {
		return uint32(l)
	}
	return 100
}

// Next returns when the level next changes and to what; ok is false when it
// never does. It steps in real minutes, so DST gaps and repeats count right.
func (s *Schedule) Next(t time.Time) (at time.Time, level uint32, ok bool) {
	cur := s.Level(t)
	step := t.Truncate(time.Minute)
	for range minutesPerWeek + 60 { // an hour of slack for a DST week
		step = step.Add(time.Minute)
		if l := s.Level(step); l != cur {
			return step, l, true
		}
	}
	return time.Time{}, cur, false
}

// Status is the one-line summary the CLI, the doctor and the installer print.
func (s *Schedule) Status(now time.Time) string {
	if s == nil {
		return "no schedule, 100% all the time"
	}
	next := s.describeNext(now)
	if next == "none" {
		return fmt.Sprintf("now %d%%, no switches", s.Level(now))
	}
	return fmt.Sprintf("now %d%%, next %s", s.Level(now), next)
}

// describeNext renders the next switch for logs and status lines.
func (s *Schedule) describeNext(now time.Time) string {
	at, level, ok := s.Next(now)
	if !ok {
		return "none"
	}
	at, now = s.local(at), s.local(now)
	format := "15:04"
	if at.YearDay() != now.YearDay() || at.Year() != now.Year() {
		format = "Mon 15:04"
	}
	return fmt.Sprintf("%s %d%%", at.Format(format), level)
}

func (s *Schedule) local(t time.Time) time.Time {
	if s != nil && s.loc != nil {
		return t.In(s.loc)
	}
	return t.Local()
}
