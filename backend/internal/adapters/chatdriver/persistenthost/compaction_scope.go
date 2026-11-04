package persistenthost

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/nativehome"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/processscope"
)

// CompactionScope reads original launch facts; unsupported hosts return a distinct error.
func (c *DeliveryClient) CompactionScope(ctx context.Context) (ports.ChatCompactionScope, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.descriptor.Address)
	if err != nil {
		return ports.ChatCompactionScope{}, err
	}
	defer func() { _ = conn.Close() }()
	finish := bindConnToContext(ctx, conn)
	if err := json.NewEncoder(conn).Encode(hello{Version: ProtocolVersion, Token: c.descriptor.Token, Action: "compaction-scope"}); err != nil {
		return ports.ChatCompactionScope{}, finish(err)
	}
	var response helloResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		return ports.ChatCompactionScope{}, finish(err)
	}
	if response.Error != "" {
		if response.Error == "unknown chat host action" {
			return ports.ChatCompactionScope{}, finish(ports.ErrChatCompactionScopeUnsupported)
		}
		return ports.ChatCompactionScope{}, finish(fmt.Errorf("chat host compaction scope: %s", response.Error))
	}
	return response.CompactionScope, finish(nil)
}

func (h *host) compactionScope(dataDir string) ports.ChatCompactionScope {
	if h.child == nil || h.child.Process == nil {
		return ports.ChatCompactionScope{}
	}
	env := map[string]string{}
	for _, entry := range h.child.Env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}
	provider := domain.AgentHarness(env["AO_CHAT_HARNESS"])
	if provider == "" && h.acp == nil {
		provider = domain.HarnessCodex
	}
	resolved, err := nativehome.Resolve(h.ctx, provider, env, h.child.Dir, dataDir)
	scope := ports.ChatCompactionScope{NativeHome: resolved, Processes: processscope.Capture(h.child.Process.Pid)}
	if err != nil {
		scope.NativeHomeError = err.Error()
	}
	return scope
}

func (h *host) currentCompactionScope() ports.ChatCompactionScope {
	h.compactionMu.Lock()
	defer h.compactionMu.Unlock()
	if h.compaction.NativeHome == "" {
		retry := h.compactionScope(h.compactionDataDir)
		// Only the previously unavailable root is retried; process identity stays frozen.
		retry.Processes = h.compaction.Processes
		h.compaction = retry
	}
	return h.compaction
}
