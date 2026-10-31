package nodeagent

import (
	"bytes"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

// Node states the control API reports.
const (
	StateActive    = "active"
	StateThrottled = "throttled" // between 1% and 99%
	StatePausing   = "pausing"   // at 0%, proxy requests still running
	StatePaused    = "paused"
	StateDraining  = "draining"
	StateOffline   = "offline" // not registered, or the last heartbeat failed
)

// Status is what the owner sees of the running agent. It carries no key or token.
type Status struct {
	Version         string         `json:"version"`
	NodeID          string         `json:"node_id"`
	Proxy           string         `json:"proxy"`
	Connection      StatusConn     `json:"connection"`
	Level           StatusLevel    `json:"level"`
	State           string         `json:"state"`
	Requests        StatusRequests `json:"requests"`
	CapacityApplied bool           `json:"capacity_applied"` // false: the proxy predates levels and routes here at 100%
	Models          []StatusModel  `json:"models"`
	GPU             *StatusGPU     `json:"gpu,omitempty"`
	Endpoint        string         `json:"endpoint"`
	ScheduleSource  string         `json:"schedule_source"` // local | ktms
	ScheduleFile    string         `json:"schedule_file"`
	ScheduleError   string         `json:"schedule_error"`
	CanAbort        bool           `json:"can_abort"` // the built-in gateway can cut running requests
	UpdatedAt       time.Time      `json:"updated_at"`
}

type StatusConn struct {
	Registered      bool      `json:"registered"`
	LastHeartbeatAt time.Time `json:"last_heartbeat_at,omitzero"`
	LastError       string    `json:"last_error"`
}

type StatusLevel struct {
	Effective uint32      `json:"effective"`
	Schedule  uint32      `json:"schedule"`           // what the schedule alone says now
	Source    string      `json:"source"`             // schedule | override | ktms_schedule | ktms_override | default
	Override  *Override   `json:"override,omitempty"` // the owner's, set on this machine
	KTMS      *StatusKTMS `json:"ktms,omitempty"`     // in ktms mode: what KTMS last sent
	Next      *StatusNext `json:"next,omitempty"`
}

type StatusKTMS struct {
	Version    int64     `json:"version"` // 0: none received, or KTMS keeps none (100%)
	Override   *Override `json:"override,omitempty"`
	ReceivedAt time.Time `json:"received_at,omitzero"`
}

type StatusNext struct {
	At     time.Time `json:"at"`
	Level  uint32    `json:"level"`
	Source string    `json:"source"` // schedule | override_end
}

type StatusRequests struct {
	ProxyActive   int32  `json:"proxy_active"` // -1 when not yet known
	EngineRunning uint32 `json:"engine_running"`
	EngineWaiting uint32 `json:"engine_waiting"`
}

type StatusModel struct {
	ID   string `json:"id"`
	Warm bool   `json:"warm"`
}

type StatusGPU struct {
	UtilPercent float64 `json:"util_percent"`
	TempC       uint32  `json:"temp_c"`
	VRAMFreeMB  uint64  `json:"vram_free_mb"`
	VRAMTotalMB uint64  `json:"vram_total_mb"`
}

// connState is the agent's view of the proxy; only Run's goroutine touches it.
type connState struct {
	registered      bool
	lastBeat        time.Time
	lastErr         string
	capacityApplied bool
}

// statusHub hands the latest Status to API goroutines and wakes event
// streams when it changes.
type statusHub struct {
	cur     atomic.Pointer[Status]
	mu      sync.Mutex
	changed chan struct{}
	last    []byte // JSON of cur without UpdatedAt, to tell a real change
}

func (h *statusHub) get() *Status { return h.cur.Load() }

// wait returns a channel closed at the next change.
func (h *statusHub) wait() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.changed == nil {
		h.changed = make(chan struct{})
	}
	return h.changed
}

func (h *statusHub) set(s *Status) {
	cmp := *s
	cmp.UpdatedAt = time.Time{}
	key, _ := json.Marshal(cmp)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur.Store(s)
	if bytes.Equal(key, h.last) {
		return
	}
	h.last = key
	if h.changed != nil {
		close(h.changed)
		h.changed = nil
	}
}

// publish snapshots the agent for the control API; Run's goroutine calls it.
func (a *Agent) publish() {
	p := &a.pause
	now := a.now()
	a.mu.Lock()
	draining := a.draining
	a.mu.Unlock()
	s := &Status{
		Version:  a.cfg.Version,
		NodeID:   a.cfg.NodeID,
		Proxy:    a.cfg.ProxyAddr,
		Endpoint: a.currentEndpoint(),
		Connection: StatusConn{
			Registered:      a.conn.registered,
			LastHeartbeatAt: a.conn.lastBeat,
			LastError:       a.conn.lastErr,
		},
		Level: StatusLevel{
			Effective: p.level,
			Schedule:  p.sched.Level(now),
			Source:    p.source,
		},
		Requests: StatusRequests{
			ProxyActive:   p.active,
			EngineRunning: a.stats.beat.EngineRunning,
			EngineWaiting: a.stats.beat.EngineWaiting,
		},
		CapacityApplied: a.conn.capacityApplied,
		ScheduleSource:  a.source,
		ScheduleFile:    a.cfg.ScheduleFile,
		ScheduleError:   p.schedErr,
		CanAbort:        a.gateway != nil,
		UpdatedAt:       now,
	}
	if p.override.activeAt(now) {
		o := *p.override
		s.Level.Override = &o
	}
	if a.source == ScheduleSourceKTMS {
		k := &StatusKTMS{}
		if m := p.managed; m != nil {
			k.Version, k.ReceivedAt = m.Version, m.ReceivedAt
			if m.Override.activeAt(now) {
				o := *m.Override
				k.Override = &o
			}
		}
		s.Level.KTMS = k
	}
	if p.nextOK {
		s.Level.Next = &StatusNext{At: p.nextAt, Level: p.next, Source: p.nextSrc}
	}
	for _, m := range p.models {
		s.Models = append(s.Models, StatusModel{ID: m.GetModelId(), Warm: m.GetIsWarm()})
	}
	if b := a.stats.beat; b.VRAMTotalMB > 0 || b.GPUUtil > 0 {
		s.GPU = &StatusGPU{UtilPercent: b.GPUUtil, TempC: b.GPUTempC, VRAMFreeMB: b.VRAMFreeMB, VRAMTotalMB: b.VRAMTotalMB}
	}
	switch {
	case !a.conn.registered || a.conn.lastErr != "":
		s.State = StateOffline
	case draining:
		s.State = StateDraining
	case p.level == 0 && (!p.gateClosed || p.active != 0):
		s.State = StatePausing
	case p.level == 0:
		s.State = StatePaused
	case p.level < 100:
		s.State = StateThrottled
	default:
		s.State = StateActive
	}
	a.status.set(s)
}
