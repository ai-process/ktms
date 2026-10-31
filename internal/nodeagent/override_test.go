package nodeagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOverrideRequestResolve(t *testing.T) {
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	next := now.Add(2 * time.Hour)
	for _, c := range []struct {
		req    OverrideRequest
		nextOK bool
		until  time.Time
		err    string
	}{
		{req: OverrideRequest{For: "10m"}, until: now.Add(10 * time.Minute)},
		{req: OverrideRequest{Level: 30, Until: now.Add(time.Hour)}, until: now.Add(time.Hour)},
		{req: OverrideRequest{UntilNextWindow: true}, nextOK: true, until: next},
		{req: OverrideRequest{UntilNextWindow: true}, err: "never switches"},
		{req: OverrideRequest{Level: 101, For: "1h"}, err: "above 100"},
		{req: OverrideRequest{For: "30s"}, err: "at least"},
		{req: OverrideRequest{For: "25h"}, err: "at most"},
		{req: OverrideRequest{}, err: "exactly one"},
		{req: OverrideRequest{For: "1h", UntilNextWindow: true}, nextOK: true, err: "exactly one"},
		{req: OverrideRequest{For: "soon"}, err: "for:"},
	} {
		o, err := c.req.resolve(now, next, c.nextOK)
		if c.err != "" {
			var bad errBadRequest
			if err == nil || !strings.Contains(err.Error(), c.err) || !errors.As(err, &bad) {
				t.Errorf("%+v: err = %v, want a bad request with %q", c.req, err, c.err)
			}
			continue
		}
		if err != nil || !o.Until.Equal(c.until) || o.Level != c.req.Level {
			t.Errorf("%+v: = %+v, %v; want until %v", c.req, o, err, c.until)
		}
	}
	// A schedule switch more than a day off still caps the pause at 24h.
	o, err := OverrideRequest{UntilNextWindow: true}.resolve(now, now.Add(72*time.Hour), true)
	if err != nil || !o.Until.Equal(now.Add(maxOverride)) {
		t.Errorf("far next window: %+v, %v", o, err)
	}
}

func TestLoadOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "override.json")
	now := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	if loadOverride(path, now) != nil {
		t.Fatal("no file gave an override")
	}
	active := &Override{Level: 0, Until: now.Add(time.Hour), SetAt: now}
	if err := saveOverride(path, active); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("override file mode %v, want 0600", fi.Mode().Perm())
	}
	if o := loadOverride(path, now); o == nil || !o.Until.Equal(active.Until) {
		t.Fatalf("active override = %+v", o)
	}
	if loadOverride(path, now.Add(2*time.Hour)) != nil {
		t.Fatal("an expired override was loaded")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("an expired override file was kept")
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if loadOverride(path, now) != nil {
		t.Fatal("a broken file gave an override")
	}
}

// startControlled runs an agent with a control socket and a state directory.
func startControlled(t *testing.T, schedule string, clock *fakeClock, opts ...func(*AgentConfig)) (*Agent, *levelRegistry, *residentOllama, *ControlClient) {
	t.Helper()
	dir, err := os.MkdirTemp("", "kn") // t.TempDir() paths overflow a Unix socket name on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "control.sock")
	opts = append([]func(*AgentConfig){func(c *AgentConfig) {
		c.StateDir, c.ControlSocket, c.Version = dir, sock, "test"
	}}, opts...)
	a, reg, ollama := startAgent(t, schedule, clock, opts...)
	c := NewControlClient(sock)
	waitFor(t, "control socket", func() bool { _, err := c.Status(context.Background()); return err == nil })
	return a, reg, ollama, c
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func at(clock string) time.Time {
	v, _ := time.ParseInLocation("2006-01-02 15:04", "2026-10-20 "+clock, time.Local)
	return v
}

func TestManualPauseEndsOnWallClockAndWarmsAfter(t *testing.T) {
	clock := &fakeClock{t: at("09:00")}
	a, reg, ollama, c := startControlled(t, "", clock)
	waitLevel(t, reg, 100, 5*time.Second)

	res, err := c.SetOverride(context.Background(), OverrideRequest{Level: 0, For: "10m"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status.Level.Effective != 0 || res.Status.Level.Source != SourceOverride || res.Status.Level.Next == nil || res.Status.Level.Next.Source != "override_end" {
		t.Fatalf("status after pause = %+v", res.Status.Level)
	}
	waitLevel(t, reg, 0, time.Second)
	waitCalls(t, ollama, "m:1b=0")

	// Inside --warmup of the end, a manual pause still keeps the memory free.
	clock.Set(at("09:07"))
	time.Sleep(100 * time.Millisecond)
	if calls := strings.Join(ollama.log(), " "); calls != "m:1b=0" {
		t.Fatalf("engine calls before the pause ended: %s", calls)
	}
	// The Mac slept past the end: the wall clock ends it on waking.
	clock.Set(at("09:30"))
	waitLevel(t, reg, 100, time.Second)
	waitCalls(t, ollama, "m:1b=0 m:1b=-1")
	if _, err := os.Stat(a.overrideFile()); !os.IsNotExist(err) {
		t.Fatal("the ended override is still on disk")
	}
	s, _ := c.Status(context.Background())
	if s.Level.Source != SourceSchedule || s.Level.Override != nil {
		t.Fatalf("after the end: %+v", s.Level)
	}
}

func TestWarmOverrideWarmsBeforeItsEnd(t *testing.T) {
	clock := &fakeClock{t: at("09:00")}
	_, reg, ollama, c := startControlled(t, "", clock)
	waitLevel(t, reg, 100, 5*time.Second)
	if _, err := c.SetOverride(context.Background(), OverrideRequest{Level: 0, For: "1h", Warm: true}); err != nil {
		t.Fatal(err)
	}
	waitCalls(t, ollama, "m:1b=0")
	clock.Set(at("09:56"))
	waitCalls(t, ollama, "m:1b=0 m:1b=-1")
}

func TestOverrideSurvivesRestart(t *testing.T) {
	dir, err := os.MkdirTemp("", "kn")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	o := &Override{Level: 0, Until: at("10:00").UTC(), SetAt: at("08:00").UTC()}
	if err := saveOverride(filepath.Join(dir, "override.json"), o); err != nil {
		t.Fatal(err)
	}
	reg, _ := func() (*levelRegistry, *residentOllama) {
		_, r, ol := startAgent(t, "", &fakeClock{t: at("09:00")}, func(c *AgentConfig) { c.StateDir = dir })
		return r, ol
	}()
	waitLevel(t, reg, 0, 5*time.Second)
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for _, l := range reg.levels {
		if l != 0 {
			t.Fatalf("levels %v: a restarted agent dropped the manual pause", reg.levels)
		}
	}
}

func TestOverrideUntilNextWindowAndResumeIntoSchedulePause(t *testing.T) {
	clock := &fakeClock{t: at("09:00")}
	_, reg, _, c := startControlled(t, "10:00-23:00 0\n", clock)
	waitLevel(t, reg, 100, 5*time.Second)
	ctx := context.Background()

	res, err := c.SetOverride(ctx, OverrideRequest{Level: 0, UntilNextWindow: true})
	if err != nil {
		t.Fatal(err)
	}
	// The pause runs into the schedule's own, so the next change is the schedule's at 23:00.
	if o := res.Status.Level.Override; o == nil || !o.Until.Equal(at("10:00")) {
		t.Fatalf("override = %+v, want until 10:00", o)
	}
	if n := res.Status.Level.Next; n == nil || !n.At.Equal(at("23:00")) || n.Source != SourceSchedule {
		t.Fatalf("next = %+v, want the schedule's 23:00", n)
	}
	if s, err := c.ClearOverride(ctx); err != nil || s.Level.Effective != 100 {
		t.Fatalf("resume before the window: %+v, %v", s, err)
	}

	clock.Set(at("10:30"))
	waitLevel(t, reg, 0, time.Second)
	if _, err := c.SetOverride(ctx, OverrideRequest{Level: 50, For: "1h"}); err != nil {
		t.Fatal(err)
	}
	waitLevel(t, reg, 50, time.Second)
	s, err := c.ClearOverride(ctx)
	if err != nil || s.Level.Effective != 0 || s.Level.Source != SourceSchedule {
		t.Fatalf("resume inside the schedule's pause: %+v, %v", s, err)
	}
	waitLevel(t, reg, 0, time.Second)
}

func TestOverrideRefusalsAreBadRequests(t *testing.T) {
	_, reg, _, c := startControlled(t, "", &fakeClock{t: at("09:00")})
	waitLevel(t, reg, 100, 5*time.Second)
	ctx := context.Background()
	for _, req := range []OverrideRequest{
		{Level: 0, UntilNextWindow: true}, // no switches in an empty schedule
		{Level: 0, For: "10m", AbortInflight: true}, // no built-in gateway
		{Level: 30, For: "48h"},
	} {
		_, err := c.SetOverride(ctx, req)
		var ce *ControlCallError
		if !errors.As(err, &ce) || ce.Status != 422 {
			t.Errorf("%+v: err = %v, want 422", req, err)
		}
	}
	b, _ := json.Marshal(OverrideRequest{Level: 0, For: "10m"})
	if strings.Contains(string(b), "until") {
		t.Errorf("an unset until is sent: %s", b)
	}
}

func TestUntilNextWindowFollowsTheOverride(t *testing.T) {
	_, reg, _, c := startControlled(t, "08:00-14:00 50\n", &fakeClock{t: at("13:29")})
	waitLevel(t, reg, 50, 5*time.Second)
	ctx := context.Background()
	res, err := c.SetOverride(ctx, OverrideRequest{Level: 70, For: "1h"})
	if err != nil || res.Status.Level.Next == nil || !res.Status.Level.Next.At.Equal(at("14:29")) {
		t.Fatalf("next after the override = %+v, %v", res.Status.Level.Next, err)
	}
	// The pause ends where the window said, not at the schedule's masked 14:00 switch.
	res, err = c.SetOverride(ctx, OverrideRequest{Level: 0, UntilNextWindow: true})
	if err != nil || !res.Status.Level.Override.Until.Equal(at("14:29")) {
		t.Fatalf("until next = %+v, %v", res.Status.Level.Override, err)
	}
}
