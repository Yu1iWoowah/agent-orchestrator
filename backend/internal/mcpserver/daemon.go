package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/caller"
	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/daemonprobe"
	"github.com/aoagents/agent-orchestrator/backend/internal/runfile"
)

// NewDaemonClient calls the daemon the run file names, as the ao CLI does.
func NewDaemonClient() Daemon {
	return daemonClient{http: daemonprobe.Client(&http.Client{}, 2*time.Minute)}
}

type daemonClient struct{ http *http.Client }

func (d daemonClient) GetJSON(ctx context.Context, path string, out any) error {
	return d.do(ctx, http.MethodGet, path, nil, out)
}

func (d daemonClient) PostJSON(ctx context.Context, path string, body, out any) error {
	return d.do(ctx, http.MethodPost, path, body, out)
}

func (d daemonClient) do(ctx context.Context, method, path string, body, out any) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	info, err := runfile.Read(cfg.RunFilePath)
	if err != nil {
		return err
	}
	if info == nil {
		return errors.New("the Orchestrator daemon is not running")
	}
	if info.PID <= 0 || info.Port <= 0 || info.Port > 65535 {
		return errors.New("invalid daemon run-file: pid must be positive and port must be between 1 and 65535")
	}
	for _, probe := range []struct{ path, want string }{{"healthz", "ok"}, {"readyz", "ready"}} {
		probeCtx, cancel := context.WithTimeout(ctx, daemonprobe.Timeout)
		response, err := daemonprobe.Read(probeCtx, d.http, info.Port, probe.path)
		cancel()
		if err != nil {
			return err
		}
		if err := daemonprobe.VerifyOwner(response, info.PID, probe.path); err != nil {
			return err
		}
		if response.Status != probe.want {
			return fmt.Errorf("%s: status %q", probe.path, response.Status)
		}
	}
	var reader io.Reader = http.NoBody
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	url := fmt.Sprintf("http://%s:%d/api/v1/%s", config.LoopbackHost, info.Port, path)
	req, err := http.NewRequestWithContext(ctx, method, url, reader) // #nosec G704 -- daemon host is fixed loopback; path is an internal API route.
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if id := strings.TrimSpace(os.Getenv("AO_SESSION_ID")); id != "" && os.Getenv(caller.ShellTerminalEnv) == "" {
		req.Header.Set(caller.Header, id)
	}
	if side := os.Getenv("AO_SIDE_CONVERSATION_ID"); side != "" {
		req.Header.Set("X-AO-Side-Conversation", side)
	}
	resp, err := d.http.Do(req) // #nosec G704 -- request target is the fixed loopback daemon URL above.
	if err != nil {
		return fmt.Errorf("call daemon: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct{ Message, Code, RequestID string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Message == "" {
			return fmt.Errorf("daemon returned HTTP %d", resp.StatusCode)
		}
		msg := e.Message
		if e.Code != "" {
			msg += " (" + e.Code + ")"
		}
		if e.RequestID != "" {
			msg += " [request " + e.RequestID + "]"
		}
		return errors.New(msg)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
