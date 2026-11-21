package codex

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestHCGDelegationMountLaunchAndRestore(t *testing.T) {
	p := &Plugin{resolvedBinary: "codex"}
	env := map[string]string{ports.EnvHCGDelegationMCP: "1", "AO_SESSION_ID": "synthetic-session", "AO_RUN_FILE": "/synthetic/run.json", "AO_DATA_DIR": "/synthetic/data", "OPENAI_API_KEY": "MUST_NOT_BE_FORWARDED", "AO_BROWSER_CAPABILITY": "MUST_NOT_BE_FORWARDED"}
	check := func(cmd, baseline []string) {
		t.Helper()
		var stripped []string
		count := 0
		for i := 0; i < len(cmd); i++ {
			if cmd[i] == "-c" && i+1 < len(cmd) && strings.HasPrefix(cmd[i+1], "mcp_servers.hcg.") {
				count++
				i++
				continue
			}
			stripped = append(stripped, cmd[i])
		}
		if count != 5 || !reflect.DeepEqual(stripped, baseline) {
			t.Fatalf("mount changed native argv: count=%d\n%q\n%q", count, stripped, baseline)
		}
		joined := strings.Join(cmd, " ")
		if !strings.Contains(joined, `mcp_servers.hcg.args=["usage","delegation","mcp"]`) || strings.Contains(joined, "MUST_NOT_BE_FORWARDED") || strings.Contains(joined, "read_only") {
			t.Fatalf("unsafe MCP argv: %q", cmd)
		}
	}
	launch := ports.LaunchConfig{Env: env, Permissions: ports.PermissionModeAuto}
	mounted, err := p.GetLaunchCommand(context.Background(), launch)
	if err != nil {
		t.Fatal(err)
	}
	launch.Env = nil
	baseline, err := p.GetLaunchCommand(context.Background(), launch)
	if err != nil {
		t.Fatal(err)
	}
	check(mounted, baseline)
	restore := ports.RestoreConfig{Env: env, Permissions: ports.PermissionModeAuto, Session: ports.SessionRef{ID: "s", Metadata: map[string]string{ports.MetadataKeyAgentSessionID: "synthetic-native"}}}
	mounted, ok, err := p.GetRestoreCommand(context.Background(), restore)
	if err != nil || !ok {
		t.Fatalf("restore: %v %v", ok, err)
	}
	restore.Env = nil
	baseline, ok, err = p.GetRestoreCommand(context.Background(), restore)
	if err != nil || !ok {
		t.Fatalf("baseline: %v %v", ok, err)
	}
	check(mounted, baseline)
}
