package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

// daemonHTTPClient applies the connection policy only to ordinary HTTP calls.
// In particular, stop retains the injected client's shutdown policy.
func (c *commandContext) daemonHTTPClient(timeout time.Duration) *http.Client {
	client := *c.deps.HTTPClient
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if transport, ok := transport.(*http.Transport); ok {
		local := transport.Clone()
		local.Proxy = nil
		client.Transport = local
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Timeout = timeout
	return &client
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
	health, err := readConnectionProbe(ctx, client, info.Port, "healthz")
	if err != nil {
		st.State, st.Error, st.probeErr = stateUnhealthy, err.Error(), err
		return st, nil
	}
	if err := verifyProbeOwner(health, info.PID, "healthz"); err != nil {
		st.State, st.Error, st.probeErr = stateStale, err.Error(), err
		return st, nil
	}
	st.Health, st.ExecutablePath = health.Status, health.ExecutablePath
	if health.Status != "ok" {
		st.State, st.Error = stateUnhealthy, fmt.Sprintf("healthz: status %q", health.Status)
		return st, nil
	}
	ready, err := readConnectionProbe(ctx, client, info.Port, "readyz")
	if err != nil {
		st.State, st.Error, st.probeErr = stateNotReady, err.Error(), err
		return st, nil
	}
	if err := verifyProbeOwner(ready, info.PID, "readyz"); err != nil {
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

func readConnectionProbe(ctx context.Context, client *http.Client, port int, path string) (probeResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d/%s", config.LoopbackHost, port, path), http.NoBody)
	if err != nil {
		return probeResult{}, err
	}
	resp, err := client.Do(req) // #nosec G704 -- the host is fixed loopback and the port was validated from the run-file.
	if err != nil {
		return probeResult{}, fmt.Errorf("%s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return probeResult{}, fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	var body probeResult
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&body); err != nil {
		return probeResult{}, fmt.Errorf("%s: decode response: %w", path, err)
	}
	if err := decoder.Decode(new(any)); err == nil {
		return probeResult{}, fmt.Errorf("%s: expected one JSON response", path)
	} else if err != io.EOF {
		return probeResult{}, fmt.Errorf("%s: decode response: %w", path, err)
	}
	if body.Status == "" {
		return probeResult{}, fmt.Errorf("%s: missing status", path)
	}
	return body, nil
}
