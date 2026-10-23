package nodeagent

import (
	"strings"
	"testing"
	"time"
)

func mustSchedule(t *testing.T, text string, loc *time.Location) *Schedule {
	t.Helper()
	s, err := ParseSchedule(strings.NewReader(text))
	if err != nil {
		t.Fatalf("ParseSchedule: %v", err)
	}
	s.loc = loc
	return s
}

func berlin(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	return loc
}

const daySchedule = `
# [days] HH:MM-HH:MM level
23:00-07:00 100
07:00-10:00 50%
10:00-23:00 0   # owner at work
`

func TestScheduleLevels(t *testing.T) {
	loc := berlin(t)
	at := func(day, clock string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", day+" "+clock, loc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	// 2026-10-19 is a Monday.
	cases := []struct {
		name, text, day, clock string
		want                   uint32
	}{
		{"night window past midnight", daySchedule, "2026-10-20", "03:00", 100},
		{"morning half", daySchedule, "2026-10-20", "07:00", 50},
		{"last minute of half", daySchedule, "2026-10-20", "09:59", 50},
		{"working hours", daySchedule, "2026-10-20", "10:00", 0},
		{"evening switch", daySchedule, "2026-10-20", "23:00", 100},
		{"uncovered is full", "10:00-12:00 0", "2026-10-20", "12:00", 100},
		{"weekday range", "mon-fri 09:00-18:00 0", "2026-10-23", "12:00", 0},
		{"weekend skipped by range", "mon-fri 09:00-18:00 0", "2026-10-24", "12:00", 100},
		{"weekend list", "sat,sun 00:00-24:00 30", "2026-10-25", "23:59", 30},
		{"wrapping range", "fri-mon 08:00-09:00 0", "2026-10-25", "08:30", 0},
		{"sunday night wraps to monday", "sun 22:00-02:00 0", "2026-10-26", "01:00", 0},
		{"whole day by 24:00", "00:00-24:00 20", "2026-10-21", "23:59", 20},
		{"empty file", "", "2026-10-21", "12:00", 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mustSchedule(t, c.text, loc).Level(at(c.day, c.clock)); got != c.want {
				t.Errorf("Level = %d, want %d", got, c.want)
			}
		})
	}
	var none *Schedule
	if none.Level(time.Now()) != 100 {
		t.Error("nil schedule must be 100%")
	}
}

func TestScheduleErrors(t *testing.T) {
	for _, c := range []struct{ name, text, want string }{
		{"overlap", "07:00-10:00 50\n09:00-11:00 0", "line 2 overlaps line 1"},
		{"overlap past midnight", "23:00-07:00 100\nmon 06:00-08:00 0", "line 2 overlaps line 1 at mon 06:00"},
		{"overlap of a day with itself", "mon,mon-tue 10:00-11:00 0", ""},
		{"bad day", "monday 10:00-11:00 0", "line 1: unknown day"},
		{"bad time", "25:00-26:00 0", "line 1: bad time"},
		{"bad minutes", "10:7-11:00 0", "bad time"},
		{"24:00 as start", "24:00-01:00 0", "bad time"},
		{"empty window", "10:00-10:00 0", "is empty"},
		{"level above 100", "10:00-11:00 150", "bad level"},
		{"negative level", "10:00-11:00 -5", "bad level"},
		{"missing level", "10:00-11:00", "want \"[days] HH:MM-HH:MM level\""},
		{"junk", "\n\nhello world", "line 3"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseSchedule(strings.NewReader(c.text))
			if c.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
		})
	}
}

func TestScheduleNextAcrossDST(t *testing.T) {
	loc := berlin(t)
	s := mustSchedule(t, "01:00-04:00 0", loc)

	// 2026-03-29: clocks jump from 02:00 to 03:00; the pause still ends at 04:00 wall time.
	from := time.Date(2026, 3, 29, 1, 30, 0, 0, loc)
	at, level, ok := s.Next(from)
	if !ok || level != 100 || at.In(loc).Hour() != 4 || at.Sub(from) != 90*time.Minute {
		t.Errorf("spring: next = %v %d%% (%v later), want 04:00 100%% 1h30m later", at.In(loc), level, at.Sub(from))
	}
	// 2026-10-25: 03:00 falls back to 02:00, so the window lasts an hour longer.
	from = time.Date(2026, 10, 25, 1, 30, 0, 0, loc)
	at, _, _ = s.Next(from)
	if at.In(loc).Hour() != 4 || at.Sub(from) != 3*time.Hour+30*time.Minute {
		t.Errorf("autumn: next = %v (%v later), want 04:00 3h30m later", at.In(loc), at.Sub(from))
	}
	if s.Level(time.Date(2026, 10, 25, 2, 30, 0, 0, loc).Add(time.Hour)) != 0 {
		t.Error("the repeated 02:30 must still be paused")
	}
}

func TestScheduleNext(t *testing.T) {
	loc := berlin(t)
	s := mustSchedule(t, daySchedule, loc)
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, loc)
	if got := s.describeNext(now); got != "23:00 100%" {
		t.Errorf("describeNext = %q", got)
	}
	if got := s.describeNext(time.Date(2026, 10, 20, 23, 30, 0, 0, loc)); got != "Wed 07:00 50%" {
		t.Errorf("describeNext past midnight = %q", got)
	}
	if _, _, ok := mustSchedule(t, "00:00-24:00 40", loc).Next(now); ok {
		t.Error("a constant schedule has no next switch")
	}
}
