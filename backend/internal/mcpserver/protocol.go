package mcpserver

import "encoding/json"

// ProtocolVersion negotiates the shared wire versions of AO's stdio MCP servers.
func ProtocolVersion(requested string) string {
	switch requested {
	case "2025-03-26", "2024-11-05":
		return requested
	default:
		return "2025-06-18"
	}
}

type Request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}
