package grpcapi

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/crypto"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/safefetch"
)

// MCPToolLister asks a registered MCP server for its tools (mcpgw.Gateway).
type MCPToolLister interface {
	ListTools(ctx context.Context, server string) ([]*mcp.Tool, error)
}

// SetMCPToolLister lets ListMCPTools reach the servers; without it the RPC is
// UNAVAILABLE (the gateway is off).
func (s *AdminServer) SetMCPToolLister(l MCPToolLister) { s.mcpTools = l }

// checkHost resolves an MCP server's host; tests replace it.
var checkHost = safefetch.CheckHost

func (s *AdminServer) UpsertMCPServer(ctx context.Context, req *pb.UpsertMCPServerRequest) (*pb.UpsertMCPServerResponse, error) {
	m := mcpServerFromProto(req.GetServer())
	if err := registry.ValidateMCPServer(m); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if !m.AllowPrivate {
		u, _ := url.Parse(m.URL) // ValidateMCPServer parsed it
		if err := checkHost(ctx, u.Hostname()); err != nil {
			if errors.Is(err, safefetch.ErrProhibitedIP) {
				return nil, status.Errorf(codes.InvalidArgument, "mcp server %s: %v; set allow_private for a server on a private network", m.Name, err)
			}
			return nil, status.Errorf(codes.InvalidArgument, "mcp server %s: %v", m.Name, err)
		}
	}
	_, err := s.db.Mutate(ctx, func(tx pgx.Tx) error {
		return proxydb.UpsertMCPServer(ctx, tx, m)
	}, nil)
	if err != nil {
		return nil, mutationStatus(err)
	}
	s.onChange()
	return &pb.UpsertMCPServerResponse{}, nil
}

func (s *AdminServer) DeleteMCPServer(ctx context.Context, req *pb.DeleteMCPServerRequest) (*pb.DeleteMCPServerResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	_, err := s.db.Mutate(ctx, func(tx pgx.Tx) error {
		return proxydb.DeleteMCPServer(ctx, tx, req.GetName())
	}, nil)
	if err != nil {
		return nil, mutationStatus(err)
	}
	s.onChange()
	return &pb.DeleteMCPServerResponse{}, nil
}

func (s *AdminServer) SetMCPCredential(ctx context.Context, req *pb.SetMCPCredentialRequest) (*pb.SetMCPCredentialResponse, error) {
	header := strings.TrimSpace(req.GetHeader())
	if header == "" {
		header = "Authorization"
	}
	if !validHeaderName(header) {
		return nil, status.Errorf(codes.InvalidArgument, "header %q is not a valid HTTP header name", header)
	}
	if req.GetPlainValue() == "" || strings.ContainsAny(req.GetPlainValue(), "\r\n") {
		return nil, status.Error(codes.InvalidArgument, "plain_value is required and must be one line")
	}
	ciphertext, version, err := s.keyring.Encrypt(req.GetPlainValue())
	if err != nil {
		return nil, status.Error(codes.Internal, "encryption failed")
	}
	_, err = s.db.Mutate(ctx, func(tx pgx.Tx) error {
		return proxydb.SetMCPCredential(ctx, tx, &proxydb.MCPCredential{
			Server: req.GetServer(), Header: header, ValueCiphertext: ciphertext, ValueVersion: int32(version),
		})
	}, nil)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return nil, status.Errorf(codes.NotFound, "no mcp server %q", req.GetServer())
	}
	if err != nil {
		return nil, mutationStatus(err)
	}
	s.onChange()
	return &pb.SetMCPCredentialResponse{}, nil
}

func (s *AdminServer) DeleteMCPCredential(ctx context.Context, req *pb.DeleteMCPCredentialRequest) (*pb.DeleteMCPCredentialResponse, error) {
	_, err := s.db.Mutate(ctx, func(tx pgx.Tx) error {
		return proxydb.DeleteMCPCredential(ctx, tx, req.GetServer())
	}, nil)
	if err != nil {
		return nil, mutationStatus(err)
	}
	s.onChange()
	return &pb.DeleteMCPCredentialResponse{}, nil
}

func (s *AdminServer) ListMCPTools(ctx context.Context, req *pb.ListMCPToolsRequest) (*pb.ListMCPToolsResponse, error) {
	if s.mcpTools == nil {
		return nil, status.Error(codes.Unavailable, "the MCP gateway is off (MCP_GATEWAY_ENABLED=false)")
	}
	tools, err := s.mcpTools.ListTools(ctx, req.GetServer())
	if err != nil {
		// The error names the server and its trouble, never its credential.
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	resp := &pb.ListMCPToolsResponse{}
	for _, t := range tools {
		resp.Tools = append(resp.Tools, &pb.MCPToolInfo{Name: t.Name, Description: t.Description})
	}
	return resp, nil
}

// rotateMCPCredentials re-encrypts the MCP credentials not at the active
// keyring version, inside the vendor keys' rotation transaction.
func (s *AdminServer) rotateMCPCredentials(ctx context.Context, tx pgx.Tx, active int32) (int32, error) {
	creds, err := proxydb.LockStaleMCPCredentialsTx(ctx, tx, active)
	if err != nil {
		return 0, err
	}
	var n int32
	for _, c := range creds {
		plain, err := s.keyring.Decrypt(c.ValueCiphertext, int(c.ValueVersion))
		if err != nil {
			return n, &unreadableKeyError{id: "mcp:" + c.Server, version: c.ValueVersion}
		}
		ciphertext, version, err := s.keyring.Encrypt(plain)
		if err != nil {
			return n, err
		}
		if err := proxydb.UpdateMCPCredentialCiphertext(ctx, tx, c.Server, ciphertext, int32(version)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *AdminServer) mcpCredentialInfo(c *proxydb.MCPCredential) *pb.MCPCredentialInfo {
	info := &pb.MCPCredentialInfo{Server: c.Server, Header: c.Header, KeyVersion: c.ValueVersion, CreatedAt: timestamppb.New(c.CreatedAt)}
	if s.keyring != nil {
		if plain, err := s.keyring.Decrypt(c.ValueCiphertext, int(c.ValueVersion)); err == nil {
			info.Fingerprint = crypto.Fingerprint(plain)
		} else {
			log.Warn().Err(err).Str("mcp_server", c.Server).Msg("mcp credential unreadable for its fingerprint")
		}
	}
	return info
}

func mcpServerFromProto(p *pb.MCPServerSpec) *proxydb.MCPServer {
	t := p.GetTransport()
	if t == "" {
		t = registry.MCPStreamableHTTP
	}
	return &proxydb.MCPServer{
		Name:             p.GetName(),
		URL:              p.GetUrl(),
		Transport:        t,
		Enabled:          p.GetEnabled(),
		AllowPrivate:     p.GetAllowPrivate(),
		TimeoutMs:        p.GetTimeoutMs(),
		MaxResponseBytes: p.GetMaxResponseBytes(),
		ToolRPM:          p.GetToolRpm(),
	}
}

func mcpServerToProto(m *proxydb.MCPServer) *pb.MCPServerSpec {
	return &pb.MCPServerSpec{
		Name:             m.Name,
		Url:              m.URL,
		Transport:        m.Transport,
		Enabled:          m.Enabled,
		AllowPrivate:     m.AllowPrivate,
		TimeoutMs:        m.TimeoutMs,
		MaxResponseBytes: m.MaxResponseBytes,
		ToolRpm:          m.ToolRPM,
	}
}

// normalizeAllowedMCP checks entries "server" and "server/tool", dropping
// duplicates. Servers are not checked against the registry, like models.
func normalizeAllowedMCP(in []string) ([]string, error) {
	out := []string{}
	for _, e := range in {
		e = strings.TrimSpace(e)
		server, tool, hasTool := strings.Cut(e, "/")
		if registry.ValidateMCPServer(&proxydb.MCPServer{Name: server, URL: "https://x", Transport: registry.MCPStreamableHTTP}) != nil || (hasTool && tool == "") {
			return nil, status.Errorf(codes.InvalidArgument, "allowed_mcp entry %q: want server or server/tool", e)
		}
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out, nil
}

func validHeaderName(h string) bool {
	for _, r := range h {
		if r > 127 || !(r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return h != "" && !strings.EqualFold(h, "host") && !strings.EqualFold(h, "content-length")
}
