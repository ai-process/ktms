package nodeagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// ctlTimeout bounds a command's wait for Run's goroutine; tests shorten it.
var ctlTimeout = 5 * time.Second

const (
	eventsEvery    = 10 * time.Second // an idle event stream still hears from the agent this often
	maxControlBody = 1 << 20
)

// DefaultHome is the node's directory: the installer's, else ~/.ktms-node.
func DefaultHome() string {
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
	return base
}

// DefaultControlSocket is where the agent listens for its owner by default.
func DefaultControlSocket() string {
	if h := DefaultHome(); h != "" {
		return filepath.Join(h, "control.sock")
	}
	return ""
}

// ctlCmd runs on Run's goroutine, the only one that touches pauseState.
type ctlCmd struct {
	run   func() (any, error)
	reply chan ctlReply
}

type ctlReply struct {
	v   any
	err error
}

var errAgentBusy = errors.New("the agent is not serving yet (still registering with the proxy); try again shortly")

// do hands fn to Run's goroutine and waits for its answer.
func (a *Agent) do(ctx context.Context, fn func() (any, error)) (any, error) {
	c := ctlCmd{run: fn, reply: make(chan ctlReply, 1)}
	t := time.NewTimer(ctlTimeout)
	defer t.Stop()
	select {
	case a.ctl <- c:
	case <-t.C:
		return nil, errAgentBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case r := <-c.reply:
		return r.v, r.err
	case <-t.C:
		return nil, errAgentBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// runCommand applies a command and answers before the heartbeat that tells
// the proxy, so a slow proxy does not hold the owner's click.
func (a *Agent) runCommand(c ctlCmd, beat func()) {
	v, err := c.run()
	changed := a.refreshLevel()
	if !changed {
		a.stepPause()
	}
	a.publish()
	c.reply <- ctlReply{v, err}
	if changed {
		beat()
		a.publish()
	}
}

// startControl serves the owner's API on a Unix socket. A socket that cannot
// be opened leaves the node serving without it.
func (a *Agent) startControl() (stop func()) {
	path := a.cfg.ControlSocket
	if path == "" {
		return func() {}
	}
	lis, err := listenControl(path)
	if err != nil {
		log.Warn().Err(err).Str("socket", path).Msg("control API unavailable; the tray and `ktms-node status` will not reach this agent")
		return func() {}
	}
	srv := &http.Server{
		Handler:           a.controlHandler(),
		ReadHeaderTimeout: 5 * time.Second,
		ConnContext:       withPeerUID,
	}
	go func() {
		if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn().Err(err).Msg("control API stopped")
		}
	}()
	log.Info().Str("socket", path).Msg("control API listening")
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx) // event streams never finish on their own
		_ = srv.Close()
		_ = os.Remove(path)
	}
}

// listenControl opens the socket owner-only. A stale socket from a crashed
// agent is replaced; one another agent still answers on is not.
func listenControl(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("another agent already listens on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		lis.Close()
		return nil, err
	}
	return lis, nil
}

func (a *Agent) controlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", a.handleStatus)
	mux.HandleFunc("GET /v1/events", a.handleEvents)
	mux.HandleFunc("POST /v1/override", a.handleSetOverride)
	mux.HandleFunc("DELETE /v1/override", a.handleClearOverride)
	mux.HandleFunc("GET /v1/schedule", a.handleGetSchedule)
	mux.HandleFunc("PUT /v1/schedule", a.handlePutSchedule)
	mux.HandleFunc("POST /v1/schedule/preview", a.handlePreviewSchedule)
	mux.HandleFunc("GET /v1/stats", a.handleStats)
	mux.HandleFunc("GET /v1/settings", a.handleGetSettings)
	mux.HandleFunc("PUT /v1/settings", a.handlePutSettings)
	return ownerOnly(mux)
}

// ControlError is the body of every failed control call.
type ControlError struct {
	Error string `json:"error"`
	Line  int    `json:"line,omitempty"` // schedule line at fault
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeCtlError(w http.ResponseWriter, err error) {
	var bad errBadRequest
	switch {
	case errors.As(err, &bad):
		writeJSON(w, http.StatusUnprocessableEntity, ControlError{Error: err.Error()})
	case errors.Is(err, errAgentBusy):
		writeJSON(w, http.StatusServiceUnavailable, ControlError{Error: err.Error()})
	case errors.Is(err, errConflict):
		writeJSON(w, http.StatusConflict, ControlError{Error: strings.TrimPrefix(err.Error(), errConflict.Error()+": ")})
	default:
		writeJSON(w, http.StatusInternalServerError, ControlError{Error: err.Error()})
	}
}

var errConflict = errors.New("conflict")

func conflict(msg string) error { return fmt.Errorf("%w: %s", errConflict, msg) }

func (a *Agent) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.status.get())
}

// handleEvents streams the status as server-sent events: on every change,
// and every eventsEvery when nothing changes.
func (a *Agent) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, ControlError{Error: "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	tick := time.NewTicker(eventsEvery)
	defer tick.Stop()
	for {
		changed := a.status.wait()
		b, _ := json.Marshal(a.status.get())
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return
		}
		fl.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-changed:
		case <-tick.C:
		}
	}
}

// OverrideResult answers POST /v1/override.
type OverrideResult struct {
	Status  *Status `json:"status"`
	Aborted int     `json:"aborted"` // requests cut by abort_inflight
}

func (a *Agent) handleSetOverride(w http.ResponseWriter, r *http.Request) {
	var req OverrideRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxControlBody)).Decode(&req); err != nil {
		writeCtlError(w, badRequest("bad JSON: %v", err))
		return
	}
	v, err := a.do(r.Context(), func() (any, error) {
		now := a.now()
		// The switch the owner was shown: overrides included, not the schedule alone.
		next, _, ok, _ := a.nextSwitch(now)
		o, err := req.resolve(now, next, ok)
		if err != nil {
			return nil, err
		}
		if req.AbortInflight && (o.Level != 0 || a.gateway == nil) {
			return nil, badRequest("abort_inflight needs level 0 and the built-in gateway (--expose=cloudflared)")
		}
		a.setOverride(o)
		log.Info().Uint32("level", o.Level).Time("until", o.Until).Bool("warm", o.Warm).Msg("manual level set")
		aborted := 0
		if req.AbortInflight {
			// The gate closes now, not after the grace period: new requests must not slip in behind the cut.
			a.gateway.SetAccepting(false)
			aborted = a.gateway.CancelInflight()
			log.Info().Int("requests", aborted).Msg("running requests cut by the owner")
		}
		return aborted, nil
	})
	if err != nil {
		writeCtlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, OverrideResult{Status: a.status.get(), Aborted: v.(int)})
}

func (a *Agent) handleClearOverride(w http.ResponseWriter, r *http.Request) {
	_, err := a.do(r.Context(), func() (any, error) {
		if a.pause.override != nil {
			log.Info().Msg("manual level cleared; following the schedule")
			a.setOverride(nil)
		}
		return nil, nil
	})
	if err != nil {
		writeCtlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.status.get())
}

// ScheduleInfo is the schedule file as the editor sees it.
type ScheduleInfo struct {
	File     string       `json:"file"`
	Text     string       `json:"text"`
	Exists   bool         `json:"exists"`
	Error    string       `json:"error,omitempty"` // the file is invalid; the agent keeps the last valid schedule
	Line     int          `json:"line,omitempty"`
	Days     []DayWindows `json:"days,omitempty"`
	Level    uint32       `json:"level"` // what this text gives now
	Upcoming []Switch     `json:"upcoming,omitempty"`
	Editable bool         `json:"editable"`
	Source   string       `json:"source,omitempty"`    // local | ktms
	ReadOnly string       `json:"read_only,omitempty"` // why it is not editable
}

var lineRE = regexp.MustCompile(`^line (\d+)`)

// describeSchedule parses text for the editor; a bad text fills Error and Line.
func describeSchedule(text string, now time.Time) ScheduleInfo {
	info := ScheduleInfo{Text: text}
	s, err := ParseSchedule(bytes.NewReader([]byte(text)))
	if err != nil {
		info.Error = err.Error()
		if m := lineRE.FindStringSubmatch(info.Error); m != nil {
			info.Line, _ = strconv.Atoi(m[1])
		}
		return info
	}
	info.Days = s.Days()
	info.Level = s.Level(now)
	info.Upcoming = s.Upcoming(now, 5)
	return info
}

func (a *Agent) handleGetSchedule(w http.ResponseWriter, _ *http.Request) {
	info := ScheduleInfo{}
	if a.sourceNow() == ScheduleSourceKTMS {
		var m managedSchedule
		if _, err := readJSON(a.stateFile("managed-schedule"), &m); err != nil {
			writeCtlError(w, err)
			return
		}
		info = describeSchedule(m.Text, a.now())
		info.Exists = m.Version > 0
		info.ReadOnly = "the schedule is set in KTMS"
		info.Source = ScheduleSourceKTMS
		writeJSON(w, http.StatusOK, info)
		return
	}
	if f := a.cfg.ScheduleFile; f != "" {
		data, err := os.ReadFile(f)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			writeCtlError(w, err)
			return
		}
		info = describeSchedule(string(data), a.now())
		info.Exists = err == nil
	} else {
		info = describeSchedule("", a.now())
		info.ReadOnly = "the agent runs without --schedule-file"
	}
	info.File, info.Source = a.cfg.ScheduleFile, ScheduleSourceLocal
	info.Editable = info.ReadOnly == ""
	writeJSON(w, http.StatusOK, info)
}

// sourceNow is the schedule source as last published, for API goroutines.
func (a *Agent) sourceNow() string {
	if s := a.status.get(); s != nil {
		return s.ScheduleSource
	}
	return ScheduleSourceLocal
}

func (a *Agent) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Settings{ScheduleSource: a.sourceNow()})
}

// handlePutSettings switches the schedule source without a restart.
func (a *Agent) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var s Settings
	if err := json.NewDecoder(io.LimitReader(r.Body, maxControlBody)).Decode(&s); err != nil {
		writeCtlError(w, badRequest("bad JSON: %v", err))
		return
	}
	if _, err := a.do(r.Context(), func() (any, error) { return nil, a.setScheduleSource(s.ScheduleSource) }); err != nil {
		writeCtlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.status.get())
}

type scheduleBody struct {
	Text string `json:"text"`
}

func readScheduleBody(r *http.Request) (string, error) {
	var b scheduleBody
	if err := json.NewDecoder(io.LimitReader(r.Body, maxControlBody)).Decode(&b); err != nil {
		return "", badRequest("bad JSON: %v", err)
	}
	return b.Text, nil
}

// handlePutSchedule validates and writes the schedule file; the agent picks
// it up at once. An invalid text leaves the file untouched.
func (a *Agent) handlePutSchedule(w http.ResponseWriter, r *http.Request) {
	if a.sourceNow() == ScheduleSourceKTMS {
		writeCtlError(w, conflict("the schedule is set in KTMS; change it there, or switch the node back to its own schedule"))
		return
	}
	if a.cfg.ScheduleFile == "" {
		writeCtlError(w, conflict("the agent runs without --schedule-file, so there is no file to save to"))
		return
	}
	text, err := readScheduleBody(r)
	if err != nil {
		writeCtlError(w, err)
		return
	}
	info := describeSchedule(text, a.now())
	if info.Error != "" {
		writeJSON(w, http.StatusUnprocessableEntity, ControlError{Error: info.Error, Line: info.Line})
		return
	}
	if err := writeFileAtomic(a.cfg.ScheduleFile, []byte(text)); err != nil {
		writeCtlError(w, err)
		return
	}
	// The file is saved either way; a busy agent reads it on its next level tick.
	if _, err := a.do(r.Context(), func() (any, error) { return nil, nil }); err != nil && !errors.Is(err, errAgentBusy) {
		writeCtlError(w, err)
		return
	}
	info.File, info.Exists, info.Editable = a.cfg.ScheduleFile, true, true
	writeJSON(w, http.StatusOK, info)
}

func (a *Agent) handlePreviewSchedule(w http.ResponseWriter, r *http.Request) {
	text, err := readScheduleBody(r)
	if err != nil {
		writeCtlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, describeSchedule(text, a.now()))
}

// handleStats is `ktms-node stats --format json` over the agent's own log.
func (a *Agent) handleStats(w http.ResponseWriter, r *http.Request) {
	if a.cfg.StatsDir == "" {
		writeCtlError(w, conflict("the agent keeps no usage log (--stats=false)"))
		return
	}
	now := a.now()
	q := r.URL.Query()
	from, to := now.Add(-24*time.Hour), now
	var err error
	if v := q.Get("from"); v != "" {
		if from, err = ParseWhen(v, now); err != nil {
			writeCtlError(w, badRequest("from: %v", err))
			return
		}
	}
	if v := q.Get("to"); v != "" {
		if to, err = ParseWhen(v, now); err != nil {
			writeCtlError(w, badRequest("to: %v", err))
			return
		}
	}
	if !to.After(from) {
		writeCtlError(w, badRequest("to must be after from"))
		return
	}
	by := q.Get("by")
	switch by {
	case "":
		by = "hour"
		if to.Sub(from) > 48*time.Hour {
			by = "day"
		}
	case "hour", "day", "model":
	default:
		writeCtlError(w, badRequest("by must be hour, day or model"))
		return
	}
	st, err := OpenStats(a.cfg.StatsDir, false)
	if err != nil {
		writeCtlError(w, err)
		return
	}
	rep, err := BuildReport(st, from, to, now, by)
	if err != nil {
		writeCtlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
