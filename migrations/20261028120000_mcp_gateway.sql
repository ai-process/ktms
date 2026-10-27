-- Upstream MCP servers proxied at /mcp, and the header the gateway adds to
-- every request it sends one. Keys and teams name the servers and tools they
-- may use in allowed_mcp ("server" or "server/tool"); empty means any.
CREATE TABLE IF NOT EXISTS mcp_server (
    name TEXT PRIMARY KEY,
    url TEXT NOT NULL,
    transport TEXT NOT NULL DEFAULT 'streamable_http',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    allow_private BOOLEAN NOT NULL DEFAULT FALSE,
    timeout_ms INT NOT NULL DEFAULT 0,
    max_response_bytes BIGINT NOT NULL DEFAULT 0,
    tool_rpm INT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS mcp_credential (
    server TEXT PRIMARY KEY REFERENCES mcp_server(name) ON DELETE CASCADE,
    header TEXT NOT NULL,
    value_ciphertext BYTEA NOT NULL, -- AES-256-GCM, as vendor_key
    value_version INT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE api_key ADD COLUMN IF NOT EXISTS allowed_mcp TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE team ADD COLUMN IF NOT EXISTS allowed_mcp TEXT[] NOT NULL DEFAULT '{}';
