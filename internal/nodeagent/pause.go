package nodeagent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog/log"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

const (
	defaultLevelEvery = 30 * time.Second // wall-clock poll: Go's timers stop while a Mac sleeps
	defaultPauseGrace = 5 * time.Second  // requests the proxy picked us for just before the pause
	engineRetry       = time.Minute      // after a failed warm or unload; a stopped engine is not hammered
	warmTimeout       = 10 * time.Minute
	unloadTimeout     = time.Minute
)

// pauseState is the agent's schedule bookkeeping; only Run's goroutine touches it.
type pauseState struct {
	sched     *Schedule
	content   []byte // of the file last read, valid or not; nil when there is none
	schedErr  string // why the file last read was refused; "" when it is in force
	override  *Override
	level     uint32
	source    string    // where level comes from: schedule, override or default
	nextAt    time.Time // next switch, kept until the level or the file changes
	next      uint32
	nextOK    bool
	nextSrc   string
	nextDirty bool // the override changed: recompute the next switch
	started   bool
	pausedAt  time.Time // when the level last fell to 0

	gateClosed bool
	unloaded   bool     // the served models were freed in this pause
	warmed     bool     // the warm-up before the next window was done
	warmLate   bool     // a pause that skipped its warm-up ended: warm now
	resident   []string // models freed by the pause, to warm again
	epoch      int      // bumps on each level change; stale engine results are ignored
	busy       bool     // an engine call is in flight
	retryAt    time.Time
	active     int32 // proxy requests still running, from the last heartbeat; -1 unknown
	models     []*pb.NodeModelSpec

	warnedOldProxy, warnedNoControl bool
}

type engineResult struct {
	warm   bool
	models []string
	epoch  int
	err    error
}

// reloadSchedule picks up an edited, created or removed schedule file and
// reports whether the schedule changed. Content, not mtime, tells an edit:
// coarse mtimes miss a quick same-size change. A broken edit keeps the
// schedule in force.
func (a *Agent) reloadSchedule() bool {
	p := &a.pause
	if a.cfg.ScheduleFile == "" {
		return false
	}
	data, err := os.ReadFile(a.cfg.ScheduleFile)
	if os.IsNotExist(err) {
		changed := p.sched != nil
		if changed {
			log.Info().Str("file", a.cfg.ScheduleFile).Msg("schedule file removed; running at 100%")
		}
		p.sched, p.content, p.schedErr = nil, nil, ""
		return changed
	}
	if err != nil {
		log.Warn().Err(err).Str("file", a.cfg.ScheduleFile).Msg("cannot read schedule file; keeping the current schedule")
		return false
	}
	if p.content != nil && bytes.Equal(data, p.content) {
		return false
	}
	p.content = data
	s, err := ParseSchedule(bytes.NewReader(data))
	if err != nil {
		p.schedErr = err.Error()
		log.Error().Err(err).Str("file", a.cfg.ScheduleFile).Msg("schedule file is invalid; keeping the current schedule")
		return false
	}
	p.sched, p.schedErr = s, ""
	log.Info().Str("file", a.cfg.ScheduleFile).Str("schedule", s.Status(a.now())).Msg("schedule loaded")
	return true
}

// refreshLevel re-reads the schedule, ends an expired override and reports
// whether the level in force changed.
func (a *Agent) refreshLevel() bool {
	p := &a.pause
	reloaded := a.reloadSchedule()
	now := a.now()
	a.expireOverride(now)
	lvl, src := a.effectiveLevel(now)
	if p.started && lvl == p.level {
		if reloaded || p.nextDirty || src != p.source {
			p.nextAt, p.next, p.nextOK, p.nextSrc = a.nextSwitch(now)
			p.nextDirty = false
		}
		p.source = src
		return false
	}
	p.nextAt, p.next, p.nextOK, p.nextSrc = a.nextSwitch(now)
	p.nextDirty = false
	from := p.level
	if !p.started {
		from = 100
		p.started = true
		if lvl == 0 {
			p.pausedAt = time.Time{} // starting inside a pause: no request can be on its way yet
		}
	} else {
		log.Info().Uint32("from", from).Uint32("to", lvl).Str("source", src).Str("next", a.describeNext(now)).Msg("level changed")
		if lvl == 0 {
			p.pausedAt = now
		}
	}
	p.level, p.source = lvl, src
	if lvl == 0 && from != 0 || lvl != 0 && from == 0 {
		// A manual pause skips the warm-up so the owner keeps the memory; the models load once it ends.
		p.warmLate = lvl != 0 && p.unloaded && !p.warmed
		p.epoch++
		p.gateClosed, p.unloaded, p.warmed = false, false, false
		p.retryAt = time.Time{}
		p.active = -1 // a count from before the switch says nothing about the pause
	}
	if lvl != 0 && a.gateway != nil {
		a.gateway.SetAccepting(true)
	}
	return true
}

// afterHeartbeat records what the proxy said and moves the pause along.
func (a *Agent) afterHeartbeat(resp *pb.HeartbeatResponse, models []*pb.NodeModelSpec) {
	p := &a.pause
	if !resp.GetOk() {
		return // a node the proxy does not know reports no requests, which proves nothing
	}
	p.active = resp.GetActiveProxyRequests()
	p.models = models
	if !resp.GetCapacityApplied() && p.level < 100 && !p.warnedOldProxy {
		p.warnedOldProxy = true
		log.Warn().Uint32("level", p.level).Msg("the proxy ignores capacity levels (it predates them) and routes to this node at 100%; at 0% the gateway still refuses requests")
	}
	a.stepPause()
}

// stepPause closes the gateway after the grace period, then frees the served
// models once the proxy has no requests left here, and loads them again
// --warmup before the next window opens.
func (a *Agent) stepPause() {
	p := &a.pause
	if p.level != 0 {
		a.warmAfterPause()
		return
	}
	now := a.now()
	if !p.gateClosed && now.Sub(p.pausedAt) >= a.pauseGrace {
		if a.gateway != nil {
			a.gateway.SetAccepting(false)
		}
		p.gateClosed = true
	}
	if p.busy || now.Before(p.retryAt) {
		return
	}
	ctl, ok := a.inspector.(EngineController)
	if !ok {
		if !p.warnedNoControl {
			p.warnedNoControl = true
			log.Info().Str("engine", a.cfg.Engine).Msg("this engine cannot unload models on request; a pause only stops routing")
		}
		return
	}
	coldPause := p.override.activeAt(now) && !p.override.Warm
	if p.nextOK && p.next > 0 && p.nextAt.Sub(now) <= a.cfg.Warmup && !coldPause {
		if !p.warmed {
			a.engineCall(ctl, true, a.warmTargets())
		}
		return
	}
	if p.gateClosed && p.active == 0 && !p.unloaded {
		a.engineCall(ctl, false, a.unloadTargets())
	}
}

// warmAfterPause loads the models a pause freed without warming them back.
func (a *Agent) warmAfterPause() {
	p := &a.pause
	if !p.warmLate || p.busy || a.now().Before(p.retryAt) {
		return
	}
	ctl, ok := a.inspector.(EngineController)
	if !ok {
		p.warmLate = false
		return
	}
	a.engineCall(ctl, true, a.warmTargets())
}

// describeNext renders the next switch of the level in force.
func (a *Agent) describeNext(now time.Time) string {
	p := &a.pause
	if !p.nextOK {
		return "none"
	}
	at, local := p.nextAt.Local(), now.Local()
	if p.sched != nil {
		at, local = p.sched.local(p.nextAt), p.sched.local(now)
	}
	format := "15:04"
	if at.YearDay() != local.YearDay() || at.Year() != local.Year() {
		format = "Mon 15:04"
	}
	return fmt.Sprintf("%s %d%%", at.Format(format), p.next)
}

// unloadTargets are the served models the engine holds in memory now.
func (a *Agent) unloadTargets() []string {
	var out []string
	for _, m := range a.pause.models {
		if m.GetIsWarm() {
			out = append(out, m.GetRemoteModelName())
		}
	}
	return out
}

// warmTargets are the models this pause freed or, for an agent started
// inside a pause, the ones it is told to serve.
func (a *Agent) warmTargets() []string {
	if len(a.pause.resident) > 0 {
		return a.pause.resident
	}
	return a.cfg.OnlyModels
}

// engineCall runs a warm or unload off the loop: loading a model takes
// minutes, and heartbeats must keep going meanwhile.
func (a *Agent) engineCall(ctl EngineController, warm bool, models []string) {
	p := &a.pause
	if len(models) == 0 {
		a.finishEngineCall(engineResult{warm: warm, epoch: p.epoch})
		return
	}
	p.busy = true
	epoch, url, token := p.epoch, a.cfg.EngineURL, a.cfg.EngineToken
	go func() {
		timeout := unloadTimeout
		if warm {
			timeout = warmTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		res := engineResult{warm: warm, models: models, epoch: epoch}
		for _, m := range models {
			call := ctl.Unload
			if warm {
				call = ctl.Warm
			}
			if err := call(ctx, url, token, m); err != nil {
				res.err = err
				break
			}
		}
		a.engineDone <- res
	}()
}

func (a *Agent) finishEngineCall(r engineResult) {
	p := &a.pause
	p.busy = false
	if r.epoch != p.epoch {
		return // the level moved on while the call ran
	}
	what := "unload"
	if r.warm {
		what = "warm-up"
	}
	if r.err != nil {
		p.retryAt = a.now().Add(engineRetry)
		log.Warn().Err(r.err).Strs("models", r.models).Dur("retry_in", engineRetry).Msg("schedule " + what + " failed")
		return
	}
	if r.warm {
		p.warmed, p.warmLate = true, false
	} else {
		p.unloaded = true
		if len(r.models) > 0 {
			p.resident = r.models
		}
	}
	if len(r.models) > 0 {
		log.Info().Strs("models", r.models).Msg("schedule " + what + " done")
	}
}
