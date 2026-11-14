package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"
)

var delegationMCPTools = []any{map[string]any{
	"name": "hcg_delegate_read", "description": "Read explicitly approved relative files through fixed HCG/PAL policy, budget and evidence gates; unavailable falls back to native read/rg.",
	"inputSchema": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"question", "paths"}, "properties": map[string]any{"question": map[string]any{"type": "string", "minLength": 1, "maxLength": 16384}, "paths": map[string]any{"type": "array", "minItems": 1, "maxItems": 16, "items": map[string]any{"type": "string"}}, "focus": map[string]any{"type": "string", "maxLength": 4096}, "output_limit": map[string]any{"type": "integer", "minimum": 0, "maximum": 65536}}},
	"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "openWorldHint": true},
}}

// Only this fixed HCG server runs concurrent calls so cancellation notifications
// can reach a pending HTTP request. It does not alter AO's existing MCP tools.
func (c *commandContext) serveDelegationMCP(ctx context.Context, in io.Reader, out io.Writer) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 256<<10)
	encoder := json.NewEncoder(out)
	var outputMu, mu sync.Mutex
	active := map[string]context.CancelFunc{}
	var wg sync.WaitGroup
	send := func(resp *mcpResponse) { outputMu.Lock(); defer outputMu.Unlock(); _ = encoder.Encode(resp) }
	defer func() {
		mu.Lock()
		for _, cancel := range active {
			cancel()
		}
		mu.Unlock()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}()
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		var req mcpRequest
		if json.Unmarshal(line, &req) != nil {
			send(&mcpResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &mcpError{Code: -32700, Message: "invalid HCG JSON-RPC"}})
			continue
		}
		if req.Method == "notifications/cancelled" {
			var cancel struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(req.Params, &cancel) == nil {
				mu.Lock()
				if fn := active[string(cancel.RequestID)]; fn != nil {
					fn()
				}
				mu.Unlock()
			}
			continue
		}
		if req.ID == nil {
			continue
		}
		resp := &mcpResponse{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			var params struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &params)
			version := mcpDefaultProtocolVersion
			if slices.Contains(mcpProtocolVersions, params.ProtocolVersion) {
				version = params.ProtocolVersion
			}
			resp.Result = map[string]any{"protocolVersion": version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "hcg", "version": Version}}
			send(resp)
		case "ping":
			resp.Result = map[string]any{}
			send(resp)
		case "tools/list":
			resp.Result = map[string]any{"tools": delegationMCPTools}
			send(resp)
		case "tools/call":
			var call struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
				Meta      json.RawMessage `json:"_meta"`
			}
			d := json.NewDecoder(bytes.NewReader(req.Params))
			d.DisallowUnknownFields()
			if d.Decode(&call) != nil || d.Decode(new(any)) != io.EOF || call.Name != "hcg_delegate_read" || !validDelegationMCPMetadata(call.Meta) {
				resp.Error = &mcpError{Code: -32602, Message: "invalid HCG tool request"}
				send(resp)
				continue
			}
			var args delegationReadRequest
			d = json.NewDecoder(bytes.NewReader(call.Arguments))
			d.DisallowUnknownFields()
			if d.Decode(&args) != nil || d.Decode(new(any)) != io.EOF || args.Question == "" || len(args.Paths) == 0 {
				resp.Error = &mcpError{Code: -32602, Message: "invalid HCG tool arguments"}
				send(resp)
				continue
			}
			id, err := renderSessionID()
			if err != nil {
				resp.Error = &mcpError{Code: -32602, Message: "AO session context unavailable"}
				send(resp)
				continue
			}
			key := string(req.ID)
			mu.Lock()
			if active[key] != nil {
				mu.Unlock()
				resp.Error = &mcpError{Code: -32600, Message: "duplicate active request ID"}
				send(resp)
				continue
			}
			callCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
			active[key] = cancel
			mu.Unlock()
			wg.Add(1)
			go func(resp *mcpResponse, args delegationReadRequest, id, key string, callCtx context.Context, cancel context.CancelFunc) {
				defer wg.Done()
				defer cancel()
				defer func() { mu.Lock(); delete(active, key); mu.Unlock() }()
				var reply json.RawMessage
				if err := c.doJSON(callCtx, http.MethodPost, "sessions/"+url.PathEscape(id)+"/hcg-delegation/read", args, &reply); err != nil {
					reply = delegationFailureJSON(callCtx, err)
				}
				var status usageReportResponse
				_ = json.Unmarshal(reply, &status)
				resp.Result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(reply)}}, "isError": status.Status != "ok" && status.Status != "not_found"}
				send(resp)
			}(resp, args, id, key, callCtx, cancel)
		default:
			resp.Error = &mcpError{Code: -32601, Message: "unknown HCG method"}
			send(resp)
		}
	}
	return scanner.Err()
}

// Metadata is validated at the wire boundary and never projected into HCG args.
func validDelegationMCPMetadata(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	var meta map[string]json.RawMessage
	if json.Unmarshal(raw, &meta) != nil || meta == nil {
		return false
	}
	token, present := meta["progressToken"]
	if !present {
		return true
	}
	d := json.NewDecoder(bytes.NewReader(token))
	d.UseNumber()
	var value any
	if d.Decode(&value) != nil {
		return false
	}
	switch value.(type) {
	case string, json.Number:
		return true
	default:
		return false
	}
}
