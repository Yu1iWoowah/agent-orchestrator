// Package appdiagnostics implements the private main-to-daemon diagnostics link.
// It is separate from Browser automation and exposes no evaluation or input.
package appdiagnostics

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// RuntimeAddressEnv publishes the non-secret private diagnostic locator.
const RuntimeAddressEnv = "AO_APP_DIAGNOSTICS_ADDRESS"

// Credentials are consumed exactly once from the app's private stdin pipe.
type Credentials struct {
	Version      int    `json:"version"`
	BrowserToken string `json:"browserToken"`
	AppToken     string `json:"appDiagnosticsToken"`
}

// ReadCredentials accepts the old Browser-only handoff without enabling diagnostics.
func ReadCredentials(r io.Reader) (Credentials, error) {
	reader := bufio.NewReader(io.LimitReader(r, 2049))
	data, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return Credentials{}, errors.New("invalid runtime handoff")
	}
	data = strings.TrimSpace(data)
	if data == "" || len(data) > 2048 {
		return Credentials{}, errors.New("invalid runtime handoff")
	}
	legacy := ""
	if !strings.HasPrefix(data, "{") {
		if len(data) > 256 {
			return Credentials{}, errors.New("invalid runtime handoff")
		}
		legacy = data
		extra, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return Credentials{}, errors.New("invalid runtime handoff")
		}
		if strings.TrimSpace(extra) == "" {
			return Credentials{BrowserToken: legacy}, nil
		}
		data = strings.TrimSpace(extra)
	}

	var out Credentials
	if err := decode([]byte(data), &out); err != nil || out.Version != 1 || out.BrowserToken == "" || len(out.BrowserToken) > 256 || len(out.AppToken) < 32 || len(out.AppToken) > 256 || out.AppToken == out.BrowserToken || (legacy != "" && legacy != out.BrowserToken) {
		return Credentials{}, errors.New("invalid runtime handoff")
	}
	return out, nil
}
func decode(raw []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if dec.Decode(new(any)) != io.EOF {
		return errors.New("invalid frame")
	}
	return nil
}

type frame struct {
	Type       string                               `json:"type"`
	Version    int                                  `json:"version,omitempty"`
	Token      string                               `json:"token,omitempty"`
	InstanceID string                               `json:"instanceId,omitempty"`
	Epoch      string                               `json:"epoch,omitempty"`
	GrantID    string                               `json:"grantId,omitempty"`
	RequestID  string                               `json:"requestId,omitempty"`
	SessionID  domain.SessionID                     `json:"sessionId,omitempty"`
	Windows    []domain.AppDiagnosticsWindow        `json:"windows,omitempty"`
	Request    *domain.AppDiagnosticsCaptureRequest `json:"request,omitempty"`
	Snapshot   *domain.AppDiagnosticsSnapshot       `json:"snapshot,omitempty"`
	Code       string                               `json:"code,omitempty"`
}
type response struct {
	snapshot domain.AppDiagnosticsSnapshot
	err      error
}

type grantAuthority interface {
	GrantRuntime(context.Context, domain.SessionID, string) (string, error)
	RevokeRuntime(context.Context, string, string) error
	Sweep(context.Context) error
}

// Broker accepts only one authenticated desktop owner at a time.
type Broker struct {
	mu        sync.Mutex
	writeMu   sync.Mutex
	token     string
	conn      net.Conn
	state     domain.AppDiagnosticsRuntimeState
	pending   map[string]chan response
	authority grantAuthority
}

// New creates a disabled runtime; authentication alone does not mark a collector ready.
func New(token string) *Broker { return &Broker{token: token, pending: map[string]chan response{}} }

// BindAuthority installs the trusted-runtime grant boundary before serving.
func (b *Broker) BindAuthority(service grantAuthority) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.authority = service
}

// State returns a copy; callers cannot mutate registered windows.
func (b *Broker) State() domain.AppDiagnosticsRuntimeState {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.state
	out.Windows = append([]domain.AppDiagnosticsWindow{}, out.Windows...)
	return out
}

// Serve owns the local IPC listener for this daemon epoch.
func (b *Broker) Serve(ctx context.Context, ln net.Listener) error {
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go b.serveConn(ctx, conn)
	}
}
func (b *Broker) send(conn net.Conn, out frame) error {
	b.writeMu.Lock()
	defer b.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	return json.NewEncoder(conn).Encode(out)
}
func (b *Broker) serveConn(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	scan := bufio.NewScanner(conn)
	scan.Buffer(make([]byte, 4096), 1<<20)
	if !scan.Scan() {
		return
	}
	var hello frame
	if decode(scan.Bytes(), &hello) != nil || hello.Type != "hello" || hello.Version != 1 || b.token == "" || subtle.ConstantTimeCompare([]byte(hello.Token), []byte(b.token)) != 1 || !safeID(hello.InstanceID) {
		_ = b.send(conn, frame{Type: "hello-rejected", Code: "APP_DIAGNOSTICS_CAPABILITY_INVALID"})
		return
	}
	b.mu.Lock()
	if b.conn != nil {
		b.mu.Unlock()
		_ = b.send(conn, frame{Type: "hello-rejected", Code: "APP_DIAGNOSTICS_BUSY"})
		return
	}
	epoch := uuid.NewString()
	b.conn = conn
	b.state = domain.AppDiagnosticsRuntimeState{InstanceID: hello.InstanceID, Epoch: epoch}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		if b.conn == conn {
			b.conn = nil
			b.state = domain.AppDiagnosticsRuntimeState{}
			for id, ch := range b.pending {
				ch <- response{err: &domain.AppDiagnosticsError{Code: "APP_NOT_CONNECTED", Status: 503}}
				delete(b.pending, id)
			}
		}
		authority := b.authority
		b.mu.Unlock()
		if authority != nil {
			_ = authority.Sweep(context.Background())
		}
	}()
	if b.send(conn, frame{Type: "hello-ok", Version: 1, Epoch: epoch}) != nil {
		return
	}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		if !scan.Scan() {
			return
		}
		var in frame
		if decode(scan.Bytes(), &in) != nil {
			return
		}
		switch in.Type {
		case "ping":
			if b.send(conn, frame{Type: "pong"}) != nil {
				return
			}
		case "ready":
			if len(in.Windows) > 16 {
				return
			}
			seen := map[string]bool{}
			for _, w := range in.Windows {
				if !safeID(w.ID) || w.Generation == 0 || seen[w.ID] {
					return
				}
				seen[w.ID] = true
			}
			b.mu.Lock()
			b.state.Windows = append([]domain.AppDiagnosticsWindow{}, in.Windows...)
			b.state.Ready = len(in.Windows) > 0
			b.mu.Unlock()
		case "grant":
			b.mu.Lock()
			authority := b.authority
			b.mu.Unlock()
			if authority == nil {
				return
			}
			grantID, err := authority.GrantRuntime(ctx, in.SessionID, epoch)
			code := ""
			if err != nil {
				code = "APP_DIAGNOSTICS_UNSUPPORTED"
			}
			if b.send(conn, frame{Type: "grant-result", RequestID: in.RequestID, GrantID: grantID, Code: code}) != nil {
				return
			}
		case "revoke":
			b.mu.Lock()
			authority := b.authority
			b.mu.Unlock()
			if authority == nil {
				return
			}
			code := ""
			if err := authority.RevokeRuntime(ctx, epoch, in.GrantID); err != nil {
				code = "APP_DIAGNOSTICS_CAPABILITY_INVALID"
			}
			if b.send(conn, frame{Type: "revoke-result", RequestID: in.RequestID, Code: code}) != nil {
				return
			}
		case "result":
			b.mu.Lock()
			ch, ok := b.pending[in.RequestID]
			if ok {
				delete(b.pending, in.RequestID)
			}
			b.mu.Unlock()
			if !ok {
				return
			}
			if in.Snapshot == nil || in.Code != "" {
				ch <- response{err: &domain.AppDiagnosticsError{Code: "APP_DIAGNOSTICS_UNSUPPORTED", Status: 503}}
			} else {
				ch <- response{snapshot: *in.Snapshot}
			}
		default:
			return
		}
	}
}
func safeID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if r != '-' && r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// Capture correlates one bounded request and propagates cancellation without retries.
func (b *Broker) Capture(ctx context.Context, in domain.AppDiagnosticsCaptureRequest) (domain.AppDiagnosticsSnapshot, error) {
	b.mu.Lock()
	conn := b.conn
	if conn == nil || !b.state.Ready {
		b.mu.Unlock()
		return domain.AppDiagnosticsSnapshot{}, &domain.AppDiagnosticsError{Code: "APP_DIAGNOSTICS_UNSUPPORTED", Status: 503}
	}
	id := uuid.NewString()
	ch := make(chan response, 1)
	b.pending[id] = ch
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.pending, id); b.mu.Unlock() }()
	if err := b.send(conn, frame{Type: "snapshot", RequestID: id, Request: &in}); err != nil {
		return domain.AppDiagnosticsSnapshot{}, err
	}
	select {
	case out := <-ch:
		return out.snapshot, out.err
	case <-ctx.Done():
		_ = b.send(conn, frame{Type: "cancel", RequestID: id})
		return domain.AppDiagnosticsSnapshot{}, ctx.Err()
	}
}
