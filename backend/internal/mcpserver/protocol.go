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

// Request is a newline-delimited MCP JSON-RPC request.
type Request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// RPCError carries a JSON-RPC failure code and message.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Response is the MCP JSON-RPC reply to a request with an ID.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}
