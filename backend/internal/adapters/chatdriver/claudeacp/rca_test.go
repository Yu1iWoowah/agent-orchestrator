package claudeacp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestClaudeRCAEnvironmentChild(t *testing.T) {
	if os.Getenv("AO_RCA_FAKE_CHILD") != "1" {
		return
	}
	checks := []bool{os.Getenv("OTEL_LOGS_EXPORTER") == "otlp", os.Getenv("OTEL_TRACES_EXPORTER") == "none", os.Getenv("OTEL_METRICS_EXPORTER") == "none", os.Getenv("OTEL_LOG_RAW_API_BODIES") == "1", strings.HasPrefix(os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"), "http://127.0.0.1:"), strings.HasPrefix(os.Getenv("OTEL_EXPORTER_OTLP_LOGS_HEADERS"), "X-AO-RCA-Capture=")}
	// Synthetic helper prints booleans only, never env/header/credential values.
	if err := json.NewEncoder(os.Stdout).Encode(checks); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
func TestClaudeRCAEnvironmentReachesFakeChildWithoutChangingAccountKeys(t *testing.T) {
	native := map[string]string{"SYNTHETIC_NATIVE_KEY": "A_SYNTHETIC_CREDENTIAL", "PROJECT_ENV": "kept"}
	env := claudeACPLaunchEnv(native, "/synthetic/claude", "", nil)
	called := false
	prepare := func(_ context.Context, actual ports.RCAClaudeRuntime, keys []string) map[string]string {
		called = true
		if actual.BinaryPath != "/synthetic/claude" || actual.BinaryVersion != "" {
			t.Fatal("runtime evidence invented")
		}
		found := false
		for _, key := range keys {
			if key == "SYNTHETIC_NATIVE_KEY" {
				found = true
			}
			if strings.Contains(key, "A_SYNTHETIC") {
				t.Fatal("callback received values")
			}
		}
		if !found {
			t.Fatal("key presence omitted")
		}
		return map[string]string{"OTEL_LOGS_EXPORTER": "otlp", "OTEL_METRICS_EXPORTER": "none", "OTEL_TRACES_EXPORTER": "none", "OTEL_LOG_RAW_API_BODIES": "1", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT": "http://127.0.0.1:12345/api/v1/rca/otlp/v1/logs", "OTEL_EXPORTER_OTLP_LOGS_HEADERS": "X-AO-RCA-Capture=synthetic-marker"}
	}
	applyClaudeRCAEnvironment(t.Context(), prepare, env, ports.RCAClaudeRuntime{BinaryPath: "/synthetic/claude"})
	if !called || env["SYNTHETIC_NATIVE_KEY"] != native["SYNTHETIC_NATIVE_KEY"] || env["PROJECT_ENV"] != "kept" {
		t.Fatal("RCA changed native account/project env")
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestClaudeRCAEnvironmentChild$")
	cmd.Env = append(os.Environ(), "AO_RCA_FAKE_CHILD=1")
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal("synthetic helper failed")
	}
	var checks []bool
	if json.Unmarshal(out, &checks) != nil || len(checks) != 6 {
		t.Fatal("invalid numeric helper evidence")
	}
	for _, ok := range checks {
		if !ok {
			t.Fatal("RCA launch key did not reach fake child")
		}
	}
}
func TestClaudeRCAEnvironmentPanicAndConflictLeaveNativeUntouched(t *testing.T) {
	for _, scenario := range []string{"unknown", "panic", "existing", "unknown-key"} {
		t.Run(scenario, func(t *testing.T) {
			env := map[string]string{"SYNTHETIC_NATIVE_KEY": "A_SYNTHETIC_CREDENTIAL"}
			if scenario == "existing" {
				env["OTEL_LOGS_EXPORTER"] = "console"
			}
			prepare := func(context.Context, ports.RCAClaudeRuntime, []string) map[string]string {
				switch scenario {
				case "unknown":
					return nil
				case "panic":
					panic("A_SYNTHETIC_CREDENTIAL B_SYNTHETIC_CONTENT")
				case "existing":
					return map[string]string{"OTEL_LOGS_EXPORTER": "otlp"}
				default:
					return map[string]string{"OTEL_LOGS_EXPORTER": "otlp", "UNAPPROVED_KEY": "bad"}
				}
			}
			applyClaudeRCAEnvironment(t.Context(), prepare, env, ports.RCAClaudeRuntime{})
			if env["SYNTHETIC_NATIVE_KEY"] != "A_SYNTHETIC_CREDENTIAL" || env["OTEL_LOGS_EXPORTER"] == "otlp" || env["UNAPPROVED_KEY"] != "" {
				t.Fatal("failed optional RCA preparation changed native launch")
			}
		})
	}
}
