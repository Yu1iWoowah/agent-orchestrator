package persistenthost

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/processscope"
)

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
	return response.CompactionScope, finish(nil)
}

func (h *host) compactionScope() ports.ChatCompactionScope {
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
	key, fallback := "CODEX_HOME", ".codex"
	if h.acp != nil {
		key, fallback = "CLAUDE_CONFIG_DIR", ".claude"
	}
	home := env[key]
	if home == "" {
		home = filepath.Join(env["HOME"], fallback)
	}
	if !filepath.IsAbs(home) {
		home = filepath.Join(h.child.Dir, home)
	}
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil {
		resolved = ""
	}
	return ports.ChatCompactionScope{NativeHome: resolved, Processes: processscope.Capture(h.child.Process.Pid)}
}
