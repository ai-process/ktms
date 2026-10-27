package proxydb

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// MCPServer is an upstream MCP server the gateway proxies.
type MCPServer struct {
	Name             string
	URL              string
	Transport        string // "streamable_http" | "sse"
	Enabled          bool
	AllowPrivate     bool
	TimeoutMs        int32
	MaxResponseBytes int64
	ToolRPM          int32
}

// MCPCredential is the header the gateway adds to a server's requests.
type MCPCredential struct {
	Server          string
	Header          string
	ValueCiphertext []byte
	ValueVersion    int32
	CreatedAt       time.Time
}

func (d *DB) LoadMCPServers(ctx context.Context) ([]*MCPServer, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT name, url, transport, enabled, allow_private, timeout_ms, max_response_bytes, tool_rpm
		 FROM mcp_server ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MCPServer
	for rows.Next() {
		var m MCPServer
		if err := rows.Scan(&m.Name, &m.URL, &m.Transport, &m.Enabled, &m.AllowPrivate,
			&m.TimeoutMs, &m.MaxResponseBytes, &m.ToolRPM); err != nil {
			return nil, err
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

func (d *DB) LoadMCPCredentials(ctx context.Context) ([]*MCPCredential, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT server, header, value_ciphertext, value_version, created_at FROM mcp_credential ORDER BY server`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MCPCredential
	for rows.Next() {
		var c MCPCredential
		if err := rows.Scan(&c.Server, &c.Header, &c.ValueCiphertext, &c.ValueVersion, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

func UpsertMCPServer(ctx context.Context, tx pgx.Tx, m *MCPServer) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO mcp_server (name, url, transport, enabled, allow_private, timeout_ms, max_response_bytes, tool_rpm)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		 ON CONFLICT (name) DO UPDATE SET url = $2, transport = $3, enabled = $4, allow_private = $5,
		   timeout_ms = $6, max_response_bytes = $7, tool_rpm = $8, updated_at = NOW()`,
		m.Name, m.URL, m.Transport, m.Enabled, m.AllowPrivate, m.TimeoutMs, m.MaxResponseBytes, m.ToolRPM)
	return err
}

// DeleteMCPServer removes the server; its credential goes with it (cascade).
func DeleteMCPServer(ctx context.Context, tx pgx.Tx, name string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM mcp_server WHERE name = $1`, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetMCPCredential stores a server's credential; an unknown server is a
// foreign key violation (23503).
func SetMCPCredential(ctx context.Context, tx pgx.Tx, c *MCPCredential) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO mcp_credential (server, header, value_ciphertext, value_version)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (server) DO UPDATE SET header = $2, value_ciphertext = $3, value_version = $4, created_at = NOW()`,
		c.Server, c.Header, c.ValueCiphertext, c.ValueVersion)
	return err
}

func DeleteMCPCredential(ctx context.Context, tx pgx.Tx, server string) error {
	tag, err := tx.Exec(ctx, `DELETE FROM mcp_credential WHERE server = $1`, server)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// LockStaleMCPCredentialsTx is LockStaleVendorKeysTx for MCP credentials.
func LockStaleMCPCredentialsTx(ctx context.Context, tx pgx.Tx, version int32) ([]*MCPCredential, error) {
	rows, err := tx.Query(ctx,
		`SELECT server, header, value_ciphertext, value_version FROM mcp_credential
		 WHERE value_version <> $1 ORDER BY server FOR UPDATE`, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MCPCredential
	for rows.Next() {
		var c MCPCredential
		if err := rows.Scan(&c.Server, &c.Header, &c.ValueCiphertext, &c.ValueVersion); err != nil {
			return nil, err
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// UpdateMCPCredentialCiphertext is the rotation path: same value, newer keyring version.
func UpdateMCPCredentialCiphertext(ctx context.Context, tx pgx.Tx, server string, ciphertext []byte, version int32) error {
	_, err := tx.Exec(ctx,
		`UPDATE mcp_credential SET value_ciphertext = $2, value_version = $3 WHERE server = $1`,
		server, ciphertext, version)
	return err
}
