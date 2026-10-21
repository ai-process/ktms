package nodeagent

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

const (
	maxGatewayBody = 128 << 20 // base64 images ride in chat bodies
	// An empty-choices chunk, not an SSE comment: openai-go fails on comment-only events.
	sseKeepalive  = "data: {\"object\":\"chat.completion.chunk\",\"choices\":[]}\n\n"
	jsonKeepalive = " " // leading whitespace is valid JSON
)

// Gateway is the only door to the engine that the proxy gets: a bearer check
// plus an allowlist of the routes it needs (/v1/chat/completions, /v1/decisions,
// /v1/models). Everything else (pull, delete, engine admin APIs) is unreachable.
type Gateway struct {
	upstream       string
	accessKey      string
	engineToken    string
	keepaliveAfter time.Duration
	keepaliveEvery time.Duration
	client         *http.Client
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

	var stream bool
	var body []byte
	switch {
	case r.Method == http.MethodPost && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/decisions" || r.URL.Path == "/v1/decisions/rollouts"):
		var err error
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxGatewayBody))
		if err != nil {
			writeGatewayError(w, http.StatusRequestEntityTooLarge, "request body too large or unreadable")
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			var probe struct {
				Stream bool `json:"stream"`
			}
			_ = json.Unmarshal(body, &probe) // a malformed body is the engine's to reject
			stream = probe.Stream
		}
	case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
	case r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/models" || r.URL.Path == "/v1/decisions" || r.URL.Path == "/v1/decisions/rollouts":
		writeGatewayError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	default:
		writeGatewayError(w, http.StatusNotFound, "route not exposed by this node")
		return
	}
	g.forward(w, r, body, stream)
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
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, body []byte, stream bool) {
	ctx := r.Context()
	req, err := http.NewRequestWithContext(ctx, r.Method, g.upstream+r.URL.Path, bytes.NewReader(body))
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
