package nodeagent

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/ai-process/ktms/internal/registry"
)

const (
	maxGatewayBody = 128 << 20 // base64 images ride in chat bodies
	// An empty-choices chunk, not an SSE comment: openai-go fails on comment-only events.
	sseKeepalive  = "data: {\"object\":\"chat.completion.chunk\",\"choices\":[]}\n\n"
	jsonKeepalive = " " // leading whitespace is valid JSON
)

// Gateway is the only door to the engine that the proxy gets: a bearer check
// plus an allowlist of the routes it needs (/v1/chat/completions, /v1/decisions,
// /v1/translate, /v1/models). Everything else (pull, delete, engine admin APIs) is unreachable.
type Gateway struct {
	upstream       string // the primary engine
	primary        string // its name
	extra          []gatewayEngine
	routes         atomic.Pointer[gatewayRoutes]
	accessKey      string
	engineToken    string
	keepaliveAfter time.Duration
	keepaliveEvery time.Duration
	client         *http.Client
	paused         atomic.Bool
	inflight       sync.Map // id -> context.CancelFunc of each engine request
	active         atomic.Int32
	onActive       func() // told each time active changes; set before serving
	seq            atomic.Uint64
}

// Active is how many engine requests run through the gateway now, all engines.
func (g *Gateway) Active() int32 { return g.active.Load() }

// Running lists the ids of the oldest n engine requests running now, oldest first.
func (g *Gateway) Running(n int) []uint64 {
	var ids []uint64
	g.inflight.Range(func(k, _ any) bool {
		ids = append(ids, k.(uint64))
		return true
	})
	slices.Sort(ids)
	return ids[:min(n, len(ids))]
}

func (g *Gateway) addActive(d int32) {
	g.active.Add(d)
	if g.onActive != nil {
		g.onActive()
	}
}

type gatewayEngine struct{ name, base string }

// gatewayRoutes tells which engine serves a model, and which engines the
// owner paused; set from each inspection.
type gatewayRoutes struct {
	byModel map[string]string
	paused  map[string]bool
}

// errAborted is the cause of a request the owner cut with CancelInflight.
var errAborted = errors.New("request stopped by the node's owner")

// CancelInflight cuts every engine request running now and returns how many.
func (g *Gateway) CancelInflight() int {
	n := 0
	g.inflight.Range(func(_, v any) bool {
		v.(context.CancelCauseFunc)(errAborted)
		n++
		return true
	})
	return n
}

// SetAccepting opens or closes the door to engine routes. Closed, the node
// answers 503: a proxy that ignores the schedule still cannot reach the engine.
func (g *Gateway) SetAccepting(ok bool) {
	g.paused.Store(!ok)
}

// NewGateway forwards to the engine at engineURL. accessKey is what callers
// must present; engineToken, if any, is what the engine itself expects.
func NewGateway(engineURL, accessKey, engineToken string) *Gateway {
	base := engineBaseURL(engineURL)
	return &Gateway{
		upstream:       base,
		accessKey:      accessKey,
		engineToken:    engineToken,
		keepaliveAfter: 20 * time.Second, // Cloudflare cuts a request with no response after 100s
		keepaliveEvery: 10 * time.Second,
		client:         &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DisableCompression: true}},
	}
}

// NewMultiGateway fronts several engines: the primary and extra, routed by
// the model a request names.
func NewMultiGateway(primary string, primaryURL string, extra []EngineSpec, accessKey, engineToken string) *Gateway {
	g := NewGateway(primaryURL, accessKey, engineToken)
	g.primary = strings.ToLower(primary)
	for _, e := range extra {
		g.extra = append(g.extra, gatewayEngine{name: e.Name, base: engineBaseURL(e.URL)})
	}
	return g
}

// SetRoutes replaces the model-to-engine map and the paused engines.
func (g *Gateway) SetRoutes(byModel map[string]string, paused []string) {
	rt := &gatewayRoutes{byModel: byModel, paused: map[string]bool{}}
	for _, p := range paused {
		rt.paused[p] = true
	}
	g.routes.Store(rt)
}

// pick is the engine for a request: the one serving its model, else for
// /v1/translate an mt engine, else the primary.
func (g *Gateway) pick(path, model string) (name, base string) {
	name = g.primary
	if rt := g.routes.Load(); rt != nil && rt.byModel[model] != "" {
		name = rt.byModel[model]
	} else if path == "/v1/translate" {
		for _, e := range g.extra {
			if e.name == registry.EngineMT {
				name = e.name
			}
		}
	}
	for _, e := range g.extra {
		if e.name == name {
			return name, e.base
		}
	}
	return name, g.upstream
}

func (g *Gateway) enginePaused(name string) bool {
	rt := g.routes.Load()
	return rt != nil && rt.paused[name]
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		_, _ = io.WriteString(w, "ok")
		return
	}
	if !g.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeGatewayError(w, http.StatusUnauthorized, "invalid node access key")
		return
	}
	if g.paused.Load() {
		writeGatewayError(w, http.StatusServiceUnavailable, "node paused by its owner")
		return
	}

	var stream bool
	var body []byte
	var model string
	switch {
	case r.Method == http.MethodPost && postRoutes[r.URL.Path]:
		var err error
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxGatewayBody))
		if err != nil {
			writeGatewayError(w, http.StatusRequestEntityTooLarge, "request body too large or unreadable")
			return
		}
		var probe struct {
			Stream bool   `json:"stream"`
			Model  string `json:"model"`
		}
		_ = json.Unmarshal(body, &probe) // a malformed body is the engine's to reject
		stream = probe.Stream && r.URL.Path == "/v1/chat/completions"
		model = probe.Model
	case r.URL.Path == "/v1/models" && r.Method == http.MethodGet && len(g.extra) > 0:
		g.listModels(w, r)
		return
	case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
	case r.URL.Path == "/v1/models" || postRoutes[r.URL.Path]:
		writeGatewayError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	default:
		writeGatewayError(w, http.StatusNotFound, "route not exposed by this node")
		return
	}
	engine, base := g.pick(r.URL.Path, model)
	if g.enginePaused(engine) {
		writeGatewayError(w, http.StatusServiceUnavailable, "engine "+engine+" paused by the node's owner")
		return
	}
	if r.Method == http.MethodPost {
		ctx, cancel := context.WithCancelCause(r.Context())
		id := g.seq.Add(1)
		g.inflight.Store(id, cancel)
		g.addActive(1)
		defer func() {
			g.inflight.Delete(id)
			g.addActive(-1)
			cancel(nil)
		}()
		r = r.WithContext(ctx)
	}
	g.forward(w, r, base, body, stream)
}

// postRoutes are the engine routes the proxy may POST to.
var postRoutes = map[string]bool{
	"/v1/chat/completions":   true,
	"/v1/decisions":          true,
	"/v1/decisions/rollouts": true,
	"/v1/translate":          true,
}

func (g *Gateway) authorized(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && g.accessKey != "" && subtle.ConstantTimeCompare([]byte(got), []byte(g.accessKey)) == 1
}

type upstreamResult struct {
	resp *http.Response
	err  error
}

// forward relays one request. After keepaliveAfter without an engine answer it
// commits a 200 and pads the body so idle timeouts hold; a later failure goes
// in the body, as the status is already sent.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, base string, body []byte, stream bool) {
	ctx := r.Context()
	req, err := http.NewRequestWithContext(ctx, r.Method, base+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		writeGatewayError(w, http.StatusBadGateway, "bad upstream request")
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if a := r.Header.Get("Accept"); a != "" {
		req.Header.Set("Accept", a)
	}
	if g.engineToken != "" {
		req.Header.Set("Authorization", "Bearer "+g.engineToken)
	}

	done := make(chan upstreamResult, 1)
	go func() {
		resp, err := g.client.Do(req)
		done <- upstreamResult{resp, err}
	}()

	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	keepalive, contentType := jsonKeepalive, "application/json"
	if stream {
		keepalive, contentType = sseKeepalive, "text/event-stream"
	}

	committed := false
	timer := time.NewTimer(g.keepaliveAfter)
	defer timer.Stop()
	var res upstreamResult
wait:
	for {
		select {
		case res = <-done:
			break wait
		case <-ctx.Done():
			go func() {
				if late := <-done; late.resp != nil {
					late.resp.Body.Close()
				}
			}()
			if errors.Is(context.Cause(ctx), errAborted) {
				g.fail(w, committed, stream, http.StatusServiceUnavailable, errAborted.Error())
			}
			return
		case <-timer.C:
			if !committed {
				committed = true
				w.Header().Set("Content-Type", contentType)
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("X-Accel-Buffering", "no")
				w.WriteHeader(http.StatusOK)
			}
			_, _ = io.WriteString(w, keepalive)
			flush()
			timer.Reset(g.keepaliveEvery)
		}
	}

	if res.err != nil && errors.Is(context.Cause(ctx), errAborted) {
		g.fail(w, committed, stream, http.StatusServiceUnavailable, errAborted.Error())
		return
	}
	if res.err != nil {
		g.fail(w, committed, stream, http.StatusBadGateway, "engine unreachable: "+res.err.Error())
		return
	}
	defer res.resp.Body.Close()

	if res.resp.StatusCode/100 != 2 {
		detail, _ := io.ReadAll(io.LimitReader(res.resp.Body, 4096))
		g.fail(w, committed, stream, res.resp.StatusCode, fmt.Sprintf("engine returned %d: %s", res.resp.StatusCode, strings.TrimSpace(string(detail))))
		return
	}

	if !committed {
		for _, h := range []string{"Content-Type", "Cache-Control"} {
			if v := res.resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(res.resp.StatusCode)
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := res.resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			flush()
		}
		if err != nil {
			if errors.Is(context.Cause(ctx), errAborted) {
				g.fail(w, true, stream, http.StatusServiceUnavailable, errAborted.Error())
			}
			return
		}
	}
}

// fail reports an upstream problem: as a status if nothing was sent yet,
// otherwise inside the already-open 200 in the shape the client parses.
func (g *Gateway) fail(w http.ResponseWriter, committed, stream bool, status int, msg string) {
	log.Warn().Int("status", status).Bool("after_keepalive", committed).Str("reason", msg).Msg("gateway: engine request failed")
	if !committed {
		writeGatewayError(w, status, msg)
		return
	}
	payload := gatewayErrorBody(msg)
	if stream {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	} else {
		_, _ = w.Write(payload)
	}
}

func gatewayErrorBody(msg string) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "node_gateway_error"}})
	return b
}

func writeGatewayError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(gatewayErrorBody(msg))
}

// listModels merges /v1/models of the engines that are not paused; one that
// fails is left out, and only none answering is an error.
func (g *Gateway) listModels(w http.ResponseWriter, r *http.Request) {
	engines := append([]gatewayEngine{{name: g.primary, base: g.upstream}}, g.extra...)
	data := []json.RawMessage{}
	answered := 0
	for _, e := range engines {
		if g.enginePaused(e.name) {
			continue
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, e.base+"/v1/models", nil)
		if err != nil {
			continue
		}
		if g.engineToken != "" {
			req.Header.Set("Authorization", "Bearer "+g.engineToken)
		}
		resp, err := g.client.Do(req)
		if err != nil {
			log.Warn().Err(err).Str("engine", e.name).Msg("gateway: model list failed")
			continue
		}
		var list struct {
			Data []json.RawMessage `json:"data"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&list)
		resp.Body.Close()
		if err != nil || resp.StatusCode/100 != 2 {
			log.Warn().Err(err).Int("status", resp.StatusCode).Str("engine", e.name).Msg("gateway: model list failed")
			continue
		}
		answered++
		data = append(data, list.Data...)
	}
	if answered == 0 {
		writeGatewayError(w, http.StatusBadGateway, "no engine listed its models")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
