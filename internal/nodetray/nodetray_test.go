package nodetray

import (
	"bytes"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
		s        *nodeagent.Status
		err      error
		icon     IconKind
		fill     uint32
		requests int
		title    string
		level    string
		resume   bool
		stop     string
		ticking  bool
	}{
		"full": {s: status(nil), icon: IconLevel, fill: 100, level: "Running at 100% (schedule)"},
		"partial by schedule": {s: status(func(s *nodeagent.Status) {
			s.Level.Effective = 80
			s.Level.Next = &nodeagent.StatusNext{At: now.Add(9 * time.Hour), Level: 0}
		}), icon: IconLevel, fill: 80, level: "Running at 80% (schedule) · 18:00 → 0%"},
		"manual pause counts down": {s: status(func(s *nodeagent.Status) {
			s.Level.Effective, s.Level.Source = 0, nodeagent.SourceOverride
			s.Level.Override = &nodeagent.Override{Until: now.Add(9*time.Minute + 41*time.Second)}
			s.Requests.ProxyActive = 0
		}), icon: IconPaused, title: "9:41", level: "Paused (by hand until 09:09)", resume: true, ticking: true},
		"schedule pause": {s: status(func(s *nodeagent.Status) {
			s.Level.Effective = 0
			s.Level.Next = &nodeagent.StatusNext{At: now.Add(22 * time.Hour), Level: 100}
		}), icon: IconPaused, title: "until Wed 07:00"},
		"waiting for requests": {s: status(func(s *nodeagent.Status) {
			s.Level.Effective, s.Requests.ProxyActive, s.CanAbort = 0, 2, true
		}), icon: IconWaiting, title: "2", level: "Pausing: waiting for 2 requests (schedule)", stop: "Stop now (cuts 2 requests)"},
		"agent down": {err: errors.New("dial unix: no such file"), icon: IconWarn, level: "The node agent does not answer"},
		"proxy down": {s: status(func(s *nodeagent.Status) { s.Connection.LastError = "unavailable" }), icon: IconWarn, fill: 100},
		"ktms override": {s: status(func(s *nodeagent.Status) {
			s.Level.Effective, s.Level.Source, s.Requests.Inflight = 30, nodeagent.SourceKTMSOverride, 3
		}), icon: IconLevel, fill: 30, requests: 3, level: "Running at 30% (set in KTMS)"},
	} {
		v := Build(c.s, c.err, now)
		if v.Icon != c.icon || v.Fill != c.fill || v.Requests != c.requests || v.Title != c.title || (c.level != "" && v.Level != c.level) || v.CanResume != c.resume || v.StopNow != c.stop || v.Ticking != c.ticking {
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
	paused := status(func(s *nodeagent.Status) {
		s.Level.Effective, s.Level.Source = 0, nodeagent.SourceOverride
		s.Level.Override = &nodeagent.Override{Until: now.Add(10 * time.Minute)}
	})
	if got := n.Next(paused, nil, now); len(got) != 0 {
		t.Fatalf("first status: %v", got)
	}
	if got := n.Next(status(nil), nil, now.Add(time.Minute)); len(got) != 0 {
		t.Fatalf("the owner's own Resume notified: %v", got)
	}
	n.Next(paused, nil, now)
	if got := n.Next(status(nil), nil, now.Add(10*time.Minute)); len(got) != 1 || !strings.Contains(got[0], "pause is over") {
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
	for k := IconLevel; k <= IconWarn; k++ {
		for _, tmpl := range []bool{true, false} {
			img, err := png.Decode(bytes.NewReader(Icon(Still(k, 50, 3), tmpl)))
			if err != nil || img.Bounds().Dx() != iconSize {
				t.Fatalf("icon %d: %v", k, err)
			}
		}
	}
}

func alphaAt(t *testing.T, f Frame, x, y float64) uint8 {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(Icon(f, true)))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, a := img.At(int(x), int(y)).RGBA()
	return uint8(a >> 8)
}

// slot is the middle of square i's place.
func slot(i int) (x, y float64) {
	return sqLeft + float64(i/2)*squarePitch + sqSize/2, sqTop + float64(i%2)*squarePitch + sqSize/2
}

func TestIconSquares(t *testing.T) {
	edgeX := float64(iconSize - 1) // the sliver of the third column
	for _, c := range []struct {
		n      int
		filled []int // slots drawn
		sliver [2]bool
	}{
		{n: 0},
		{n: 1, filled: []int{0}},
		{n: 4, filled: []int{0, 1, 2, 3}},
		{n: 5, filled: []int{0, 1, 2, 3}, sliver: [2]bool{true, false}},
		{n: 40, filled: []int{0, 1, 2, 3}, sliver: [2]bool{true, true}},
	} {
		f := Still(IconLevel, 100, c.n)
		for i := range 4 * min(c.n, 1) { // alone, the big circle covers the slots
			x, y := slot(i)
			want := slices.Contains(c.filled, i)
			if got := alphaAt(t, f, x, y) == 0xff; got != want {
				t.Errorf("%d requests: square %d drawn %v", c.n, i, got)
			}
		}
		for row, want := range c.sliver {
			_, y := slot(row)
			if got := alphaAt(t, f, edgeX, y) > 0; got != want {
				t.Errorf("%d requests: row %d cut by the edge %v", c.n, row, got)
			}
		}
		// Alone, the circle is centered and big; beside squares it moves left.
		if got := alphaAt(t, f, iconCenter+aloneR-2, iconCenter) > 0; got != (c.n == 0) {
			t.Errorf("%d requests: circle reaches the right half %v", c.n, got)
		}
	}
}

func TestIconSquaresFly(t *testing.T) {
	x, y := slot(1)
	arrive := Frame{Kind: IconLevel, Level: 100, Squares: Move([]uint64{7}, []uint64{7, 8})}
	if alphaAt(t, arrive, x, y) != 0 {
		t.Error("an arriving square starts in its place")
	}
	if arrive.T = 1; alphaAt(t, arrive, x, y) != 0xff {
		t.Error("an arriving square does not land")
	}
	mid := Frame{Kind: IconLevel, Level: 100, Squares: Move(nil, []uint64{1}), T: 0.5}
	if _, y0 := slot(0); alphaAt(t, mid, sqLeft+1.5, y0) > 0x40 || alphaAt(t, mid, sqLeft+sqSize+1.5, y0) < 0x80 {
		t.Error("halfway the square is not on its way in from the right")
	}
}

// The request that ends leaves from its own place; the others close up.
func TestIconSquaresFollowRequests(t *testing.T) {
	m := Move([]uint64{1, 2, 3}, []uint64{2, 3, 4})
	want := []Square{{0, Off}, {1, 0}, {2, 1}, {Off, 2}}
	if !slices.Equal(m, want) {
		t.Fatalf("move = %v, want %v", m, want)
	}
	x0, y0 := slot(0)
	f := Frame{Kind: IconLevel, Level: 100, Squares: Move([]uint64{1, 2}, []uint64{2}), T: 0.15}
	if alphaAt(t, f, x0, y0) == 0xff {
		t.Error("the first square still sits in its place on its way out")
	}
	if alphaAt(t, f, x0, y0-sqSize) == 0 {
		t.Error("the first square does not leave upward")
	}
	if f.T = 1; alphaAt(t, f, x0, y0) != 0xff {
		t.Error("the second square did not take the first place")
	}
	if x1, y1 := slot(1); alphaAt(t, f, x1, y1) != 0 {
		t.Error("the second place stays filled")
	}
}

func TestIconCircleFill(t *testing.T) {
	c, inner := iconCenter, aloneR-4 // inside the ring
	at := func(level uint32, dx, dy float64) bool {
		return alphaAt(t, Still(IconLevel, level, 0), c+dx*inner, c+dy*inner) == 0xff
	}
	if alphaAt(t, Still(IconLevel, 0, 0), c, c-aloneR) == 0 {
		t.Error("no ring")
	}
	q := math.Sqrt(0.5)
	for _, x := range []struct {
		level                                  uint32
		topRight, bottomRight, bottomLeft, top bool
	}{
		{0, false, false, false, false},
		{25, true, false, false, true},
		{50, true, true, false, true},
		{100, true, true, true, true},
	} {
		got := [4]bool{at(x.level, q, -q), at(x.level, q, q), at(x.level, -q, q), at(x.level, 0.05, -1)}
		if want := [4]bool{x.topRight, x.bottomRight, x.bottomLeft, x.top}; got != want {
			t.Errorf("%d%%: top-right, bottom-right, bottom-left, top = %v, want %v", x.level, got, want)
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
	// Strict would be withheld on the redirect from the tray's file:// page.
	peek, _ := w.URL("status")
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if r, err := noFollow.Get(peek); err != nil || len(r.Cookies()) != 1 || r.Cookies()[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie: %v %+v", err, r.Cookies())
	}
	link, _ := w.URL("status")
	r, err := c.Get(link)
	if err != nil || r.StatusCode != http.StatusOK || r.Request.URL.RawQuery != "" {
		t.Fatalf("ticket login: %v %v %q", r.StatusCode, err, r.Request.URL)
	}
	// A ticket works once: whoever saw the link afterwards gets nothing.
	if r, _ := http.Get(link); r.StatusCode != http.StatusForbidden {
		t.Fatalf("reused ticket: %d", r.StatusCode)
	}
	path, err := w.OpenFile("schedule")
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	page, _ := os.ReadFile(path)
	if fi.Mode().Perm() != 0o600 || !strings.Contains(string(page), "#schedule") {
		t.Fatalf("open file %v: %s", fi.Mode().Perm(), page)
	}
	os.Remove(path)
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
	req.AddCookie(&http.Cookie{Name: cookieName, Value: w.session})
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

func TestStopNowKeepsAManualPause(t *testing.T) {
	plain := StopNow(status(nil), now)
	if plain.For != "10m" || !plain.AbortInflight {
		t.Fatalf("without a pause: %+v", plain)
	}
	end := now.Add(25 * time.Minute)
	held := StopNow(status(func(s *nodeagent.Status) {
		s.Level.Effective, s.Level.Source = 0, nodeagent.SourceOverride
		s.Level.Override = &nodeagent.Override{Level: 0, Until: end}
	}), now)
	if held.For != "" || !held.Until.Equal(end) || !held.AbortInflight {
		t.Fatalf("with a 25-minute pause: %+v", held)
	}
}

func TestBuildEngines(t *testing.T) {
	if v := Build(status(nil), nil, now); len(v.Engines) != 0 {
		t.Errorf("one engine: lines %v, want none", v.Engines)
	}
	v := Build(status(func(s *nodeagent.Status) {
		s.Engines = []nodeagent.StatusEngine{
			{Name: "ollama", Primary: true, Up: true, Models: []string{"gemma"}},
			{Name: "mt", Up: true, Paused: true, Models: []string{"opus-mt-de"}},
			{Name: "vllm", Error: "connection refused"},
		}
	}), nil, now)
	want := []EngineLine{
		{Name: "ollama", Title: "ollama · gemma", Action: "Pause ollama"},
		{Name: "mt", Title: "mt · paused", Action: "Resume mt", Paused: true},
		{Name: "vllm", Title: "vllm · not answering", Action: "Pause vllm"},
	}
	if len(v.Engines) != len(want) {
		t.Fatalf("lines = %+v", v.Engines)
	}
	for i := range want {
		if v.Engines[i] != want[i] {
			t.Errorf("line %d = %+v, want %+v", i, v.Engines[i], want[i])
		}
	}
}
