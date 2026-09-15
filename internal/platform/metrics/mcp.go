package metrics

// The key and request trace are audit fields, never metric labels. Tool names
// are normalized to the finite registered catalog by the MCP gateway.
var (
	MCPLatency = histogram("mcp_request_duration_seconds", "MCP call latency by tool.", "tool")
	MCPBytes   = counter("mcp_payload_bytes_total", "Serialized MCP input/output bytes, not payload contents.", "tool", "direction")
)
