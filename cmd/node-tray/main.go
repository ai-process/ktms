//go:build (darwin && cgo) || linux

// Command ktms-node-tray shows a local node's level in the menu bar or tray
// and lets its owner pause it, limit it and edit its schedule. It is a client
// of the agent's control socket and keeps no logic of levels of its own.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"fyne.io/systray"

	"github.com/ai-process/ktms/internal/nodeagent"
	"github.com/ai-process/ktms/internal/nodetray"
)

var version = "dev"

func main() {
	socket := flag.String("control-socket", nodeagent.DefaultControlSocket(), "The agent's control socket")
	showVersion := flag.Bool("version", false, "Print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	t := &tray{client: nodeagent.NewControlClient(*socket), home: nodeagent.DefaultHome()}
	web, err := nodetray.StartWeb(*socket, filepath.Join(t.home, "tray.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "ktms-node-tray: %v\n", err)
		os.Exit(1)
	}
	t.web = web
	systray.Run(t.onReady, func() { _ = web.Close() })
}

type tray struct {
	client *nodeagent.ControlClient
	web    *nodetray.Web
	home   string

	mu     sync.Mutex
	status *nodeagent.Status
	err    error
	notes  nodetray.Notifier
	icon   nodetray.IconKind
	iconOK bool

	header, level, detail, notice                  *systray.MenuItem
	p5, p10, p30, untilNext, limit, stop, resume   *systray.MenuItem
	l50, l30, l10, lOther                          *systray.MenuItem
	ktms, schedule, stats, settings, openLog, quit *systray.MenuItem
}

func (t *tray) onReady() {
	systray.SetTooltip("Keystone node")
	t.header = systray.AddMenuItem("Keystone node", "")
	t.level = systray.AddMenuItem("Connecting…", "")
	t.detail = systray.AddMenuItem("", "")
	t.notice = systray.AddMenuItem("", "")
	for _, m := range []*systray.MenuItem{t.header, t.level, t.detail, t.notice} {
		m.Disable()
	}
	systray.AddSeparator()
	t.p5 = systray.AddMenuItem("Pause for 5 minutes", "")
	t.p10 = systray.AddMenuItem("Pause for 10 minutes", "")
	t.p30 = systray.AddMenuItem("Pause for 30 minutes", "")
	t.untilNext = systray.AddMenuItem("Pause until the next switch", "")
	t.limit = systray.AddMenuItem("Limit", "Run at a lower level for a while")
	t.l50 = t.limit.AddSubMenuItem("50% for 1 hour", "")
	t.l30 = t.limit.AddSubMenuItem("30% for 1 hour", "")
	t.l10 = t.limit.AddSubMenuItem("10% for 1 hour", "")
	t.lOther = t.limit.AddSubMenuItem("Other…", "")
	t.stop = systray.AddMenuItem("Stop now", "Cut the requests running now; their callers get an error")
	t.resume = systray.AddMenuItem("Resume schedule", "")
	systray.AddSeparator()
	t.ktms = systray.AddMenuItemCheckbox("Schedule set by KTMS", "KTMS sets the schedule and load; the local schedule is ignored", false)
	t.schedule = systray.AddMenuItem("Schedule…", "")
	t.stats = systray.AddMenuItem("Statistics…", "")
	t.settings = systray.AddMenuItem("Settings…", "")
	t.openLog = systray.AddMenuItem("Open log", "")
	systray.AddSeparator()
	t.quit = systray.AddMenuItem("Quit tray", "The node keeps running")
	t.render()

	go t.follow()
	go t.tick()
	go t.clicks()
}

// follow keeps an event stream to the agent open, reconnecting while it is down.
func (t *tray) follow() {
	backoff := time.Second
	for {
		err := t.client.Events(context.Background(), func(s *nodeagent.Status) {
			backoff = time.Second
			t.update(s, nil)
		})
		t.update(nil, err)
		time.Sleep(backoff)
		backoff = min(backoff*2, 10*time.Second)
	}
}

// tick redraws a countdown and lets a silent agent raise its notification.
func (t *tray) tick() {
	for range time.Tick(time.Second) {
		t.mu.Lock()
		s, err := t.status, t.err
		t.mu.Unlock()
		if err != nil {
			t.update(nil, err)
		} else if s != nil && nodetray.Build(s, nil, time.Now()).Ticking {
			t.render()
		}
	}
}

func (t *tray) update(s *nodeagent.Status, err error) {
	t.mu.Lock()
	t.status, t.err = s, err
	msgs := t.notes.Next(s, err, time.Now())
	t.mu.Unlock()
	if t.web.Prefs().Notifications {
		for _, m := range msgs {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = nodetray.Notify(ctx, m)
			}()
		}
	}
	t.render()
}

func (t *tray) render() {
	t.mu.Lock()
	v := nodetray.Build(t.status, t.err, time.Now())
	changed := !t.iconOK || v.Icon != t.icon
	t.icon, t.iconOK = v.Icon, true
	t.mu.Unlock()

	if changed {
		systray.SetTemplateIcon(nodetray.Icon(v.Icon, true), nodetray.Icon(v.Icon, false))
	}
	if runtime.GOOS == "darwin" {
		systray.SetTitle(v.Title)
	}
	systray.SetTooltip(v.Tooltip)
	t.header.SetTitle(v.Header)
	t.level.SetTitle(v.Level)
	setOptional(t.detail, v.Detail)
	setOptional(t.notice, v.Notice)
	for _, m := range []*systray.MenuItem{t.p5, t.p10, t.p30, t.limit, t.ktms} {
		enable(m, v.Connected)
	}
	setOptional(t.untilNext, v.UntilNext)
	setOptional(t.stop, v.StopNow)
	if v.CanResume {
		t.resume.Show()
	} else {
		t.resume.Hide()
	}
	if v.ScheduleKTMS {
		t.ktms.Check()
	} else {
		t.ktms.Uncheck()
	}
}

func setOptional(m *systray.MenuItem, title string) {
	if title == "" {
		m.Hide()
		return
	}
	m.SetTitle(title)
	m.Show()
}

func enable(m *systray.MenuItem, on bool) {
	if on {
		m.Enable()
	} else {
		m.Disable()
	}
}

func (t *tray) clicks() {
	pause := func(d string) { t.set(nodeagent.OverrideRequest{Level: 0, For: d}) }
	limit := func(l uint32) { t.set(nodeagent.OverrideRequest{Level: l, For: "1h"}) }
	for {
		select {
		case <-t.p5.ClickedCh:
			pause("5m")
		case <-t.p10.ClickedCh:
			pause("10m")
		case <-t.p30.ClickedCh:
			pause("30m")
		case <-t.untilNext.ClickedCh:
			t.set(nodeagent.OverrideRequest{Level: 0, UntilNextWindow: true})
		case <-t.l50.ClickedCh:
			limit(50)
		case <-t.l30.ClickedCh:
			limit(30)
		case <-t.l10.ClickedCh:
			limit(10)
		case <-t.lOther.ClickedCh:
			openURL(t.web.URL("status"))
		case <-t.stop.ClickedCh:
			t.set(nodeagent.OverrideRequest{Level: 0, For: "10m", AbortInflight: true})
		case <-t.resume.ClickedCh:
			t.call(func(ctx context.Context) error { _, err := t.client.ClearOverride(ctx); return err })
		case <-t.ktms.ClickedCh:
			src := nodeagent.ScheduleSourceKTMS
			if t.ktms.Checked() {
				src = nodeagent.ScheduleSourceLocal
			}
			t.call(func(ctx context.Context) error { _, err := t.client.SetScheduleSource(ctx, src); return err })
		case <-t.schedule.ClickedCh:
			openURL(t.web.URL("schedule"))
		case <-t.stats.ClickedCh:
			openURL(t.web.URL("stats"))
		case <-t.settings.ClickedCh:
			openURL(t.web.URL("settings"))
		case <-t.openLog.ClickedCh:
			openURL(filepath.Join(t.home, "logs", "node.log"))
		case <-t.quit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

func (t *tray) set(req nodeagent.OverrideRequest) {
	t.call(func(ctx context.Context) error { _, err := t.client.SetOverride(ctx, req); return err })
}

// call runs a command off the menu's goroutine; a refusal becomes a notification.
func (t *tray) call(fn func(context.Context) error) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := fn(ctx); err != nil {
			_ = nodetray.Notify(ctx, err.Error())
		}
	}()
}

func openURL(target string) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	_ = exec.Command(name, target).Start()
}
