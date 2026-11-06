package registry

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"github.com/ai-process/ktms/internal/crypto"
	"github.com/ai-process/ktms/internal/proxydb"
)

// MCP transports.
const (
	MCPStreamableHTTP = "streamable_http"
	MCPSSE            = "sse"
)

// MCP server defaults for the 0 values of the spec.
const (
	DefaultMCPTimeout          = 60 * time.Second
	DefaultMCPMaxResponseBytes = 4 << 20
)

// mcpName keeps server names out of the "__" and "/" that separate them from
// tool names, and short enough for usage events and client tool name limits.
var mcpName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// MCPServer is an enabled MCP server with its decrypted credential.
type MCPServer struct {
	proxydb.MCPServer
	Header string // "" when the server has no credential
	Value  string
}

// Timeout bounds one tool call.
func (m *MCPServer) Timeout() time.Duration {
	if m.TimeoutMs > 0 {
		return time.Duration(m.TimeoutMs) * time.Millisecond
	}
	return DefaultMCPTimeout
}

// MaxResponse bounds one tool result.
func (m *MCPServer) MaxResponse() int64 {
	if m.MaxResponseBytes > 0 {
		return m.MaxResponseBytes
	}
	return DefaultMCPMaxResponseBytes
}

// Fingerprint changes whenever anything a connection depends on does, so the
// gateway knows to drop a connection made with an older spec or credential.
func (m *MCPServer) Fingerprint() string {
	return crypto.Fingerprint(fmt.Sprintf("%s\x00%s\x00%t\x00%d\x00%d\x00%s\x00%s",
		m.URL, m.Transport, m.AllowPrivate, m.TimeoutMs, m.MaxResponseBytes, m.Header, m.Value))
}

// ValidMCPName reports whether name may name an MCP server.
func ValidMCPName(name string) bool { return mcpName.MatchString(name) }

// ValidateMCPServer checks a spec on its own; whether its host resolves to a
// private address is checked where the network is (the Admin API).
func ValidateMCPServer(m *proxydb.MCPServer) error {
	if !mcpName.MatchString(m.Name) {
		return fmt.Errorf("mcp server name %q: 1-32 of a-z, 0-9 and -, starting with a letter", m.Name)
	}
	switch m.Transport {
	case MCPStreamableHTTP, MCPSSE:
	default:
		return fmt.Errorf("mcp server %s: transport must be %s or %s, got %q", m.Name, MCPStreamableHTTP, MCPSSE, m.Transport)
	}
	u, err := url.Parse(m.URL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("mcp server %s: url %q is not an absolute URL", m.Name, m.URL)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && m.AllowPrivate:
	case u.Scheme == "http":
		return fmt.Errorf("mcp server %s: http is allowed only with allow_private (a server on a private network); use https", m.Name)
	default:
		return fmt.Errorf("mcp server %s: url scheme must be https", m.Name)
	}
	if u.User != nil {
		return errors.New("mcp server " + m.Name + ": keep credentials out of the url, set them with SetMCPCredential")
	}
	if m.TimeoutMs < 0 || m.MaxResponseBytes < 0 || m.ToolRPM < 0 {
		return fmt.Errorf("mcp server %s: timeout_ms, max_response_bytes and tool_rpm must be >= 0", m.Name)
	}
	return nil
}
