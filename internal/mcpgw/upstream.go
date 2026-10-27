// Package mcpgw is Keystone's MCP gateway: clients reach registered MCP
// servers through /mcp with a KTMS key, and the gateway holds the servers'
// credentials, enforces each key's allowed_mcp and tool_rpm, and accounts for
// every tool call.
package mcpgw

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/safefetch"
)

// toolsTTL is how long a server's tool list is trusted; the gateway asks for
// no change notifications, so a new tool shows up within this.
const toolsTTL = time.Minute

// envelope is room for the JSON-RPC wrapping around a result of max size.
const envelope = 64 << 10

var (
	errTooLarge = errors.New("mcp server response exceeds max_response_bytes")
	errRedirect = errors.New("mcp server redirects are not followed")
)

// cutKey marks a call's context: a reply body cut at the size cap sets it. The
// SDK reports a body read error without wrapping it, so only the mark tells.
type cutKey struct{}

func withCutMark(ctx context.Context) (context.Context, *atomic.Bool) {
	cut := &atomic.Bool{}
	return context.WithValue(ctx, cutKey{}, cut), cut
}

// upstream is one shared client session to a server, made with the spec and
// credential its fingerprint names. Sessions are reconnected on demand.
type upstream struct {
	srv *registry.MCPServer
	fp  string

	mu      sync.Mutex
	session *mcp.ClientSession
	cancel  context.CancelFunc // ends the session's streams
	tools   []*mcp.Tool
	toolsAt time.Time // zero: no list cached
	retired bool      // replaced or removed: never connects again
}

var errRetired = errors.New("mcp server was reconfigured or removed during the call")

// connect returns the live session, making one if there is none. The session
// outlives the request that made it: an SSE stream is bound to the context
// it was opened with, so that one ends only when the session is dropped.
func (u *upstream) connect(version string) (*mcp.ClientSession, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.retired {
		return nil, errRetired
	}
	if u.session != nil {
		return u.session, nil
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "ktms-mcp-gateway", Version: version}, nil)
	// A redirect would carry the credential to whatever host it names.
	hc := &http.Client{Transport: &credentialTransport{srv: u.srv, base: transport(u.srv)},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect }}
	var t mcp.Transport
	if u.srv.Transport == registry.MCPSSE {
		t = &mcp.SSEClientTransport{Endpoint: u.srv.URL, HTTPClient: hc, MaxEventSize: int(u.srv.MaxResponse() + envelope)}
	} else {
		t = &mcp.StreamableClientTransport{Endpoint: u.srv.URL, HTTPClient: hc, MaxRetries: -1,
			DisableStandaloneSSE: true, MaxEventSize: int(u.srv.MaxResponse() + envelope)}
	}
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(u.srv.Timeout(), cancel)
	s, err := client.Connect(ctx, t, nil)
	if !timer.Stop() && err == nil {
		err = context.DeadlineExceeded
	}
	if err != nil {
		cancel()
		if s != nil {
			_ = s.Close()
		}
		return nil, err
	}
	u.session, u.cancel = s, cancel
	return s, nil
}

// drop forgets the session s, if it is still the current one.
func (u *upstream) drop(s *mcp.ClientSession) {
	u.mu.Lock()
	if u.session != s {
		u.mu.Unlock()
		return
	}
	cancel := u.cancel
	u.session, u.cancel, u.tools, u.toolsAt = nil, nil, nil, time.Time{}
	u.mu.Unlock()
	_ = s.Close() // the session is unusable either way
	if cancel != nil {
		cancel()
	}
}

// retire closes the upstream for good, when the pool replaces or drops it.
func (u *upstream) retire() {
	u.mu.Lock()
	u.retired = true
	u.mu.Unlock()
	u.close()
}

func (u *upstream) close() {
	u.mu.Lock()
	s, cancel := u.session, u.cancel
	u.session, u.cancel, u.tools, u.toolsAt = nil, nil, nil, time.Time{}
	u.mu.Unlock()
	if s != nil {
		_ = s.Close() // the session is unusable either way
	}
	if cancel != nil {
		cancel()
	}
}

// listTools returns the server's tools, from the cache while it is fresh.
func (u *upstream) listTools(ctx context.Context, version string) ([]*mcp.Tool, error) {
	u.mu.Lock()
	if !u.toolsAt.IsZero() && time.Since(u.toolsAt) < toolsTTL {
		t := u.tools
		u.mu.Unlock()
		return t, nil
	}
	u.mu.Unlock()

	// A list is safe to ask twice: one retry covers a session the server dropped.
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var s *mcp.ClientSession
		if s, err = u.connect(version); err != nil {
			return nil, err
		}
		tools := []*mcp.Tool{}
		lctx, cancel := context.WithTimeout(ctx, u.srv.Timeout())
		for t, lerr := range s.Tools(lctx, nil) {
			if lerr != nil {
				err = lerr
				break
			}
			tools = append(tools, t)
		}
		cancel()
		if err == nil {
			u.mu.Lock()
			if u.session == s {
				u.tools, u.toolsAt = tools, time.Now()
			}
			u.mu.Unlock()
			return tools, nil
		}
		if !transportError(err) {
			return nil, err
		}
		u.drop(s)
	}
	return nil, err
}

// call forwards one tools/call. Only a call the server refused for an unknown
// session is retried: a tool may not be safe to run twice, and a closed
// connection does not say whether the call ran.
func (u *upstream) call(ctx context.Context, version string, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	for attempt := 0; ; attempt++ {
		s, err := u.connect(version)
		if err != nil {
			return nil, err
		}
		res, err := s.CallTool(ctx, p)
		if err == nil {
			return res, nil
		}
		if transportError(err) {
			u.drop(s)
			if attempt == 0 && errors.Is(err, mcp.ErrSessionMissing) {
				continue
			}
		}
		return nil, err
	}
}

// transportError reports an error that is not the server's JSON-RPC answer:
// the session is then suspect and is made again.
func transportError(err error) bool {
	var rpc *jsonrpc.Error
	return !errors.As(err, &rpc) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// transport dials only public addresses unless the server allows private ones,
// and ignores proxy variables, which would dial past that check.
func transport(srv *registry.MCPServer) http.RoundTripper {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	dial := d.DialContext
	if !srv.AllowPrivate {
		dial = safefetch.GuardedDial(d)
	}
	return &http.Transport{
		DialContext:         dial,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
}

// credentialTransport adds the server's credential to every request and caps
// what a reply body may hold. The standalone stream (GET) is never opened.
type credentialTransport struct {
	srv  *registry.MCPServer
	base http.RoundTripper
}

func (t *credentialTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.srv.Header != "" {
		r = r.Clone(r.Context())
		r.Header.Set(t.srv.Header, t.srv.Value)
	}
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	// An SSE transport's event stream is long-lived; its events are capped by MaxEventSize.
	if r.Method == http.MethodPost {
		cut, _ := r.Context().Value(cutKey{}).(*atomic.Bool)
		resp.Body = &limitedBody{rc: resp.Body, left: t.srv.MaxResponse() + envelope, cut: cut}
	}
	return resp, nil
}

type limitedBody struct {
	rc   io.ReadCloser
	left int64
	cut  *atomic.Bool
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		if b.cut != nil {
			b.cut.Store(true)
		}
		return 0, errTooLarge
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }

// pool keeps one upstream per server and replaces it when the server's spec
// or credential changes.
type pool struct {
	mu sync.Mutex
	m  map[string]*upstream
}

func (p *pool) get(srv *registry.MCPServer) *upstream {
	fp := srv.Fingerprint()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]*upstream{}
	}
	u := p.m[srv.Name]
	if u != nil && u.fp == fp {
		return u
	}
	if u != nil {
		go u.retire()
	}
	u = &upstream{srv: srv, fp: fp}
	p.m[srv.Name] = u
	return u
}

// prune closes the sessions of servers no longer registered or enabled.
func (p *pool) prune(servers map[string]*registry.MCPServer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for name, u := range p.m {
		if servers[name] == nil {
			delete(p.m, name)
			go u.retire()
		}
	}
}

func (p *pool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for name, u := range p.m {
		delete(p.m, name)
		u.retire()
	}
}
