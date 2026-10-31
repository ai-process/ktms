package nodeagent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestControlSocketIsOwnerOnlyAndKeepsSecrets(t *testing.T) {
	a, reg, _, c := startControlled(t, "", &fakeClock{t: at("09:00")}, func(cfg *AgentConfig) {
		cfg.NodeKey, cfg.NodeAuthToken, cfg.EngineToken = "llm_node_secretkey", "access-secret", "engine-secret"
	})
	waitLevel(t, reg, 100, 5*time.Second)
	fi, err := os.Stat(c.Socket)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	resp, err := c.hc.Get("http://agent/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, secret := range []string{"llm_node_secretkey", "access-secret", "engine-secret"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("status leaks %q: %s", secret, body)
		}
	}

	// Another user on the socket is refused, whatever the file mode says.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	a.controlHandler().ServeHTTP(rec, req.WithContext(context.WithValue(req.Context(), peerUIDKey{}, os.Getuid()+1)))
	if peerCredSupported && rec.Code != http.StatusForbidden {
		t.Errorf("another uid got %d, want 403", rec.Code)
	}
}

func TestControlCommandTimesOutWhileAgentIsBusy(t *testing.T) {
	old := ctlTimeout
	ctlTimeout = 50 * time.Millisecond
	defer func() { ctlTimeout = old }()
	a := NewAgent(AgentConfig{}) // its Run loop never starts
	if _, err := a.do(context.Background(), func() (any, error) { return nil, nil }); !errors.Is(err, errAgentBusy) {
		t.Fatalf("do = %v, want errAgentBusy", err)
	}
	rec := httptest.NewRecorder()
	a.handleClearOverride(rec, httptest.NewRequest(http.MethodDelete, "/v1/override", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
}

func TestPutScheduleValidatesAndApplies(t *testing.T) {
	clock := &fakeClock{t: at("09:00")}
	a, reg, _, c := startControlled(t, "", clock)
	waitLevel(t, reg, 100, 5*time.Second)
	ctx := context.Background()

	_, err := c.PutSchedule(ctx, "10:00-11:00 50\n10:30-12:00 0\n")
	var ce *ControlCallError
	if !errors.As(err, &ce) || ce.Status != 422 || ce.Line != 2 {
		t.Fatalf("invalid schedule: %v (%+v), want 422 at line 2", err, ce)
	}
	if data, _ := os.ReadFile(a.cfg.ScheduleFile); len(data) != 0 {
		t.Fatalf("an invalid schedule was written: %q", data)
	}

	info, err := c.PutSchedule(ctx, "00:00-24:00 40\n")
	if err != nil || info.Level != 40 {
		t.Fatalf("PutSchedule = %+v, %v", info, err)
	}
	waitLevel(t, reg, 40, time.Second)

	got, err := c.Schedule(ctx)
	if err != nil || got.Text != "00:00-24:00 40\n" || !got.Exists || !got.Editable || len(got.Days) != 7 {
		t.Fatalf("Schedule = %+v, %v", got, err)
	}
	prev, err := c.PreviewSchedule(ctx, "mon-fri 09:00-18:00 0\n")
	if err != nil || prev.Error != "" || len(prev.Upcoming) != 5 {
		t.Fatalf("Preview = %+v, %v", prev, err)
	}
	if data, _ := os.ReadFile(a.cfg.ScheduleFile); string(data) != "00:00-24:00 40\n" {
		t.Fatalf("a preview wrote the file: %q", data)
	}
}

func TestEventsStreamLevelChanges(t *testing.T) {
	_, reg, _, c := startControlled(t, "", &fakeClock{t: at("09:00")})
	waitLevel(t, reg, 100, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := make(chan *Status, 16)
	go func() { _ = c.Events(ctx, func(s *Status) { got <- s }) }()
	if s := <-got; s.Level.Effective != 100 {
		t.Fatalf("first event level %d, want the current 100", s.Level.Effective)
	}
	if _, err := c.SetOverride(ctx, OverrideRequest{Level: 0, For: "10m"}); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case s := <-got:
			if s.Level.Effective == 0 && s.Level.Source == SourceOverride {
				return
			}
		case <-ctx.Done():
			t.Fatal("no event for the pause")
		}
	}
}

func TestListenControlReplacesStaleSocketOnly(t *testing.T) {
	dir, err := os.MkdirTemp("", "kn")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "c.sock")

	live, err := listenControl(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := listenControl(path); err == nil || !strings.Contains(err.Error(), "another agent") {
		t.Fatalf("second listener: %v, want refusal", err)
	}
	// A crashed agent leaves the file behind with nobody listening.
	live.(*net.UnixListener).SetUnlinkOnClose(false)
	live.Close()
	l, err := listenControl(path)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	l.Close()

	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenControl(path); err == nil {
		t.Fatal("a regular file was replaced by the socket")
	}
}

func TestControlClientReportsAgentDown(t *testing.T) {
	dir, err := os.MkdirTemp("", "kn")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	_, err = NewControlClient(filepath.Join(dir, "none.sock")).Status(context.Background())
	if !errors.Is(err, ErrAgentDown) {
		t.Fatalf("err = %v, want ErrAgentDown", err)
	}
}

func TestStatsEndpointMatchesReport(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStats(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := st.AppendRequests([]RequestRecord{{RequestID: "r1", StartedAt: now.Add(-time.Hour), DurationMs: 1000, Model: "m", Outcome: "ok", OutputTokens: 10}}); err != nil {
		t.Fatal(err)
	}
	a := NewAgent(AgentConfig{StatsDir: dir})
	rec := httptest.NewRecorder()
	a.handleStats(rec, httptest.NewRequest(http.MethodGet, "/v1/stats?from=24h&by=model", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"requests":1`) || !strings.Contains(rec.Body.String(), `"by":"model"`) {
		t.Fatalf("stats = %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	a.handleStats(rec, httptest.NewRequest(http.MethodGet, "/v1/stats?by=week", nil))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad by: %d", rec.Code)
	}
}

func TestCancelInflightCutsRunningRequests(t *testing.T) {
	started := make(chan struct{})
	gw, srv, _ := newTestGateway(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body) // the server notices a dropped client only once the body is read
		close(started)
		<-r.Context().Done()
	}))
	gw.keepaliveAfter = time.Hour
	done := make(chan *http.Response, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer access-key")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
		}
		done <- resp
	}()
	<-started
	if n := gw.CancelInflight(); n != 1 {
		t.Fatalf("cut %d requests, want 1", n)
	}
	resp := <-done
	if resp == nil {
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "stopped by the node's owner") {
		t.Fatalf("cut request = %d %s", resp.StatusCode, body)
	}
	if n := gw.CancelInflight(); n != 0 {
		t.Fatalf("finished requests still tracked: %d", n)
	}
}
