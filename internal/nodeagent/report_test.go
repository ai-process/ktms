package nodeagent

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestReportByHour(t *testing.T) {
	st, loc := pinnedStore(t)
	at := func(h, m, s int) time.Time { return time.Date(2026, 10, 24, h, m, s, 0, loc) }
	if err := st.AppendRequests([]RequestRecord{
		// Overlapping requests count their union: 90 s busy, not 120 s.
		{RequestID: "a", StartedAt: at(10, 0, 0), DurationMs: 60_000, Model: "m", Outcome: "ok", InputTokens: 100, OutputTokens: 50, TTFTMs: 300},
		{RequestID: "b", StartedAt: at(10, 0, 30), DurationMs: 60_000, Model: "m", Outcome: "ok", InputTokens: 10, OutputTokens: 50, TTFTMs: 500},
		// Crosses into 11:00: 30 s in each hour.
		{RequestID: "c", StartedAt: at(10, 59, 30), DurationMs: 60_000, Model: "big", Outcome: "error", ErrorCode: "http_503", CooldownMs: 60_000},
	}); err != nil {
		t.Fatal(err)
	}
	for m := range 60 {
		state := SampleStateServing
		if m >= 30 {
			state = SampleStatePaused
		}
		if err := st.AppendSample(Sample{At: at(10, m, 5), State: state}); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := BuildReport(st, at(10, 0, 0), at(12, 0, 0), at(12, 0, 0), "hour")
	if err != nil || len(rep.Rows) != 2 {
		t.Fatalf("report = %+v (%v), want two hours", rep, err)
	}
	h10, h11 := rep.Rows[0], rep.Rows[1]
	if h10.Requests != 3 || h10.Errors != 1 || h10.BusyMs != 120_000 || h10.InputTokens != 110 {
		t.Errorf("10:00 = %+v, want 3 requests, 1 error, 90 s + 30 s busy", h10)
	}
	if h10.PausedShare != 0.5 || h10.NoDataMin != 0 || h10.TTFTp50Ms != 300 || h10.TPS == 0 {
		t.Errorf("10:00 paused %.2f, no data %d, ttft %d, tps %.1f", h10.PausedShare, h10.NoDataMin, h10.TTFTp50Ms, h10.TPS)
	}
	if h11.Requests != 0 || h11.BusyMs != 30_000 || h11.PausedShare != -1 || h11.NoDataMin != 60 {
		t.Errorf("11:00 = %+v, want only c's busy tail (it is counted where it started), no samples", h11)
	}
	if h10.Requests+h11.Requests != rep.Total.Requests || h10.InputTokens+h11.InputTokens != rep.Total.InputTokens {
		t.Errorf("rows %d+%d requests do not add up to the total %d", h10.Requests, h11.Requests, rep.Total.Requests)
	}

	// A request that began before --since still makes the start of the range busy.
	late, err := BuildReport(st, at(11, 0, 0), at(12, 0, 0), at(12, 0, 0), "hour")
	if err != nil || late.Total.Requests != 0 || late.Total.BusyMs != 30_000 {
		t.Errorf("from 11:00: %+v (%v), want no requests and c's 30 s busy", late.Total, err)
	}
	if rep.Total.Requests != 3 || rep.Total.BusyMs != 150_000 || rep.Total.CooldownMs != 60_000 {
		t.Errorf("total = %+v", rep.Total)
	}

	byModel, err := BuildReport(st, at(10, 0, 0), at(12, 0, 0), at(12, 0, 0), "model")
	if err != nil || len(byModel.Rows) != 2 || byModel.Rows[0].Key != "big" || byModel.Rows[1].Requests != 2 {
		t.Fatalf("by model = %+v (%v)", byModel.Rows, err)
	}
	if _, err := BuildReport(st, at(10, 0, 0), at(12, 0, 0), at(12, 0, 0), "week"); err == nil {
		t.Error("unknown --by accepted")
	}
}

// One slot at 50%: each request is followed by an equal cooldown, so the node
// is busy half the time (ai-process/docs#59).
func TestReportHalfLevelIsHalfBusy(t *testing.T) {
	st, loc := pinnedStore(t)
	start := time.Date(2026, 10, 24, 10, 0, 0, 0, loc)
	var recs []RequestRecord
	for i, t0 := 0, start; t0.Before(start.Add(time.Hour)); i, t0 = i+1, t0.Add(40*time.Second) {
		recs = append(recs, RequestRecord{RequestID: fmt.Sprint("r", i), StartedAt: t0, DurationMs: 20_000, CooldownMs: 20_000, Outcome: "ok", Level: 50})
	}
	if err := st.AppendRequests(recs); err != nil {
		t.Fatal(err)
	}
	rep, err := BuildReport(st, start, start.Add(time.Hour), start.Add(time.Hour), "hour")
	if err != nil {
		t.Fatal(err)
	}
	if b := rep.Total.BusyShare; b < 0.49 || b > 0.51 {
		t.Errorf("busy share = %.3f, want 0.50", b)
	}
}

func TestWriteReportFormats(t *testing.T) {
	st, loc := pinnedStore(t)
	at := time.Date(2026, 10, 24, 10, 0, 0, 0, loc)
	if err := st.AppendRequests([]RequestRecord{{RequestID: "a", StartedAt: at, DurationMs: 1000, Model: "m", Outcome: "ok"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendGap(Gap{From: at.Add(-time.Hour), Until: at}); err != nil {
		t.Fatal(err)
	}
	rep, err := BuildReport(st, at.Add(-2*time.Hour), at.Add(time.Hour), at.Add(time.Hour), "hour")
	if err != nil {
		t.Fatal(err)
	}
	var table bytes.Buffer
	if err := WriteReport(&table, rep, "table"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Period", "total", "Missing: the proxy no longer had requests", "No data"} {
		if !strings.Contains(table.String(), want) {
			t.Errorf("table lacks %q:\n%s", want, table.String())
		}
	}
	var js bytes.Buffer
	if err := WriteReport(&js, rep, "json"); err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(js.Bytes(), &back); err != nil || back.Total.Requests != 1 || len(back.Gaps) != 1 {
		t.Errorf("json = %s (%v)", js.String(), err)
	}
	var cs bytes.Buffer
	if err := WriteReport(&cs, rep, "csv"); err != nil {
		t.Fatal(err)
	}
	if rows, err := csv.NewReader(&cs).ReadAll(); err != nil || len(rows) != len(rep.Rows)+2 || rows[0][0] != "period" {
		t.Errorf("csv rows = %d (%v)", len(rows), err)
	}
	byModel, _ := BuildReport(st, at.Add(-2*time.Hour), at.Add(time.Hour), at.Add(time.Hour), "model")
	var mcs bytes.Buffer
	if err := WriteReport(&mcs, byModel, "csv"); err != nil {
		t.Fatal(err)
	}
	if rows, _ := csv.NewReader(&mcs).ReadAll(); len(rows) < 2 || rows[1][8] != "" {
		t.Errorf("model rows must leave the paused cell empty, got %q", rows[1][8])
	}
	if err := WriteReport(&cs, rep, "xml"); err == nil {
		t.Error("unknown format accepted")
	}
}

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.Local)
	for in, want := range map[string]time.Time{
		"24h":              now.Add(-24 * time.Hour),
		"7d":               now.AddDate(0, 0, -7),
		"2026-10-01":       time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local),
		"2026-10-01T15:04": time.Date(2026, 10, 1, 15, 4, 0, 0, time.Local),
	} {
		if got, err := ParseWhen(in, now); err != nil || !got.Equal(want) {
			t.Errorf("ParseWhen(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseWhen("yesterday", now); err == nil {
		t.Error("ParseWhen accepted junk")
	}
}

func TestTailRequests(t *testing.T) {
	st, _ := pinnedStore(t)
	now := time.Now()
	if err := st.AppendRequests([]RequestRecord{{RequestID: "a", StartedAt: now, DurationMs: 1500, Model: "tiny:1b", Kind: "chat", Outcome: "error", ErrorCode: "http_503", InputTokens: 12, OutputTokens: 3, Level: 50}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	if err := TailRequests(ctx, st, now.Add(-time.Minute), &out, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	line := out.String()
	if strings.Count(line, "tiny:1b") != 1 || !strings.Contains(line, "error http_503") || !strings.Contains(line, "12→3 tok") {
		t.Errorf("tail printed %q, want the request once", line)
	}
}

func TestReportNamesTheRepeatedDSTHour(t *testing.T) {
	st, loc := pinnedStore(t)
	from := time.Date(2026, 10, 25, 1, 0, 0, 0, loc)
	rep, err := BuildReport(st, from, from.Add(4*time.Hour), from.Add(4*time.Hour), "hour")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, r := range rep.Rows {
		if keys[r.Key] {
			t.Fatalf("hour %q appears twice: %+v", r.Key, rep.Rows)
		}
		keys[r.Key] = true
	}
	if len(rep.Rows) != 4 {
		t.Errorf("rows = %d, want four real hours across the fall-back", len(rep.Rows))
	}
}
