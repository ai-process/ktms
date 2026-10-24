package nodeagent

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// ReportRow sums one period (or model) of the usage log.
type ReportRow struct {
	Key          string  `json:"key"`
	Requests     int     `json:"requests"`
	Errors       int     `json:"errors"` // outcome error or timeout
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	BusyMs       int64   `json:"busy_ms"` // time with at least one request running
	SpanMs       int64   `json:"span_ms"` // the period, up to now
	BusyShare    float64 `json:"busy"`    // BusyMs / SpanMs
	CooldownMs   int64   `json:"cooldown_ms"`
	PausedShare  float64 `json:"paused"`      // share of the agent's samples at 0%; -1 = no samples
	NoDataMin    int     `json:"no_data_min"` // minutes without a sample: the agent was not running
	Samples      int     `json:"samples"`
	TTFTp50Ms    int64   `json:"ttft_p50_ms,omitempty"`
	TTFTp95Ms    int64   `json:"ttft_p95_ms,omitempty"`
	TPS          float64 `json:"tps,omitempty"`
}

// Report is the usage log over [From, To) split by hour, day or model.
type Report struct {
	From  time.Time   `json:"from"`
	To    time.Time   `json:"to"`
	By    string      `json:"by"`
	Rows  []ReportRow `json:"rows"`
	Total ReportRow   `json:"total"`
	Gaps  []Gap       `json:"gaps,omitempty"`
}

// BuildReport reads the log in [from, to) and sums it by "hour", "day" or "model".
func BuildReport(st *StatsStore, from, to, now time.Time, by string) (*Report, error) {
	// Requests that began before the range still make its start busy.
	all, err := st.Requests(from.Add(-busyLookback), to)
	if err != nil {
		return nil, err
	}
	reqs := startedIn(all, from, to)
	samples, err := st.Samples(from, to)
	if err != nil {
		return nil, err
	}
	gaps, err := st.Gaps(from, to)
	if err != nil {
		return nil, err
	}
	end := minTime(to, now)
	rep := &Report{From: from, To: to, By: by, Gaps: gaps}
	rep.Total = summarize("total", reqs, all, samples, from, end)
	switch by {
	case "model":
		groups, busy := map[string][]RequestRecord{}, map[string][]RequestRecord{}
		for _, r := range reqs {
			groups[r.Model] = append(groups[r.Model], r)
		}
		for _, r := range all {
			busy[r.Model] = append(busy[r.Model], r)
		}
		models := make([]string, 0, len(groups))
		for m := range groups {
			models = append(models, m)
		}
		sort.Strings(models)
		for _, m := range models {
			row := summarize(m, groups[m], busy[m], nil, from, end)
			row.PausedShare, row.NoDataMin = -1, 0 // samples belong to the node, not a model
			rep.Rows = append(rep.Rows, row)
		}
	case "hour", "day":
		for start := st.periodStart(from, by); start.Before(end); {
			next := st.periodNext(start, by)
			a, b := maxTime(start, from), minTime(next, end)
			rep.Rows = append(rep.Rows, summarize(st.periodKey(start, by), startedIn(reqs, a, b), all, withinSamples(samples, a, b), a, b))
			start = next
		}
	default:
		return nil, fmt.Errorf("unknown --by %q (want hour, day or model)", by)
	}
	return rep, nil
}

// busyLookback is how far before a range requests are read for its busy time.
const busyLookback = 6 * time.Hour

// summarize counts the requests that started in the period and takes busy
// time from every request that overlaps it, so rows add up to the total.
func summarize(key string, reqs, busyReqs []RequestRecord, samples []Sample, from, to time.Time) ReportRow {
	row := ReportRow{Key: key, SpanMs: max(to.Sub(from).Milliseconds(), 0), PausedShare: -1}
	var ttfts []int64
	var tpsOut, tpsMs int64
	type span struct{ a, b time.Time }
	spans := make([]span, 0, len(busyReqs))
	for _, r := range reqs {
		row.Requests++
		if r.Outcome == "error" || r.Outcome == "timeout" { // refused or abandoned requests are not the node failing
			row.Errors++
		}
		row.InputTokens += r.InputTokens
		row.OutputTokens += r.OutputTokens
		row.CooldownMs += r.CooldownMs
		if r.TTFTMs > 0 {
			ttfts = append(ttfts, r.TTFTMs)
		}
		if r.OutputTokens > 0 && r.DurationMs > 0 {
			tpsOut += r.OutputTokens
			tpsMs += r.DurationMs
		}
	}
	for _, r := range busyReqs {
		a := maxTime(r.StartedAt, from)
		b := minTime(r.StartedAt.Add(time.Duration(r.DurationMs)*time.Millisecond), to)
		if b.After(a) {
			spans = append(spans, span{a, b})
		}
	}
	// Busy is the union of request spans: parallel requests do not count twice.
	sort.Slice(spans, func(i, j int) bool { return spans[i].a.Before(spans[j].a) })
	var cur span
	for i, s := range spans {
		switch {
		case i == 0:
			cur = s
		case s.a.After(cur.b):
			row.BusyMs += cur.b.Sub(cur.a).Milliseconds()
			cur = s
		case s.b.After(cur.b):
			cur.b = s.b
		}
	}
	if len(spans) > 0 {
		row.BusyMs += cur.b.Sub(cur.a).Milliseconds()
	}
	if row.SpanMs > 0 {
		row.BusyShare = float64(row.BusyMs) / float64(row.SpanMs)
	}
	row.Samples = len(samples)
	if len(samples) > 0 {
		paused := 0
		for _, s := range samples {
			if s.State == SampleStatePaused {
				paused++
			}
		}
		row.PausedShare = float64(paused) / float64(len(samples))
	}
	row.NoDataMin = int(noData(samples, from, to) / time.Minute)
	if len(ttfts) > 0 {
		sort.Slice(ttfts, func(i, j int) bool { return ttfts[i] < ttfts[j] })
		row.TTFTp50Ms = ttfts[(len(ttfts)-1)*50/100]
		row.TTFTp95Ms = ttfts[(len(ttfts)-1)*95/100]
	}
	if tpsMs > 0 {
		row.TPS = float64(tpsOut) / (float64(tpsMs) / 1000)
	}
	return row
}

// noData is the time without the agent's minute samples: stretches over two
// minutes between samples, or before the first and after the last.
func noData(samples []Sample, from, to time.Time) time.Duration {
	if len(samples) == 0 {
		return to.Sub(from)
	}
	var missing time.Duration
	prev := from
	for _, sm := range append(samples, Sample{At: to}) {
		if gap := sm.At.Sub(prev); gap > 2*time.Minute {
			missing += gap - time.Minute
		}
		prev = sm.At
	}
	return missing
}

func startedIn(reqs []RequestRecord, a, b time.Time) []RequestRecord {
	var out []RequestRecord
	for _, r := range reqs {
		if !r.StartedAt.Before(a) && r.StartedAt.Before(b) {
			out = append(out, r)
		}
	}
	return out
}

func withinSamples(ss []Sample, a, b time.Time) []Sample {
	var out []Sample
	for _, s := range ss {
		if !s.At.Before(a) && s.At.Before(b) {
			out = append(out, s)
		}
	}
	return out
}

func (s *StatsStore) periodStart(t time.Time, by string) time.Time {
	t = s.local(t)
	if by == "hour" {
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location())
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// periodNext steps by wall clock, so a DST day is 23 or 25 hours long.
func (s *StatsStore) periodNext(t time.Time, by string) time.Time {
	if by == "hour" {
		return t.Add(time.Hour)
	}
	return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
}

func (s *StatsStore) periodKey(t time.Time, by string) string {
	if by == "hour" { // the zone tells apart the hour a DST night repeats
		return s.local(t).Format("2006-01-02 15:00 MST")
	}
	return s.local(t).Format("2006-01-02 Mon")
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// WriteReport renders the report as "table", "json" or "csv".
func WriteReport(w io.Writer, rep *Report, format string) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	case "csv":
		cw := csv.NewWriter(w)
		rows := [][]string{{"period", "requests", "errors", "input_tokens", "output_tokens", "busy_share", "busy_ms", "cooldown_ms", "paused_share", "no_data_min", "ttft_p50_ms", "ttft_p95_ms", "tps"}}
		for _, r := range append(rep.Rows, rep.Total) {
			rows = append(rows, []string{r.Key, strconv.Itoa(r.Requests), strconv.Itoa(r.Errors),
				strconv.FormatInt(r.InputTokens, 10), strconv.FormatInt(r.OutputTokens, 10),
				strconv.FormatFloat(r.BusyShare, 'f', 4, 64), strconv.FormatInt(r.BusyMs, 10), strconv.FormatInt(r.CooldownMs, 10),
				pausedCSV(r.PausedShare), strconv.Itoa(r.NoDataMin),
				strconv.FormatInt(r.TTFTp50Ms, 10), strconv.FormatInt(r.TTFTp95Ms, 10), strconv.FormatFloat(r.TPS, 'f', 1, 64)})
		}
		return cw.WriteAll(rows)
	case "table", "":
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
		head := "Period"
		if rep.By == "model" {
			head = "Model"
		}
		fmt.Fprintf(tw, "%s\tRequests\tErrors\tTokens in\tTokens out\tBusy\tCooldown\tPaused\tNo data\tTTFT p50/p95\tTPS\t\n", head)
		for _, r := range append(rep.Rows, rep.Total) {
			if r.Key == "total" {
				fmt.Fprintln(tw, strings.Repeat("\t", 11))
			}
			fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t\n", r.Key, r.Requests, r.Errors,
				humanCount(r.InputTokens), humanCount(r.OutputTokens), percent(r.BusyShare), humanDur(r.CooldownMs),
				pausedCell(r.PausedShare), noDataCell(r.NoDataMin), ttftCell(r), tpsCell(r.TPS))
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		for _, g := range rep.Gaps {
			if g.From.IsZero() {
				fmt.Fprintf(w, "\nMissing: the proxy no longer had requests from before %s.\n", fmtTime(g.Until))
				continue
			}
			fmt.Fprintf(w, "\nMissing: the proxy no longer had requests from %s to %s.\n", fmtTime(g.From), fmtTime(g.Until))
		}
		if rep.Total.NoDataMin > 0 && rep.By != "model" {
			fmt.Fprintf(w, "\nNo data: the agent was not running for %s of this period; requests then may still be listed.\n", humanDur(int64(rep.Total.NoDataMin)*60000))
		}
		return nil
	}
	return fmt.Errorf("unknown --format %q (want table, json or csv)", format)
}

// pausedCSV leaves the cell empty where there are no samples (model rows).
func pausedCSV(f float64) string {
	if f < 0 {
		return ""
	}
	return strconv.FormatFloat(f, 'f', 4, 64)
}

func percent(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }

func pausedCell(f float64) string {
	if f < 0 {
		return "-"
	}
	return percent(f)
}

func noDataCell(min int) string {
	if min == 0 {
		return "-"
	}
	return humanDur(int64(min) * 60000)
}

func ttftCell(r ReportRow) string {
	if r.TTFTp50Ms == 0 {
		return "-"
	}
	return fmt.Sprintf("%s/%s", humanDur(r.TTFTp50Ms), humanDur(r.TTFTp95Ms))
}

func tpsCell(f float64) string {
	if f == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", f)
}

func humanCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	}
	return strconv.FormatInt(n, 10)
}

func humanDur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case ms == 0:
		return "0"
	case d < time.Second:
		return fmt.Sprintf("%dms", ms)
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func fmtTime(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }

// ParseWhen reads "24h", "7d", "2026-10-01" or "2026-10-01T15:04" (local time)
// as a moment before now.
func ParseWhen(s string, now time.Time) (time.Time, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		if days, err := strconv.Atoi(n); err == nil && days >= 0 {
			return now.AddDate(0, 0, -days), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, now.Location()); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("bad time %q (want e.g. 24h, 7d, 2026-10-01 or 2026-10-01T15:04)", s)
}

// tailWindow is how far back each poll looks: records reach the log when their
// request ends, up to a pull later, so a long request lands well after it began.
const tailWindow = time.Hour

// TailRequests prints requests as they reach the log, from since on, until ctx ends.
func TailRequests(ctx context.Context, st *StatsStore, since time.Time, w io.Writer, every time.Duration) error {
	seen := map[string]time.Time{}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for {
		now := time.Now()
		recs, err := st.Requests(maxTime(since, now.Add(-tailWindow)), now.Add(time.Minute))
		if err != nil {
			return err
		}
		for _, r := range recs {
			if _, ok := seen[r.RequestID]; ok {
				continue
			}
			seen[r.RequestID] = r.StartedAt
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d→%d tok\t%s\t%d%%\n", r.StartedAt.Local().Format("15:04:05"), r.Kind, r.Model,
				outcomeCell(r), r.InputTokens, r.OutputTokens, humanDur(r.DurationMs), r.Level)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		for id, at := range seen { // out of the window: never read again
			if at.Before(now.Add(-tailWindow)) {
				delete(seen, id)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(every):
		}
	}
}

func outcomeCell(r RequestRecord) string {
	if r.ErrorCode != "" {
		return r.Outcome + " " + r.ErrorCode
	}
	return r.Outcome
}
