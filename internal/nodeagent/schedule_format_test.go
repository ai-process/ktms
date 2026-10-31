package nodeagent

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFormatScheduleRoundTrip(t *testing.T) {
	for _, text := range []string{
		"",
		"00:00-24:00 50\n",
		"mon-fri 09:00-18:00 30\nmon-fri 18:00-23:30 0\nsat,sun 10:00-12:00 80\n",
		"fri 22:00-02:00 0\n",             // past midnight into Saturday
		"sun 23:00-01:30 40\n",            // past the week's end into Monday
		"mon,wed,fri 12:01-12:02 7\n",     // minute precision
		"fri-mon 08:00-09:00 0 # comment", // wraps over the weekend
	} {
		s, err := ParseSchedule(strings.NewReader(text))
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		out, err := FormatSchedule(s.Week())
		if err != nil {
			t.Fatalf("%q: format: %v", text, err)
		}
		back, err := ParseSchedule(strings.NewReader(out))
		if err != nil {
			t.Fatalf("%q: formatted %q does not parse: %v", text, out, err)
		}
		if !reflect.DeepEqual(back.Week(), s.Week()) {
			t.Errorf("%q: round trip through %q changed the week", text, out)
		}
	}
}

func TestFormatScheduleGroupsDays(t *testing.T) {
	s, err := ParseSchedule(strings.NewReader("mon-fri 09:00-18:00 30\nsun 00:00-24:00 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := FormatSchedule(s.Week())
	want := "mon-fri 09:00-18:00 30\nsun 00:00-24:00 0\n"
	if out != want {
		t.Errorf("FormatSchedule = %q, want %q", out, want)
	}
	all, _ := ParseSchedule(strings.NewReader("09:00-10:00 0\n"))
	if out, _ := FormatSchedule(all.Week()); out != "09:00-10:00 0\n" {
		t.Errorf("every day = %q, want no day field", out)
	}
	if _, err := FormatSchedule(make([]int8, 10)); err == nil {
		t.Error("a short week was formatted")
	}
}

func TestScheduleDaysAndUpcoming(t *testing.T) {
	s, _ := ParseSchedule(strings.NewReader("mon 09:00-18:00 30\n"))
	days := s.Days()
	if len(days) != 7 || days[0].Day != "mon" || !reflect.DeepEqual(days[0].Windows, []Window{{From: "09:00", To: "18:00", Level: 30}}) || len(days[1].Windows) != 0 {
		t.Fatalf("Days = %+v", days)
	}
	s.loc = time.UTC
	mon := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC) // a Monday
	up := s.Upcoming(mon, 3)
	if len(up) != 3 || !up[0].At.Equal(mon.Add(time.Hour)) || up[0].Level != 30 || up[1].Level != 100 || !up[2].At.Equal(mon.Add(7*24*time.Hour+time.Hour)) {
		t.Fatalf("Upcoming = %+v", up)
	}
	var none *Schedule
	if len(none.Upcoming(mon, 3)) != 0 || len(none.Days()[0].Windows) != 0 {
		t.Error("no schedule has switches or windows")
	}
}
