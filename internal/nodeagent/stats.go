package nodeagent

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RequestRecord is one request the proxy sent to this node, one line of
// requests-YYYY-MM-DD.jsonl (the day it started, local time).
type RequestRecord struct {
	ID              string    `json:"id"`
	RequestID       string    `json:"request_id"`
	StartedAt       time.Time `json:"started_at"`
	DurationMs      int64     `json:"duration_ms"`
	TTFTMs          int64     `json:"ttft_ms,omitempty"`
	Model           string    `json:"model"`
	Kind            string    `json:"kind"`
	InputTokens     int64     `json:"input_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	CacheReadTokens int64     `json:"cache_read_tokens,omitempty"`
	Outcome         string    `json:"outcome"`
	ErrorCode       string    `json:"error_code,omitempty"`
	Level           uint32    `json:"level"`
	CooldownMs      int64     `json:"cooldown_ms,omitempty"`
}

// Sample is the agent's own view of the node once a minute, one line of
// samples-YYYY-MM-DD.jsonl.
type Sample struct {
	At            time.Time `json:"at"`
	State         string    `json:"state"` // serving | paused | draining | offline
	Level         uint32    `json:"level"`
	ProxyActive   int32     `json:"proxy_active"`
	EngineRunning uint32    `json:"engine_running,omitempty"`
	EngineWaiting uint32    `json:"engine_waiting,omitempty"`
	GPUUtil       float64   `json:"gpu_util,omitempty"`
	GPUTempC      uint32    `json:"gpu_temp_c,omitempty"`
	VRAMFreeMB    uint64    `json:"vram_free_mb,omitempty"`
	VRAMTotalMB   uint64    `json:"vram_total_mb,omitempty"`
	Warm          []string  `json:"warm,omitempty"`
}

// Gap is a stretch the proxy no longer had when the agent pulled: requests in
// it may be missing from the log.
type Gap struct {
	From  time.Time `json:"from"`
	Until time.Time `json:"until"`
}

const (
	SampleStateServing  = "serving"
	SampleStatePaused   = "paused"
	SampleStateDraining = "draining"
	SampleStateOffline  = "offline"
)

var dayFile = regexp.MustCompile(`^(requests|samples)-(\d{4}-\d{2}-\d{2})\.jsonl(\.gz)?$`)

// StatsStore is the node's usage log on disk: JSON Lines per kind and local
// day, appended and fsynced, closed days gzipped. Plain files keep the binary
// cgo-free and readable with jq.
type StatsStore struct {
	dir string
	loc *time.Location // nil = the machine's zone; tests pin one
}

// DefaultStatsDir is where the installer's node keeps its log.
func DefaultStatsDir() string {
	base := os.Getenv("KTMS_HOME")
	if base == "" {
		base = os.Getenv("LLMPROXY_HOME")
	}
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".ktms-node")
	}
	return filepath.Join(base, "stats")
}

// OpenStats returns the store in dir, creating it for writing when create is set.
func OpenStats(dir string, create bool) (*StatsStore, error) {
	if dir == "" {
		return nil, errors.New("no stats directory")
	}
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	return &StatsStore{dir: dir}, nil
}

func (s *StatsStore) Dir() string { return s.dir }

func (s *StatsStore) local(t time.Time) time.Time {
	if s.loc != nil {
		return t.In(s.loc)
	}
	return t.Local()
}

func (s *StatsStore) day(t time.Time) string { return s.local(t).Format("2006-01-02") }

// AppendRequests writes records to the file of the day each started.
func (s *StatsStore) AppendRequests(recs []RequestRecord) error {
	byDay := map[string][][]byte{}
	var days []string
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			return err
		}
		d := s.day(r.StartedAt)
		if _, ok := byDay[d]; !ok {
			days = append(days, d)
		}
		byDay[d] = append(byDay[d], line)
	}
	for _, d := range days {
		if err := s.appendLines("requests-"+d+".jsonl", byDay[d]); err != nil {
			return err
		}
	}
	return nil
}

func (s *StatsStore) AppendSample(sm Sample) error {
	line, err := json.Marshal(sm)
	if err != nil {
		return err
	}
	return s.appendLines("samples-"+s.day(sm.At)+".jsonl", [][]byte{line})
}

func (s *StatsStore) AppendGap(g Gap) error {
	line, err := json.Marshal(g)
	if err != nil {
		return err
	}
	return s.appendLines("gaps.jsonl", [][]byte{line})
}

func (s *StatsStore) appendLines(name string, lines [][]byte) error {
	f, err := os.OpenFile(filepath.Join(s.dir, name), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	// A crash mid-write leaves a line without its newline; end it, or the next
	// record would join the fragment and be lost with it.
	if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
			buf.WriteByte('\n')
		}
	}
	for _, l := range lines {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Cursor is the proxy log position already stored; "" before the first pull.
func (s *StatsStore) Cursor() (string, error) {
	b, err := os.ReadFile(filepath.Join(s.dir, "cursor"))
	if os.IsNotExist(err) {
		return "", nil
	}
	return strings.TrimSpace(string(b)), err
}

// SetCursor moves the position, after the records before it are on disk: a
// crash in between repeats records (readers drop them by request_id), never
// loses them.
func (s *StatsStore) SetCursor(id string) error {
	return writeFileAtomic(filepath.Join(s.dir, "cursor"), []byte(id+"\n"))
}

// CursorAge is how long ago the cursor last moved; ok is false before the first pull.
func (s *StatsStore) CursorAge(now time.Time) (age time.Duration, ok bool) {
	fi, err := os.Stat(filepath.Join(s.dir, "cursor"))
	if err != nil {
		return 0, false
	}
	return now.Sub(fi.ModTime()), true
}

// Touch marks a pull that found nothing new, so CursorAge tells a live agent
// from a stopped one.
func (s *StatsStore) Touch(now time.Time) error {
	p := filepath.Join(s.dir, "cursor")
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return s.SetCursor("")
	}
	return os.Chtimes(p, now, now)
}

// Maintain gzips the files of closed days and deletes days older than
// retention (0 keeps everything). A record pulled late for a closed day
// lands in a fresh .jsonl, which joins the .gz as one more gzip member.
func (s *StatsStore) Maintain(now time.Time, retention time.Duration) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	today := s.day(now)
	oldest := ""
	if retention > 0 {
		oldest = s.day(now.Add(-retention))
	}
	var errs []error
	if oldest != "" {
		errs = append(errs, s.pruneGaps(now.Add(-retention)))
	}
	for _, e := range entries {
		m := dayFile.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		day, gz := m[2], m[3] != ""
		path := filepath.Join(s.dir, e.Name())
		switch {
		case oldest != "" && day < oldest:
			errs = append(errs, os.Remove(path))
		case !gz && day < today:
			errs = append(errs, gzipInto(path, path+".gz"))
		}
	}
	return errors.Join(errs...)
}

// pruneGaps drops gaps that ended before the retention, with the days they covered.
func (s *StatsStore) pruneGaps(before time.Time) error {
	gaps, err := s.Gaps(before, time.Now().AddDate(100, 0, 0))
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, g := range gaps {
		line, err := json.Marshal(g)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	path := filepath.Join(s.dir, "gaps.jsonl")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	return writeFileAtomic(path, buf.Bytes())
}

func gzipInto(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	if _, err := zw.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Days lists the days in [from, to] (local dates) that have files of kind.
func (s *StatsStore) Days(kind, from, to string) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, e := range entries {
		m := dayFile.FindStringSubmatch(e.Name())
		if m == nil || m[1] != kind || m[2] < from || m[2] > to {
			continue
		}
		seen[m[2]] = true
	}
	days := make([]string, 0, len(seen))
	for d := range seen {
		days = append(days, d)
	}
	sort.Strings(days)
	return days, nil
}

// readDay calls fn for each line of a day's files; a torn line (crash
// mid-write) is skipped. The open file goes first: if the agent gzips it in
// between, its lines show up twice (callers drop repeats), never not at all.
func (s *StatsStore) readDay(kind, day string, fn func([]byte) error) error {
	base := filepath.Join(s.dir, kind+"-"+day+".jsonl")
	for _, p := range []string{base, base + ".gz"} {
		f, err := os.Open(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		var r io.Reader = f
		if strings.HasSuffix(p, ".gz") {
			zr, err := gzip.NewReader(f)
			if err != nil {
				f.Close()
				return fmt.Errorf("%s: %w", p, err)
			}
			r = zr
		}
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if len(bytes.TrimSpace(sc.Bytes())) == 0 {
				continue
			}
			if err := fn(sc.Bytes()); err != nil {
				f.Close()
				return err
			}
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	return nil
}

// Requests returns the requests that started in [from, to), without the
// repeats a crash between a write and its cursor leaves behind.
func (s *StatsStore) Requests(from, to time.Time) ([]RequestRecord, error) {
	days, err := s.Days("requests", s.day(from), s.day(to))
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []RequestRecord
	for _, d := range days {
		err := s.readDay("requests", d, func(line []byte) error {
			var r RequestRecord
			if json.Unmarshal(line, &r) != nil || seen[r.RequestID] {
				return nil
			}
			seen[r.RequestID] = true
			if !r.StartedAt.Before(from) && r.StartedAt.Before(to) {
				out = append(out, r)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, nil
}

// Samples returns the samples taken in [from, to), each once.
func (s *StatsStore) Samples(from, to time.Time) ([]Sample, error) {
	days, err := s.Days("samples", s.day(from), s.day(to))
	if err != nil {
		return nil, err
	}
	seen := map[int64]bool{}
	var out []Sample
	for _, d := range days {
		err := s.readDay("samples", d, func(line []byte) error {
			var sm Sample
			if json.Unmarshal(line, &sm) != nil || seen[sm.At.UnixNano()] {
				return nil
			}
			seen[sm.At.UnixNano()] = true
			if !sm.At.Before(from) && sm.At.Before(to) {
				out = append(out, sm)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// Gaps returns the recorded gaps that overlap [from, to).
func (s *StatsStore) Gaps(from, to time.Time) ([]Gap, error) {
	f, err := os.Open(filepath.Join(s.dir, "gaps.jsonl"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Gap
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var g Gap
		if json.Unmarshal(sc.Bytes(), &g) == nil && g.Until.After(from) && g.From.Before(to) {
			out = append(out, g)
		}
	}
	return out, sc.Err()
}

// ParseRetention reads "90d", "12h" or "0" (keep everything).
func ParseRetention(s string) (time.Duration, error) {
	if s == "0" {
		return 0, nil
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days < 0 {
			return 0, fmt.Errorf("bad retention %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("bad retention %q (want e.g. 90d, 36h or 0)", s)
	}
	return d, nil
}
