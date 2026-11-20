package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

const defaultStopTimeout = 10 * time.Second

type stopOptions struct {
	timeout time.Duration
	json    bool
}

func newStopCommand(ctx *commandContext) *cobra.Command {
	opts := stopOptions{timeout: defaultStopTimeout}
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the Orchestrator daemon",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := ctx.stopDaemon(cmd.Context(), opts)
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), st)
			}
			if st.State == stateStopped {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "Orchestrator daemon stopped")
				return err
			}
			return writeStatus(cmd, st)
		},
	}
	cmd.Flags().DurationVar(&opts.timeout, "timeout", defaultStopTimeout, "How long to wait for daemon shutdown")
	cmd.Flags().BoolVar(&opts.json, "json", false, "Output stop result as JSON")
	return cmd
}

func (c *commandContext) stopDaemon(ctx context.Context, opts stopOptions) (daemonStatus, error) {
	cfg, err := config.Load()
	if err != nil {
		return daemonStatus{}, err
	}
	st, err := c.inspectDaemon(ctx)
	if err != nil {
		return daemonStatus{}, err
	}
	switch st.State {
	case stateStopped:
		return st, nil
	case stateStale:
		if err := runfile.Remove(cfg.RunFilePath); err != nil {
			return daemonStatus{}, err
		}
		return daemonStatus{State: stateStopped, RunFile: cfg.RunFilePath, DataDir: cfg.DataDir}, nil
	}
	if !st.owned {
		if st.Error != "" {
			return daemonStatus{}, fmt.Errorf("daemon pid %d ownership could not be verified: %s", st.PID, st.Error)
		}
		return daemonStatus{}, fmt.Errorf("daemon pid %d ownership could not be verified", st.PID)
	}

	if err := c.requestShutdown(ctx, st.Port); err != nil {
		return daemonStatus{}, fmt.Errorf("request daemon shutdown: %w", err)
	}
	// inspectDaemon verified this daemon either by its PID or, when the PID is
	// hidden from this process (see probeOwnedDaemon), over loopback only.
	if !st.pidVisible {
		return c.waitForHiddenStopped(ctx, st, opts.timeout)
	}
	return c.waitForStopped(ctx, st.PID, cfg.RunFilePath, cfg.DataDir, opts.timeout)
}

// waitForHiddenStopped waits for a daemon whose PID this process cannot see
// to finish the shutdown it accepted. Without the PID, the daemon's own
// run-file is the only exit signal: the daemon closes its listeners as soon as
// it starts draining and removes the run-file once it has drained, so a failed
// probe does not mean it has stopped. The CLI never removes the run-file here.
// It cannot tell a draining daemon from a crashed one, a successor daemon may
// publish its own run-file at the same path, and a sandboxed CLI often cannot
// write the file at all.
func (c *commandContext) waitForHiddenStopped(ctx context.Context, original daemonStatus, timeout time.Duration) (daemonStatus, error) {
	if timeout <= 0 {
		timeout = defaultStopTimeout
	}
	stopped := daemonStatus{State: stateStopped, RunFile: original.RunFile, DataDir: original.DataDir}
	deadline := c.deps.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return daemonStatus{}, ctx.Err()
		default:
		}
		info, err := runfile.Read(original.RunFile)
		if err != nil {
			return daemonStatus{}, err
		}
		if info == nil || info.PID != original.PID || info.Port != original.Port || !info.StartedAt.Equal(*original.StartedAt) {
			// Removed by the daemon, or replaced by a new one.
			return stopped, nil
		}
		if !c.deps.Now().Before(deadline) {
			return daemonStatus{}, fmt.Errorf("daemon pid %d accepted shutdown but exit remains unconfirmed after %s", original.PID, timeout)
		}
		c.deps.Sleep(100 * time.Millisecond)
	}
}

func (c *commandContext) requestShutdown(ctx context.Context, port int) error {
	reqCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, fmt.Sprintf("http://%s:%d/shutdown", config.LoopbackHost, port), http.NoBody)
	if err != nil {
		return err
	}
	// Keep the shared 2s probe client unchanged. Shutdown waits for the
	// server's 10s preflight under the dedicated request context above.
	client := *c.deps.HTTPClient
	client.Timeout = 0
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var body struct {
			Status         string `json:"status"`
			BlockedSession string `json:"blockedSession"`
			BlockedReason  string `json:"blockedReason"`
		}
		if resp.StatusCode == http.StatusConflict && json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body) == nil && body.Status == "shutdown_not_settled" {
			blocked := ""
			if body.BlockedReason != "" {
				blocked = " (blocked by " + body.BlockedReason
				if body.BlockedSession != "" {
					blocked += " in session " + body.BlockedSession
				}
				blocked += ")"
			}
			return fmt.Errorf("HTTP 409: shutdown_not_settled%s: provider execution or delivery evidence is not settled; keep the daemon running and retry after reconciliation", blocked)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *commandContext) waitForStopped(ctx context.Context, pid int, runFilePath, dataDir string, timeout time.Duration) (daemonStatus, error) {
	if timeout <= 0 {
		timeout = defaultStopTimeout
	}
	deadline := c.deps.Now().Add(timeout)
	for {
		select {
		case <-ctx.Done():
			return daemonStatus{}, ctx.Err()
		default:
		}

		info, err := runfile.Read(runFilePath)
		if err != nil {
			return daemonStatus{}, err
		}
		alive := c.deps.ProcessAlive(pid)
		if !alive {
			// Only remove the run-file if it still belongs to the process we
			// stopped. A concurrent `ao start` may have already written a new
			// run-file for a different daemon; removing that would corrupt its
			// handshake and make a live daemon look stopped.
			if info != nil && info.PID == pid {
				if err := runfile.Remove(runFilePath); err != nil {
					return daemonStatus{}, err
				}
			}
			return daemonStatus{State: stateStopped, RunFile: runFilePath, DataDir: dataDir}, nil
		}
		if info == nil {
			// The run-file is the daemon's own liveness marker; it removes it as
			// it shuts down, before the OS process has necessarily exited. Once
			// the marker is gone the daemon has committed to stopping, so treat
			// that as stopped.
			//
			// We still poll for full process exit as a best effort so Windows
			// releases inherited handles such as daemon.log before callers clean
			// up the data directory, but exceeding the timeout is NOT an error:
			// with no desktop client connected the daemon can drain its
			// background workers slower than the stop timeout, and failing here
			// made `ao stop` spuriously report failure (issue #2214).
			if !c.deps.Now().Before(deadline) {
				return daemonStatus{State: stateStopped, RunFile: runFilePath, DataDir: dataDir}, nil
			}
			c.deps.Sleep(100 * time.Millisecond)
			continue
		}
		if !c.deps.Now().Before(deadline) {
			return daemonStatus{}, fmt.Errorf("daemon pid %d did not stop within %s", pid, timeout)
		}
		c.deps.Sleep(100 * time.Millisecond)
	}
}
