package cli

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/daemonprobe"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

// daemonHTTPClient applies the connection policy only to ordinary HTTP calls.
// In particular, stop retains the injected client's shutdown policy.
func (c *commandContext) daemonHTTPClient(timeout time.Duration) *http.Client {
	return daemonprobe.Client(c.deps.HTTPClient, timeout)
}

// inspectDaemonConnection observes HTTP identity and readiness without using
// process visibility, granting shutdown ownership, or changing the run-file.
// inspectDaemon remains the separate process-management inspection for stop.
func (c *commandContext) inspectDaemonConnection(ctx context.Context) (daemonStatus, error) {
	cfg, err := config.Load()
	if err != nil {
		return daemonStatus{}, err
	}
	st := daemonStatus{State: stateStopped, RunFile: cfg.RunFilePath, DataDir: cfg.DataDir}
	info, err := runfile.Read(cfg.RunFilePath)
	if err != nil {
		return daemonStatus{}, err
	}
	if info == nil {
		return st, nil
	}
	if info.PID <= 0 || info.Port <= 0 || info.Port > 65535 {
		return daemonStatus{}, fmt.Errorf("invalid daemon run-file %s: pid must be positive and port must be between 1 and 65535", cfg.RunFilePath)
	}
	st.PID, st.Port = info.PID, info.Port
	startedAt := info.StartedAt
	st.StartedAt = &startedAt
	client := c.daemonHTTPClient(probeTimeout)
	if transport, ok := client.Transport.(*http.Transport); ok {
		defer transport.CloseIdleConnections()
	}
	health, err := daemonprobe.Read(ctx, client, info.Port, "healthz")
	if err != nil {
		st.State, st.Error, st.probeErr = stateUnhealthy, err.Error(), err
		return st, nil
	}
	if err := daemonprobe.VerifyOwner(health, info.PID, "healthz"); err != nil {
		st.State, st.Error, st.probeErr = stateStale, err.Error(), err
		return st, nil
	}
	st.Health, st.ExecutablePath = health.Status, health.ExecutablePath
	if health.Status != "ok" {
		st.State, st.Error = stateUnhealthy, fmt.Sprintf("healthz: status %q", health.Status)
		return st, nil
	}
	ready, err := daemonprobe.Read(ctx, client, info.Port, "readyz")
	if err != nil {
		st.State, st.Error, st.probeErr = stateNotReady, err.Error(), err
		return st, nil
	}
	if err := daemonprobe.VerifyOwner(ready, info.PID, "readyz"); err != nil {
		st.State, st.Error, st.probeErr = stateStale, err.Error(), err
		return st, nil
	}
	st.Ready = ready.Status
	if ready.Status != string(stateReady) {
		st.State, st.Error = stateNotReady, fmt.Sprintf("readyz: status %q", ready.Status)
		return st, nil
	}
	st.State = stateReady
	return st, nil
}
