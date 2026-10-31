package nodeagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	minOverride = time.Minute
	maxOverride = 24 * time.Hour // a node cannot be forgotten on a pause for good
)

// Where the level in force comes from.
const (
	SourceSchedule = "schedule"
	SourceOverride = "override"
	SourceDefault  = "default" // no schedule: 100%
)

// Override is a level the owner set by hand until a moment; it wins over the
// schedule until then. Until is wall clock, so a Mac that slept past it ends it on waking.
type Override struct {
	Level uint32    `json:"level"`
	Until time.Time `json:"until"`
	SetAt time.Time `json:"set_at"`
	Warm  bool      `json:"warm"` // warm the models before Until, as a schedule pause does
}

func (o *Override) activeAt(t time.Time) bool { return o != nil && t.Before(o.Until) }

// OverrideRequest is what the control API and the CLI ask for. Exactly one of
// For, Until and UntilNextWindow sets the end.
type OverrideRequest struct {
	Level           uint32    `json:"level"`
	For             string    `json:"for,omitempty"` // Go duration: 10m, 1h
	Until           time.Time `json:"until,omitzero"`
	UntilNextWindow bool      `json:"until_next_window,omitempty"`
	Warm            bool      `json:"warm,omitempty"`
	AbortInflight   bool      `json:"abort_inflight,omitempty"`
}

// errBadRequest marks an error the caller can fix; the API answers 422 with it.
type errBadRequest struct{ msg string }

func (e errBadRequest) Error() string { return e.msg }

func badRequest(format string, args ...any) error { return errBadRequest{fmt.Sprintf(format, args...)} }

// resolve turns a request into an override starting at now. next is the
// schedule's next switch, for UntilNextWindow.
func (r OverrideRequest) resolve(now time.Time, next time.Time, nextOK bool) (*Override, error) {
	if r.Level > 100 {
		return nil, badRequest("level %d is above 100", r.Level)
	}
	ends := 0
	for _, set := range []bool{r.For != "", !r.Until.IsZero(), r.UntilNextWindow} {
		if set {
			ends++
		}
	}
	if ends != 1 {
		return nil, badRequest("give exactly one of for, until and until_next_window")
	}
	var until time.Time
	switch {
	case r.For != "":
		d, err := time.ParseDuration(r.For)
		if err != nil {
			return nil, badRequest("for: %v", err)
		}
		until = now.Add(d)
	case r.UntilNextWindow:
		if !nextOK {
			return nil, badRequest("the schedule never switches; give a duration instead")
		}
		until = next
		if until.Sub(now) > maxOverride {
			until = now.Add(maxOverride)
		}
	default:
		until = r.Until
	}
	switch d := until.Sub(now); {
	case d < minOverride:
		return nil, badRequest("the override must last at least %v", minOverride)
	case d > maxOverride:
		return nil, badRequest("the override may last at most %v", maxOverride)
	}
	return &Override{Level: r.Level, Until: until.UTC(), SetAt: now.UTC(), Warm: r.Warm}, nil
}

// loadOverride reads the override a previous run left. An expired one is
// removed; a broken one is ignored with a warning and the schedule rules.
func loadOverride(path string, now time.Time) *Override {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		log.Warn().Err(err).Str("file", path).Msg("cannot read the manual level; following the schedule")
		return nil
	}
	var o Override
	if err := json.Unmarshal(data, &o); err != nil || o.Level > 100 || o.Until.IsZero() {
		log.Warn().Err(err).Str("file", path).Msg("the manual level file is broken; following the schedule")
		return nil
	}
	if !o.activeAt(now) {
		if err := removeIfExists(path); err != nil {
			log.Warn().Err(err).Str("file", path).Msg("cannot remove the expired manual level")
		}
		return nil
	}
	log.Info().Uint32("level", o.Level).Time("until", o.Until).Msg("manual level restored")
	return &o
}

// saveOverride stores o so a restarted agent keeps it; nil removes the file.
func saveOverride(path string, o *Override) error {
	if path == "" {
		return nil
	}
	if o == nil {
		return removeIfExists(path)
	}
	data, err := json.Marshal(o)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// effectiveLevel is the override while it lasts, else the schedule.
func (a *Agent) effectiveLevel(now time.Time) (uint32, string) {
	p := &a.pause
	if p.override.activeAt(now) {
		return p.override.Level, SourceOverride
	}
	if p.sched == nil {
		return 100, SourceDefault
	}
	return p.sched.Level(now), SourceSchedule
}

// nextSwitch is when the level in force next changes: the override's end
// if the schedule then differs from it, else the schedule's next switch.
func (a *Agent) nextSwitch(now time.Time) (at time.Time, level uint32, ok bool, source string) {
	p := &a.pause
	if o := p.override; o.activeAt(now) {
		if l := p.sched.Level(o.Until); l != o.Level {
			return o.Until, l, true, "override_end"
		}
		at, level, ok = p.sched.Next(o.Until)
		return at, level, ok, SourceSchedule
	}
	at, level, ok = p.sched.Next(now)
	return at, level, ok, SourceSchedule
}

// expireOverride drops an override whose time has passed and reports whether it did.
func (a *Agent) expireOverride(now time.Time) bool {
	p := &a.pause
	if p.override == nil || p.override.activeAt(now) {
		return false
	}
	log.Info().Uint32("level", p.override.Level).Msg("manual level ended; following the schedule")
	a.setOverride(nil)
	return true
}

// setOverride replaces the override, stores it and re-arms the timer that ends it.
func (a *Agent) setOverride(o *Override) {
	p := &a.pause
	p.override = o
	p.nextDirty = true
	if err := saveOverride(a.overrideFile(), o); err != nil {
		log.Warn().Err(err).Msg("cannot store the manual level; a restart will forget it")
	}
	if a.ovTimer != nil {
		a.ovTimer.Stop()
		a.ovTimer = nil
	}
	if o != nil {
		a.ovTimer = time.NewTimer(max(o.Until.Sub(a.now()), 0))
	}
}

// ovC fires when the override ends; a time.Timer stops while a Mac sleeps,
// so the level ticker still checks the wall clock behind it.
func (a *Agent) ovC() <-chan time.Time {
	if a.ovTimer == nil {
		return nil
	}
	return a.ovTimer.C
}

func (a *Agent) overrideFile() string {
	if a.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(a.cfg.StateDir, "override.json")
}
