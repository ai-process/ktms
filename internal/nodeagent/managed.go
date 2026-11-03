package nodeagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

// Where the agent takes its schedule from.
const (
	ScheduleSourceLocal = "local" // --schedule-file
	ScheduleSourceKTMS  = "ktms"  // what KTMS sends with each heartbeat; the file is ignored
)

// Settings are the owner's choices the tray changes at run time; they win over the flags.
type Settings struct {
	ScheduleSource string `json:"schedule_source"`
}

// managedSchedule is the last schedule KTMS sent, kept on disk so a restart
// without the proxy does not fall back to the local file.
type managedSchedule struct {
	Text       string    `json:"text"`
	Version    int64     `json:"version"` // 0: KTMS keeps none for this node, so 100%
	Override   *Override `json:"override,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
}

func (m *managedSchedule) override() *Override {
	if m == nil {
		return nil
	}
	return m.Override
}

func validSource(s string) bool { return s == ScheduleSourceLocal || s == ScheduleSourceKTMS }

func (a *Agent) stateFile(name string) string {
	if a.cfg.StateDir == "" {
		return ""
	}
	return filepath.Join(a.cfg.StateDir, name)
}

// readJSON fills v from path; a missing file is (false, nil).
func readJSON(path string, v any) (bool, error) {
	if path == "" {
		return false, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(data, v)
}

func writeJSON0600(path string, v any) error {
	if path == "" {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

// initSource picks the schedule source: the tray's saved choice, else the
// flag; in ktms mode it starts from the last schedule KTMS sent.
func (a *Agent) initSource() {
	a.source = sourceSetting(a.cfg)
	var m managedSchedule
	if ok, err := readJSON(a.stateFile("managed-schedule"), &m); err != nil {
		log.Warn().Err(err).Msg("cannot read the schedule last received from KTMS")
	} else if ok {
		a.pause.managed = &m
	}
	if a.source == ScheduleSourceKTMS {
		a.useManaged()
		log.Info().Int64("version", a.pause.managed.version()).Msg("the schedule is set by KTMS; the local schedule file is ignored")
	}
}

func (m *managedSchedule) version() int64 {
	if m == nil {
		return 0
	}
	return m.Version
}

// useManaged puts the schedule from KTMS in force.
func (a *Agent) useManaged() {
	p := &a.pause
	p.sched, p.content, p.schedErr = nil, nil, ""
	if m := p.managed; m != nil && m.Version > 0 {
		s, err := ParseSchedule(strings.NewReader(m.Text))
		if err != nil {
			p.schedErr = fmt.Sprintf("schedule from KTMS (version %d): %v", m.Version, err)
			return
		}
		p.sched = s
	}
}

// setScheduleSource switches between the local file and KTMS at run time and
// stores the choice.
func (a *Agent) setScheduleSource(src string) error {
	if !validSource(src) {
		return badRequest("schedule_source must be %q or %q", ScheduleSourceLocal, ScheduleSourceKTMS)
	}
	if err := writeJSON0600(a.stateFile("settings.json"), Settings{ScheduleSource: src}); err != nil {
		return fmt.Errorf("store the setting: %w", err)
	}
	if src == a.source {
		return nil
	}
	a.source = src
	p := &a.pause
	if src == ScheduleSourceKTMS {
		a.useManaged()
	} else {
		p.sched, p.content, p.schedErr = nil, nil, "" // the file is read on the next refresh
	}
	p.nextDirty = true
	a.armOverrideTimer()
	log.Info().Str("schedule_source", src).Msg("schedule source changed")
	return nil
}

// applyManaged takes a schedule KTMS sent and reports whether it changed
// anything. A schedule the agent cannot parse is refused and the last one holds.
func (a *Agent) applyManaged(ns *pb.NodeSchedule) bool {
	p := &a.pause
	if a.source != ScheduleSourceKTMS {
		return false
	}
	if ns == nil {
		if !p.warnedNoManaged {
			p.warnedNoManaged = true
			log.Warn().Int64("version", p.managed.version()).Msg("KTMS sent no schedule (it predates node schedules or could not read it); keeping the last one")
		}
		return false
	}
	p.warnedNoManaged = false
	m := &managedSchedule{Text: ns.GetText(), Version: ns.GetVersion(), ReceivedAt: a.now().UTC()}
	if o := ns.GetOverride(); o != nil && o.GetLevel() <= 100 {
		m.Override = &Override{Level: o.GetLevel(), Until: o.GetUntil().AsTime()}
	}
	if old := p.managed; old != nil && old.Version == m.Version && old.Text == m.Text && sameOverride(old.Override, m.Override) {
		return false
	}
	if m.Version > 0 && m.Version == p.managedBad {
		return false
	}
	if m.Version > 0 {
		if _, err := ParseSchedule(strings.NewReader(m.Text)); err != nil {
			p.managedBad = m.Version
			p.schedErr = fmt.Sprintf("schedule from KTMS (version %d): %v", m.Version, err)
			log.Error().Err(err).Int64("version", m.Version).Msg("the schedule from KTMS is invalid; keeping the last one")
			return false
		}
	}
	p.managed = m
	a.useManaged()
	p.nextDirty = true
	a.armOverrideTimer()
	if err := writeJSON0600(a.stateFile("managed-schedule"), m); err != nil {
		log.Warn().Err(err).Msg("cannot store the schedule from KTMS; a restart without the proxy runs at 100%")
	}
	log.Info().Int64("version", m.Version).Bool("override", m.Override != nil).Msg("schedule from KTMS applied")
	return true
}

func sameOverride(a, b *Override) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Level == b.Level && a.Until.Equal(b.Until)
}

// capacityUntil is the end of the override behind the level in force, if any;
// it asks the layers themselves, not p.source, which lags until the next refresh.
func (a *Agent) capacityUntil() *time.Time {
	p := &a.pause
	now := a.now()
	if p.override.activeAt(now) {
		return &p.override.Until
	}
	if o := p.managed.override(); a.source == ScheduleSourceKTMS && o.activeAt(now) {
		return &o.Until
	}
	return nil
}

// sourceSetting is the schedule source the agent starts with: the saved
// choice, else the flag. The doctor reads it without a running agent.
func sourceSetting(cfg AgentConfig) string {
	var s Settings
	if cfg.StateDir != "" {
		if ok, err := readJSON(filepath.Join(cfg.StateDir, "settings.json"), &s); ok && err == nil && validSource(s.ScheduleSource) {
			return s.ScheduleSource
		}
	}
	if cfg.ScheduleSource == "" {
		return ScheduleSourceLocal
	}
	return cfg.ScheduleSource
}

func timestampOrNil(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}
