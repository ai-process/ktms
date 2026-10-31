package nodetray

import (
	"bytes"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ai-process/ktms/internal/nodeagent"
)

var now = time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local)

func status(mut func(*nodeagent.Status)) *nodeagent.Status {
	s := &nodeagent.Status{
		NodeID: "mac", State: nodeagent.StateActive, CapacityApplied: true,
		Connection: nodeagent.StatusConn{Registered: true},
		Level:      nodeagent.StatusLevel{Effective: 100, Schedule: 100, Source: nodeagent.SourceSchedule},
		Models:     []nodeagent.StatusModel{{ID: "q:32b", Warm: true}},
	}
	if mut != nil {
		mut(s)
	}
	return s
}

func TestBuildView(t *testing.T) {
	for name, c := range map[string]struct {
		s       *nodeagent.Status
		err     error
		icon    IconKind
		title   string
		level   string
		resume  bool
		stop    string
		ticking bool
	}{
		"full": {s: status(nil), icon: IconFull, title: "100%", level: "Running at 100% (schedule)"},
		"partial by schedule": {s: status(func(s *nodeagent.Status) {
			s.Level.Effective = 80
			s.Level.Next = &nodeagent.StatusNext{At: now.Add(9 * time.Hour), Level: 0}
		}), icon: IconPartial, title: "80%", level: "Running at 80% (schedule) · 18:00 → 0%"},
		"manual pause counts down": {s: status(func(s *nodeagent.Status) {
			s.Level.Effective, s.Level.Source = 0, nodeagent.SourceOverride
			s.Level.Override = &nodeagent.Override{Until: now.Add(9*time.Minute + 41*time.Second)}
			s.Requests.ProxyActive = 0
		}), icon: IconPaused, title: "⏸ 9:41", level: "Paused (by hand until 09:09)", resume: true, ticking: true},
		"schedule pause": {s: status(func(s *nodeagent.Status) {
			s.Level.Effective = 0
			s.Level.Next = &nodeagent.StatusNext{At: now.Add(22 * time.Hour), Level: 100}
		}), icon: IconPaused, title: "⏸ until Wed 07:00"},
		"waiting for requests": {s: status(func(s *nodeagent.Status) {
			s.Level.Effective, s.Requests.ProxyActive, s.CanAbort = 0, 2, true
		}), icon: IconWaiting, title: "⏳ 2", level: "Pausing: waiting for 2 requests (schedule)", stop: "Stop now (cuts 2 requests)"},
		"agent down":    {err: errors.New("dial unix: no such file"), icon: IconWarn, level: "The node agent does not answer"},
		"proxy down":    {s: status(func(s *nodeagent.Status) { s.Connection.LastError = "unavailable" }), icon: IconWarn, title: "100%"},
		"ktms override": {s: status(func(s *nodeagent.Status) { s.Level.Effective, s.Level.Source = 30, nodeagent.SourceKTMSOverride }), icon: IconPartial, title: "30%", level: "Running at 30% (set in KTMS)"},
	} {
		v := Build(c.s, c.err, now)
		if v.Icon != c.icon || v.Title != c.title || (c.level != "" && v.Level != c.level) || v.CanResume != c.resume || v.StopNow != c.stop || v.Ticking != c.ticking {
			t.Errorf("%s: %+v", name, v)
		}
	}
	if v := Build(status(func(s *nodeagent.Status) { s.ScheduleSource = "ktms" }), nil, now); !v.ScheduleKTMS || !v.Connected {
		t.Errorf("ktms checkbox: %+v", v)
	}
	if v := Build(status(func(s *nodeagent.Status) { s.ScheduleError = "line 2: bad" }), nil, now); !strings.Contains(v.Notice, "invalid") {
		t.Errorf("schedule error notice: %q", v.Notice)
	}
}

func TestNotifier(t *testing.T) {
	var n Notifier
	paused := status(func(s *nodeagent.Status) { s.Level.Effective, s.Level.Source = 0, nodeagent.SourceOverride })
	if got := n.Next(paused, nil, now); len(got) != 0 {
		t.Fatalf("first status: %v", got)
	}
	if got := n.Next(status(nil), nil, now); len(got) != 1 || !strings.Contains(got[0], "pause is over") {
		t.Fatalf("pause end: %v", got)
	}
	if got := n.Next(status(func(s *nodeagent.Status) { s.ScheduleError = "line 3: x" }), nil, now); len(got) != 1 || !strings.Contains(got[0], "invalid") {
		t.Fatalf("bad schedule: %v", got)
	}
	down := errors.New("down")
	if got := n.Next(nil, down, now); len(got) != 0 {
		t.Fatalf("a brief silence notified: %v", got)
	}
	if got := n.Next(nil, down, now.Add(silentAfter)); len(got) != 1 {
		t.Fatalf("a long silence: %v", got)
	}
	if got := n.Next(nil, down, now.Add(2*silentAfter)); len(got) != 0 {
		t.Fatalf("silence told twice: %v", got)
	}
	if got := n.Next(status(nil), nil, now.Add(3*silentAfter)); len(got) != 1 || !strings.Contains(got[0], "again") {
		t.Fatalf("recovery: %v", got)
	}
}

func TestIconsArePNGs(t *testing.T) {
	for k := IconFull; k <= IconWarn; k++ {
		for _, tmpl := range []bool{true, false} {
			img, err := png.Decode(bytes.NewReader(Icon(k, tmpl)))
			if err != nil || img.Bounds().Dx() != iconSize {
				t.Fatalf("icon %d: %v", k, err)
			}
		}
	}
}

// fakeAgent serves a control API on a socket; it answers status and records the last request.
func fakeAgent(t *testing.T) (socket string, last *http.Request) {
	t.Helper()
	dir, err := os.MkdirTemp("", "kt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket = filepath.Join(dir, "c.sock")
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	last = &http.Request{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*last = *r
		_ = json.NewEncoder(w).Encode(status(nil))
	})}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Close() })
	return socket, last
}

func TestWebGuardsAndRelays(t *testing.T) {
	socket, last := fakeAgent(t)
	w, err := StartWeb(socket, filepath.Join(t.TempDir(), "tray.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	base := "http://" + w.addr

	// Without the token nothing opens.
	if r, _ := http.Get(base + "/api/v1/status"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("no cookie: %d", r.StatusCode)
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	if r, _ := c.Get(base + "/?t=wrong"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong token: %d", r.StatusCode)
	}
	r, err := c.Get(w.URL("status"))
	if err != nil || r.StatusCode != http.StatusOK || r.Request.URL.RawQuery != "" {
		t.Fatalf("token login: %v %v %q", r.StatusCode, err, r.Request.URL)
	}
	body, _ := io.ReadAll(r.Body)
	if !strings.Contains(string(body), "Keystone node") {
		t.Fatal("index not served")
	}

	// Reads relay to the agent; writes need the header a cross-site form cannot send.
	if r, _ := c.Get(base + "/api/v1/status"); r.StatusCode != http.StatusOK || last.URL.Path != "/v1/status" || last.Header.Get("Cookie") != "" {
		t.Fatalf("relay: %d %q cookie %q", r.StatusCode, last.URL.Path, last.Header.Get("Cookie"))
	}
	if r, _ := c.Post(base+"/api/v1/override", "application/json", strings.NewReader(`{}`)); r.StatusCode != http.StatusForbidden {
		t.Fatalf("write without header: %d", r.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodDelete, base+"/api/v1/override", nil)
	req.Header.Set(csrfHeader, "1")
	if r, _ := c.Do(req); r.StatusCode != http.StatusOK || last.Method != http.MethodDelete {
		t.Fatalf("write with header: %d %s", r.StatusCode, last.Method)
	}

	// Another Host (DNS rebinding) is refused even with the cookie.
	req, _ = http.NewRequest(http.MethodGet, base+"/api/v1/status", nil)
	req.Host = "evil.example"
	req.AddCookie(&http.Cookie{Name: cookieName, Value: w.token})
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host: %d", r.StatusCode)
	}

	// Prefs persist.
	req, _ = http.NewRequest(http.MethodPut, base+"/api/prefs", strings.NewReader(`{"notifications":false}`))
	req.Header.Set(csrfHeader, "1")
	if r, _ := c.Do(req); r.StatusCode != http.StatusOK || w.Prefs().Notifications || LoadPrefs(w.prefsFile).Notifications {
		t.Fatalf("prefs not saved: %d", r.StatusCode)
	}
}

func TestFormatGrid(t *testing.T) {
	week := make([]int, slotsPerWeek)
	for i := range week {
		week[i] = 100
	}
	for d := range 5 { // mon-fri 09:00-18:00 at 0%
		for s := 18; s < 36; s++ {
			week[d*48+s] = 0
		}
	}
	body, _ := json.Marshal(map[string]any{"week": week})
	rec := httptest.NewRecorder()
	handleFormat(rec, httptest.NewRequest(http.MethodPost, "/api/format", bytes.NewReader(body)))
	var out struct{ Text string }
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || out.Text != "mon-fri 09:00-18:00 0\n" {
		t.Fatalf("format = %d %q", rec.Code, out.Text)
	}
	rec = httptest.NewRecorder()
	handleFormat(rec, httptest.NewRequest(http.MethodPost, "/api/format", strings.NewReader(`{"week":[1,2]}`)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("short week: %d", rec.Code)
	}
}
