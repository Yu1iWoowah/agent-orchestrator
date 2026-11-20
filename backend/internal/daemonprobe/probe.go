// Package daemonprobe implements the loopback daemon handshake shared by local clients.
package daemonprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/daemonmeta"
)

// Timeout limits each daemon health or readiness probe.
const Timeout = 2 * time.Second

// MaxBody bounds a probe response from an unverified loopback peer.
const MaxBody = 64 << 10

// Result identifies the daemon that answered a health or readiness probe.
type Result struct {
	Status           string `json:"status"`
	Service          string `json:"service"`
	PID              int    `json:"pid"`
	ExecutablePath   string `json:"executablePath,omitempty"`
	WorkingDirectory string `json:"workingDirectory,omitempty"`
}

// Client confines local HTTP requests to the expected loopback origin.
func Client(base *http.Client, timeout time.Duration) *http.Client {
	client := *base
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

// Read returns one bounded, valid probe response.
func Read(ctx context.Context, client *http.Client, port int, path string) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d/%s", config.LoopbackHost, port, path), http.NoBody)
	if err != nil {
		return Result{}, err
	}
	resp, err := client.Do(req) // #nosec G704 -- the host is fixed loopback and the port was validated from the run-file.
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Result{}, fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	var body Result
	bodyReader := &io.LimitedReader{R: resp.Body, N: MaxBody + 1}
	decoder := json.NewDecoder(bodyReader)
	if err := decoder.Decode(&body); err != nil {
		return Result{}, fmt.Errorf("%s: decode response: %w", path, err)
	}
	if err := decoder.Decode(new(any)); err == nil {
		return Result{}, fmt.Errorf("%s: expected one JSON response", path)
	} else if err != io.EOF {
		return Result{}, fmt.Errorf("%s: decode response: %w", path, err)
	}
	if bodyReader.N == 0 {
		return Result{}, fmt.Errorf("%s: response exceeds %d bytes", path, MaxBody)
	}
	if body.Status == "" {
		return Result{}, fmt.Errorf("%s: missing status", path)
	}
	return body, nil
}

// VerifyOwner checks the claimed service and run-file process identity.
func VerifyOwner(probe Result, wantPID int, path string) error {
	if probe.Service != daemonmeta.ServiceName {
		return fmt.Errorf("%s: response is not from the Orchestrator daemon", path)
	}
	if probe.PID != wantPID {
		return fmt.Errorf("%s: daemon pid %d does not match run-file pid %d", path, probe.PID, wantPID)
	}
	return nil
}
