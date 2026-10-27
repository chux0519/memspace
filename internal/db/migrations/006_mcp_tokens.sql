-- M3: read-only MCP token per device (PRD §12: per-device credentials,
-- revocable). It authorizes MCP read tools only, never event writes, so it
-- can be handed to agent configuration with less risk than the device token.
ALTER TABLE devices ADD COLUMN mcp_token_hash TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_devices_mcp_token_hash ON devices (mcp_token_hash);
