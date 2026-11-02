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
	Version       int    `json:"version"`
	BrowserToken  string `json:"browserToken"`
	AppToken      string `json:"appDiagnosticsToken"`
	OperatorToken string `json:"operatorConsentToken,omitempty"`
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
	if out.OperatorToken != "" && (len(out.OperatorToken) < 32 || len(out.OperatorToken) > 256 || out.OperatorToken == out.AppToken || out.OperatorToken == out.BrowserToken) {
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
	SchemaVersion int                                  `json:"schemaVersion,omitempty"`
	Type          string                               `json:"type"`
	Version       int                                  `json:"version,omitempty"`
	Token         string                               `json:"token,omitempty"`
	InstanceID    string                               `json:"instanceId,omitempty"`
	Epoch         string                               `json:"epoch,omitempty"`
	GrantID       string                               `json:"grantId,omitempty"`
	RequestID     string                               `json:"requestId,omitempty"`
	SessionID     domain.SessionID                     `json:"sessionId,omitempty"`
	Windows       []domain.AppDiagnosticsWindow        `json:"windows,omitempty"`
	Request       *domain.AppDiagnosticsCaptureRequest `json:"request,omitempty"`
	Snapshot      *domain.AppDiagnosticsSnapshot       `json:"snapshot,omitempty"`
	Audit         json.RawMessage                      `json:"audit,omitempty"`
	Code          string                               `json:"code,omitempty"`
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
	if b.send(conn, frame{Type: "hello-ok", Version: 1, SchemaVersion: 1, Epoch: epoch}) != nil {
		return
	}
	managementSeen := map[string]bool{}
	for {
		_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		if !scan.Scan() {
			return
		}
		var in frame
		if decode(scan.Bytes(), &in) != nil {
			return
		}
		if in.Type == "grant" || in.Type == "revoke" || in.Type == "audit" {
			if in.SchemaVersion != 1 || !safeID(in.RequestID) || managementSeen[in.RequestID] || len(managementSeen) >= 1024 {
				return
			}
			managementSeen[in.RequestID] = true
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
				code = managementFailure(err, "APP_DIAGNOSTICS_UNSUPPORTED")
			}
			if b.send(conn, frame{Type: "grant-result", SchemaVersion: 1, RequestID: in.RequestID, GrantID: grantID, Code: code}) != nil {
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
				code = managementFailure(err, "APP_DIAGNOSTICS_CAPABILITY_INVALID")
			}
			if b.send(conn, frame{Type: "revoke-result", SchemaVersion: 1, RequestID: in.RequestID, Code: code}) != nil {
				return
			}
		case "audit":
			authority, ok := b.authority.(interface {
				AuditRuntime(context.Context, string, string) (json.RawMessage, error)
			})
			if !ok {
				return
			}
			audit, err := authority.AuditRuntime(ctx, epoch, in.GrantID)
			code := ""
			if err != nil {
				code = managementFailure(err, "APP_DIAGNOSTICS_UNSUPPORTED")
			}
			if b.send(conn, frame{Type: "audit-result", SchemaVersion: 1, RequestID: in.RequestID, Audit: audit, Code: code}) != nil {
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
			if in.SchemaVersion != 1 {
				ch <- response{err: &domain.AppDiagnosticsError{Code: "APP_DIAGNOSTICS_INVALID_PAYLOAD", Status: 502}}
			} else if in.Code != "" {
				ch <- response{err: captureFailure(in.Code)}
			} else if in.Snapshot == nil || !domain.ValidAppDiagnosticsCaptureSnapshot(*in.Snapshot) {
				ch <- response{err: &domain.AppDiagnosticsError{Code: "APP_DIAGNOSTICS_INVALID_PAYLOAD", Status: 502}}
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
	if err := b.send(conn, frame{Type: "snapshot", SchemaVersion: 1, RequestID: id, Request: &in}); err != nil {
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

// Only fixed collector failures cross the private runtime boundary.
func captureFailure(code string) *domain.AppDiagnosticsError {
	status := 503
	switch code {
	case "APP_DIAGNOSTICS_PAYLOAD_TOO_LARGE":
		status = 413
	case "APP_DIAGNOSTICS_INVALID_PAYLOAD":
		status = 502
	case "APP_WINDOW_GONE", "APP_RENDERER_CHANGED":
		status = 409
	case "APP_SNAPSHOT_TIMEOUT":
		status = 504
	case "APP_DIAGNOSTICS_BUSY":
		status = 429
	case "APP_DIAGNOSTICS_DISABLED", "APP_RENDERER_UNAVAILABLE", "APP_DIAGNOSTICS_UNSUPPORTED":
	default:
		code = "APP_RENDERER_UNAVAILABLE"
	}
	return &domain.AppDiagnosticsError{Code: code, Status: status}
}

func managementFailure(err error, fallback string) string {
	var failure *domain.AppDiagnosticsError
	if errors.As(err, &failure) {
		switch failure.Code {
		case "APP_DIAGNOSTICS_CAPABILITY_INVALID", "APP_DIAGNOSTICS_UNSUPPORTED", "APP_DIAGNOSTICS_STORAGE_FAILED", "APP_DIAGNOSTICS_AUDIT_FAILED", "APP_DIAGNOSTICS_SESSION_UNAVAILABLE", "APP_DIAGNOSTICS_GRANT_EXPIRED", "APP_DIAGNOSTICS_CLEANUP_FAILED":
			return failure.Code
		}
	}
	return fallback
}

// NotifyGrantRevoked clears the current native owner's collector even when a
// grant is revoked through HTTP, expiry or session lifecycle rather than Settings.
func (b *Broker) NotifyGrantRevoked(epoch, id string) {
	b.mu.Lock()
	conn := b.conn
	current := b.state.Epoch
	b.mu.Unlock()
	if conn == nil || epoch != current {
		return
	}
	if err := b.send(conn, frame{Type: "grant-revoked", SchemaVersion: 1, GrantID: id}); err != nil {
		_ = conn.Close()
	}
}
