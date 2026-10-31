package nodetray

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ai-process/ktms/internal/nodeagent"
)

//go:embed web/index.html
var webFS embed.FS

const (
	cookieName   = "ktms_tray"
	csrfHeader   = "X-KTMS-Tray"
	slotsPerWeek = 7 * 48 // the grid edits half hours
	ticketTTL    = 2 * time.Minute
)

// Prefs are the tray's own settings.
type Prefs struct {
	Notifications bool `json:"notifications"`
}

// Web is the settings window: a page on 127.0.0.1 that only a browser holding
// a one-time ticket opens. It relays the agent's control API to the page.
type Web struct {
	socket    string
	prefsFile string
	session   string // the cookie a used ticket turns into
	addr      string
	srv       *http.Server

	mu      sync.Mutex
	prefs   Prefs
	tickets map[string]time.Time // unused tickets and when they expire
}

func randomHex() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// StartWeb serves the window on a random local port.
func StartWeb(socket, prefsFile string) (*Web, error) {
	session, err := randomHex()
	if err != nil {
		return nil, err
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	w := &Web{socket: socket, prefsFile: prefsFile, session: session, addr: lis.Addr().String(), prefs: LoadPrefs(prefsFile), tickets: map[string]time.Time{}}
	w.srv = &http.Server{Handler: w.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = w.srv.Serve(lis) }()
	return w, nil
}

// URL opens the window at a section (status, schedule, stats or settings)
// with a ticket that works once, within ticketTTL.
func (w *Web) URL(section string) (string, error) {
	t, err := randomHex()
	if err != nil {
		return "", err
	}
	w.mu.Lock()
	now := time.Now()
	for k, exp := range w.tickets {
		if now.After(exp) {
			delete(w.tickets, k)
		}
	}
	w.tickets[t] = now.Add(ticketTTL)
	w.mu.Unlock()
	return fmt.Sprintf("http://%s/?t=%s#%s", w.addr, t, section), nil
}

// OpenFile writes an owner-only page that forwards to URL(section) and
// returns its path. Opening the file rather than the URL keeps the ticket
// out of the opener's arguments, which every local user can list.
func (w *Web) OpenFile(section string) (string, error) {
	u, err := w.URL(section)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "ktms-tray-*.html") // 0600
	if err != nil {
		return "", err
	}
	page := fmt.Sprintf("<!doctype html><meta charset=utf-8><meta http-equiv=refresh content=\"0;url=%s\"><title>Keystone node</title>", u)
	if _, err := f.WriteString(page); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	time.AfterFunc(time.Minute, func() { _ = os.Remove(f.Name()) })
	return f.Name(), nil
}

// useTicket consumes a ticket; a used or expired one fails.
func (w *Web) useTicket(t string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	exp, ok := w.tickets[t]
	delete(w.tickets, t)
	return ok && time.Now().Before(exp)
}

func (w *Web) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return w.srv.Shutdown(ctx)
}

func (w *Web) Prefs() Prefs {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.prefs
}

// Handler guards every route: the Host must be this listener (no DNS
// rebinding), the cookie must hold the token, and writes need a header a
// cross-site form cannot send.
func (w *Web) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", w.handleIndex)
	mux.Handle("/api/v1/", w.agentProxy())
	mux.HandleFunc("POST /api/format", handleFormat)
	mux.HandleFunc("GET /api/prefs", w.handleGetPrefs)
	mux.HandleFunc("PUT /api/prefs", w.handlePutPrefs)
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Host != w.addr {
			http.Error(rw, "wrong host", http.StatusForbidden)
			return
		}
		if t := r.URL.Query().Get("t"); t != "" && r.Method == http.MethodGet && r.URL.Path == "/" {
			if !w.useTicket(t) {
				http.Error(rw, "this link was used or has expired; open the window from the tray menu again", http.StatusForbidden)
				return
			}
			http.SetCookie(rw, &http.Cookie{Name: cookieName, Value: w.session, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(rw, r, "/", http.StatusSeeOther) // the token leaves the address bar and history
			return
		}
		c, err := r.Cookie(cookieName)
		if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(w.session)) != 1 {
			http.Error(rw, "open this window from the tray menu", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get(csrfHeader) != "1" {
			http.Error(rw, "missing "+csrfHeader, http.StatusForbidden)
			return
		}
		rw.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; frame-ancestors 'none'")
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(rw, r)
	})
}

func (w *Web) handleIndex(rw http.ResponseWriter, _ *http.Request) {
	page, _ := webFS.ReadFile("web/index.html")
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	_, _ = rw.Write(page)
}

// agentProxy relays /api/v1/... to the agent's socket as /v1/...; event
// streams flush as they come.
func (w *Web) agentProxy() http.Handler {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", w.socket)
	}}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "agent"
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, "/api")
			pr.Out.URL.RawPath = ""
			pr.Out.Host = "agent"
			pr.Out.Header.Del("Cookie")
		},
		Transport:     tr,
		FlushInterval: -1,
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, err error) {
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(rw).Encode(nodeagent.ControlError{Error: "the node agent does not answer: " + err.Error()})
		},
	}
}

// handleFormat turns the grid (half hours, Monday 00:00 first; 100 or -1 =
// no rule) into schedule text.
func handleFormat(rw http.ResponseWriter, r *http.Request) {
	var in struct {
		Week []int `json:"week"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in); err != nil || len(in.Week) != slotsPerWeek {
		writeErr(rw, http.StatusUnprocessableEntity, fmt.Sprintf("want %d half-hour levels", slotsPerWeek))
		return
	}
	week := make([]int8, slotsPerWeek*30)
	for i, l := range in.Week {
		if l < -1 || l > 100 {
			writeErr(rw, http.StatusUnprocessableEntity, fmt.Sprintf("bad level %d", l))
			return
		}
		if l == 100 {
			l = -1 // uncovered time runs at 100%: no rule needed
		}
		for m := range 30 {
			week[i*30+m] = int8(l)
		}
	}
	text, err := nodeagent.FormatSchedule(week)
	if err != nil {
		writeErr(rw, http.StatusUnprocessableEntity, err.Error())
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(map[string]string{"text": text})
}

func (w *Web) handleGetPrefs(rw http.ResponseWriter, _ *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(w.Prefs())
}

func (w *Web) handlePutPrefs(rw http.ResponseWriter, r *http.Request) {
	var p Prefs
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&p); err != nil {
		writeErr(rw, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := SavePrefs(w.prefsFile, p); err != nil {
		writeErr(rw, http.StatusInternalServerError, err.Error())
		return
	}
	w.mu.Lock()
	w.prefs = p
	w.mu.Unlock()
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(p)
}

func writeErr(rw http.ResponseWriter, status int, msg string) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(nodeagent.ControlError{Error: msg})
}

// LoadPrefs reads the tray's settings; notifications are on by default.
func LoadPrefs(path string) Prefs {
	p := Prefs{Notifications: true}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &p) // a broken file keeps the defaults
	}
	return p
}

func SavePrefs(path string, p Prefs) error {
	if path == "" {
		return errors.New("no settings file")
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
