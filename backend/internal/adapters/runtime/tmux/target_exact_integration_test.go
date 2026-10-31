//go:build !windows

package tmux

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestExactSessionTargetsDoNotReachPrefixDecoy(t *testing.T) {
	binary, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux unavailable")
	}
	socketDir, err := os.MkdirTemp("", "t-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	t.Setenv("TMUX_TMPDIR", socketDir)
	t.Setenv("TMUX", "")
	const socket = "exact-test"
	const target, decoy = "review-x-1", "review-x-10"
	raw := func(args ...string) ([]byte, error) {
		return exec.Command(binary, append([]string{"-L", socket}, args...)...).CombinedOutput()
	}
	run := func(t *testing.T, args ...string) string {
		t.Helper()
		out, err := raw(args...)
		if err != nil {
			t.Fatalf("tmux %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(t, "-f", os.DevNull, "new-session", "-d", "-s", "anchor", "/bin/cat")
	t.Cleanup(func() { _, _ = raw("kill-server") })
	r := New(Options{Binary: binary, SocketName: socket, Shell: "/bin/sh"})
	workspace := t.TempDir()
	for _, tc := range []struct {
		name string
		args func(string) []string
		want string
	}{
		{"respawn", func(id string) []string { return respawnPaneArgs(id, workspace, "/bin/sh", "exec /bin/cat") }, ""},
		{"status", setStatusOffArgs, ""},
		{"mouse", setMouseOnArgs, ""},
		{"window_size", setWindowSizeLargestArgs, ""},
		{"detach", setDetachOnDestroyOnArgs, ""},
		{"show_options", showDetachOnDestroyArgs, "off"},
		{"pane_pid", panePIDArgs, "PID"},
		{"pane_dead", paneDeadArgs, "0"},
		{"pane_cwd", paneCurrentPathArgs, workspace},
		{"pane_pids", listPanePIDsArgs, "PID"},
		{"literal_input", func(id string) []string { return sendKeysLiteralArgs(id, "wrong-reviewer-input") }, ""},
		{"enter", sendEnterArgs, ""},
		{"interrupt", sendInterruptArgs, ""},
		{"capture", func(id string) []string { return capturePaneArgs(id, 50) }, "TARGET_READY"},
		{"styled_capture", func(id string) []string { return capturePaneStyledArgs(id, 50) }, "TARGET_READY"},
		{"has_session", hasSessionArgs, ""},
		{"kill_session", killSessionArgs, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, id := range []string{target, decoy} {
				marker := "TARGET_READY"
				if id == decoy {
					marker = "DECOY_READY"
				}
				run(t, "new-session", "-d", "-s", id, "-c", workspace, "/bin/sh", "-c", "printf '"+marker+"\\n'; exec /bin/cat")
				t.Cleanup(func() { _, _ = raw("kill-session", "-t", "="+id) })
				run(t, "set-option", "-t", "="+id+":", "detach-on-destroy", "off")
				deadline := time.Now().Add(5 * time.Second)
				for !strings.Contains(run(t, "capture-pane", "-p", "-t", "="+id+":"), marker) {
					if time.Now().After(deadline) {
						t.Fatalf("session %s did not become ready", id)
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			decoyState := func() string {
				return run(t, "capture-pane", "-p", "-t", "="+decoy+":") +
					run(t, "display-message", "-p", "-t", "="+decoy+":", "#{pane_id} #{pane_pid}") +
					run(t, "show-options", "-t", "="+decoy+":") +
					run(t, "show-options", "-w", "-t", "="+decoy+":")
			}
			before := decoyState()
			want := tc.want
			if want == "PID" {
				want = run(t, "display-message", "-p", "-t", "="+target+":", "#{pane_pid}")
			}
			out, err := r.run(context.Background(), tc.args(target)...)
			if err != nil || !strings.Contains(string(out), want) {
				t.Fatalf("live target: output %q, error %v; want %q", out, err, want)
			}
			if got := decoyState(); got != before {
				t.Fatal("operation on live target changed decoy")
			}
			_, _ = raw("kill-session", "-t", "="+target)
			out, err = r.run(context.Background(), tc.args(target)...)
			// display-message may return success with no output for a missing target.
			absent := err != nil && sessionMissingOutput(string(out))
			if tc.name == "window_size" {
				absent = absent || err != nil && strings.Contains(string(out), "no such window: ="+target+":")
			}
			if tc.name == "pane_pid" || tc.name == "pane_cwd" {
				absent = absent || err == nil && strings.TrimSpace(string(out)) == ""
			}
			if !absent {
				t.Errorf("missing target reached decoy or failed unexpectedly: output %q, error %v", out, err)
			}
			if tc.name == "pane_pids" {
				pids, err := r.ProcessRootPIDs(context.Background(), ports.RuntimeHandle{ID: target})
				if err != nil || len(pids) != 0 {
					t.Errorf("missing process roots = %v, %v; want none", pids, err)
				}
				pids, err = r.ProcessRootPIDs(context.Background(), ports.RuntimeHandle{ID: decoy})
				if err != nil || len(pids) != 1 || strconv.Itoa(pids[0]) != run(t, "display-message", "-p", "-t", "="+decoy+":", "#{pane_pid}") {
					t.Errorf("decoy process roots changed: %v, %v", pids, err)
				}
			}
			if got := decoyState(); got != before {
				t.Fatal("operation on missing target changed decoy")
			}
		})
	}
}
