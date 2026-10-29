// Package appdiagnostics owns short-lived, local-only app diagnostic grants.
// Only the authenticated desktop runtime may issue a grant. HTTP cannot enable it.
package appdiagnostics

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// Error is the fixed safe failure vocabulary shared with the private runtime.
type Error = domain.AppDiagnosticsError

// Window identifies an app shell document, never a Browser tab.
type Window = domain.AppDiagnosticsWindow

// RuntimeState is supplied by the independent authenticated desktop channel.
type RuntimeState = domain.AppDiagnosticsRuntimeState

// CaptureRequest has no selector, code, path, or remote host.
type CaptureRequest = domain.AppDiagnosticsCaptureRequest

// Snapshot is metadata-only until a typed collector is installed.
type Snapshot = domain.AppDiagnosticsSnapshot

func failure(code string, status int) error { return &Error{Code: code, Status: status} }

// Record is the stored result bound to exactly one grant.
type Record struct {
	ID        string    `json:"id"`
	GrantID   string    `json:"grantId"`
	RequestID string    `json:"requestId"`
	CreatedAt time.Time `json:"createdAt"`
	Snapshot  Snapshot  `json:"snapshot"`
}

// Credential is written owner-only by the daemon, never returned by HTTP or to renderer.
type Credential struct {
	ProjectID  domain.ProjectID `json:"projectId"`
	ID         string           `json:"grantId"`
	Token      string           `json:"token"`
	SessionID  domain.SessionID `json:"sessionId"`
	InstanceID string           `json:"instanceId"`
	ExpiresAt  time.Time        `json:"expiresAt"`
}

// Audit records bounded identifiers and outcomes, never snapshot contents.
type Audit struct {
	ReadErrorCount              uint64           `json:"readErrorCount,omitempty"`
	InvalidSnapshotDeletedCount uint64           `json:"invalidSnapshotDeletedCount,omitempty"`
	WindowID                    string           `json:"windowId,omitempty"`
	Bytes                       int              `json:"bytes,omitempty"`
	Partial                     *bool            `json:"partial,omitempty"`
	RedactionSchemaVersion      int              `json:"redactionSchemaVersion,omitempty"`
	SkippedCount                uint64           `json:"skippedCount,omitempty"`
	ReasonCode                  string           `json:"reasonCode,omitempty"`
	Count                       uint64           `json:"count,omitempty"`
	RequestID                   string           `json:"requestId"`
	GrantID                     string           `json:"grantId"`
	SessionID                   domain.SessionID `json:"sessionId"`
	At                          time.Time        `json:"at"`
	Outcome                     string           `json:"outcome"`
}

// AuditLog explicitly marks when retained history has been truncated.
type AuditLog struct {
	Partial                     bool       `json:"partial"`
	ReadErrorCount              uint64     `json:"readErrorCount,omitempty"`
	InvalidSnapshotDeletedCount uint64     `json:"invalidSnapshotDeletedCount,omitempty"`
	SkippedCount                uint64     `json:"skippedCount,omitempty"`
	Records                     []Audit    `json:"records"`
	OldestRetainedAt            *time.Time `json:"oldestRetainedAt"`
	DroppedCount                uint64     `json:"droppedCount"`
}

// SnapshotList reports partial readable results and cumulative safe I/O counts.
type SnapshotList struct {
	Partial        bool     `json:"partial"`
	Records        []Record `json:"records"`
	ReadErrorCount uint64   `json:"readErrorCount,omitempty"`
}

// RevokeResult distinguishes an effective revocation from its disk cleanup.
type RevokeResult struct {
	Revoked bool   `json:"revoked"`
	Cleanup string `json:"cleanup"`
}

// Runtime is the daemon-to-main port. Browser runtime does not implement this protocol.
type Runtime interface {
	State() RuntimeState
	Capture(context.Context, CaptureRequest) (Snapshot, error)
}
type sessionReader interface {
	Get(context.Context, domain.SessionID) (domain.Session, error)
}
type grantState struct {
	credential Credential
	verifier   [32]byte
	launch     string
	epoch      string
	cancel     context.CancelFunc
}

// Service serializes grant revocation, audit and snapshot commits.
type Service struct {
	deniedID         string
	deniedCount      uint64
	unavailableID    string
	unavailableCount uint64

	mu          sync.Mutex
	sessions    sessionReader
	runtime     Runtime
	now         func() time.Time
	store       *diskStore
	grants      map[string]*grantState
	busy        bool
	lastCapture time.Time
	closed      bool
}

// New opens confined diagnostic storage and discards credentials from older daemon launches.
func New(dataDir string, sessions sessionReader, runtime Runtime, now func() time.Time) (*Service, error) {
	store, err := openStore(dataDir)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	s := &Service{sessions: sessions, runtime: runtime, now: now, store: store, grants: map[string]*grantState{}, deniedID: uuid.NewString(), unavailableID: uuid.NewString()}
	if err := store.reset(now()); err != nil {
		_ = store.root.Close()
		return nil, err
	}
	return s, nil
}

// issueGrant is reachable only through the trusted runtime port, never HTTP.
func (s *Service) issueGrant(ctx context.Context, id domain.SessionID, epoch string) (Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var zero Credential
	if s.closed || s.runtime == nil || s.sessions == nil {
		return zero, failure("APP_DIAGNOSTICS_UNSUPPORTED", 503)
	}
	state := s.runtime.State()
	if !state.Ready || state.Epoch != epoch || state.InstanceID == "" || len(state.Windows) == 0 {
		return zero, failure("APP_DIAGNOSTICS_UNSUPPORTED", 503)
	}
	row, err := s.sessions.Get(ctx, id)
	if err != nil || row.ID != id || row.IsTerminated || row.Kind != domain.KindOrchestrator || launchID(row) == "\x00" {
		return zero, failure("APP_DIAGNOSTICS_CAPABILITY_INVALID", 403)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return zero, err
	}
	credential := Credential{ProjectID: row.ProjectID, ID: uuid.NewString(), Token: base64.RawURLEncoding.EncodeToString(raw), SessionID: id, InstanceID: state.InstanceID, ExpiresAt: s.now().Add(15 * time.Minute)}
	g := &grantState{credential: credential, verifier: sha256.Sum256([]byte(credential.Token)), launch: launchID(row), epoch: epoch}
	if err := s.store.write("grants", credential.ID, credential); err != nil {
		return zero, failure("APP_DIAGNOSTICS_STORAGE_FAILED", 500)
	}
	if err := s.store.audit(Audit{RequestID: uuid.NewString(), GrantID: credential.ID, SessionID: id, At: s.now(), Outcome: "granted"}); err != nil {
		_ = s.store.remove("grants", credential.ID)
		return zero, failure("APP_DIAGNOSTICS_AUDIT_FAILED", 500)
	}
	// Only the verifier stays in memory. The raw credential remains in its owner-only file.
	g.credential.Token = ""
	s.grants[credential.ID] = g
	return credential, nil
}
func launchID(row domain.Session) string {
	return row.Metadata.RuntimeLaunchID + "\x00" + row.Metadata.ControllerGeneration
}
func (s *Service) authorize(ctx context.Context, token string) (*grantState, error) {
	if s.closed || token == "" || len(token) > 128 {
		return nil, failure("APP_DIAGNOSTICS_CAPABILITY_INVALID", 403)
	}
	hash := sha256.Sum256([]byte(token))
	var found *grantState
	for _, g := range s.grants {
		if subtle.ConstantTimeCompare(hash[:], g.verifier[:]) == 1 {
			found = g
		}
	}
	if found == nil {
		return nil, failure("APP_DIAGNOSTICS_CAPABILITY_INVALID", 403)
	}
	return s.authorizeGrant(ctx, found)
}
func (s *Service) authorizeGrant(ctx context.Context, found *grantState) (*grantState, error) {
	row, err := s.sessions.Get(ctx, found.credential.SessionID)
	state := s.runtime.State()
	// Querying the session can itself cross the deadline. Check time after I/O.
	code := ""
	if !s.now().Before(found.credential.ExpiresAt) {
		code = "APP_DIAGNOSTICS_GRANT_EXPIRED"
	}
	var lookup *apierr.Error
	missing := errors.As(err, &lookup) && lookup.Code == "SESSION_NOT_FOUND"
	if state.InstanceID != found.credential.InstanceID || state.Epoch != found.epoch {
		code = "APP_DIAGNOSTICS_CAPABILITY_INVALID"
	}
	if err != nil && !missing && code == "" {
		return nil, failure("APP_DIAGNOSTICS_SESSION_UNAVAILABLE", 503)
	}
	if missing || (err == nil && (row.ID != found.credential.SessionID || row.ProjectID != found.credential.ProjectID || row.IsTerminated || row.Kind != domain.KindOrchestrator || launchID(row) != found.launch)) {
		code = "APP_DIAGNOSTICS_CAPABILITY_INVALID"
	}
	if code != "" {
		_ = s.revoke(found)
		return nil, failure(code, 403)
	}
	return found, nil
}

// authorizeDelivery is the final, mutex-protected delivery decision after all
// content/audit I/O. Reauthorization may perform I/O too, so authorizeGrant
// checks expiry after its query. No further storage work follows a successful fence.
func (s *Service) authorizeDelivery(ctx context.Context, g *grantState) error {
	if s.closed || s.grants[g.credential.ID] != g {
		return failure("APP_DIAGNOSTICS_CAPABILITY_INVALID", 403)
	}
	if ctx.Err() != nil {
		return failure("APP_SNAPSHOT_TIMEOUT", 504)
	}
	if _, err := s.authorizeGrant(ctx, g); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return failure("APP_SNAPSHOT_TIMEOUT", 504)
	}
	return nil
}

func (s *Service) failDelivery(audit Audit, err error) error {
	audit.Outcome = "failed"
	audit.At = s.now()
	var typed *Error
	if errors.As(err, &typed) {
		audit.ReasonCode = typed.Code
	}
	if s.store.audit(audit) != nil {
		return failure("APP_DIAGNOSTICS_AUDIT_FAILED", 500)
	}
	return err
}

// Status never reveals state without a current session-scoped grant.
func (s *Service) Status(ctx context.Context, token string) (RuntimeState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.authorize(ctx, token)
	if err != nil {
		return RuntimeState{}, err
	}
	state := s.runtime.State()
	if err := s.authorizeDelivery(ctx, g); err != nil {
		return RuntimeState{}, err
	}
	return state, nil
}

// Capture does not retry or queue. A single runtime capture may be pending.
func (s *Service) Capture(ctx context.Context, token string, in CaptureRequest) (Record, error) {
	s.mu.Lock()
	g, err := s.authorize(ctx, token)
	if err != nil {
		audit := Audit{RequestID: s.deniedID, At: s.now(), Outcome: "denied"}
		var typed *Error
		if errors.As(err, &typed) && typed.Code == "APP_DIAGNOSTICS_SESSION_UNAVAILABLE" {
			if s.unavailableCount < ^uint64(0) {
				s.unavailableCount++
			}
			audit.RequestID = s.unavailableID
			audit.Outcome = "unavailable"
			audit.ReasonCode = "APP_DIAGNOSTICS_SESSION_UNAVAILABLE"
			audit.Count = s.unavailableCount
		} else {
			if s.deniedCount < ^uint64(0) {
				s.deniedCount++
			}
			audit.Count = s.deniedCount
		}
		if auditErr := s.store.audit(audit); auditErr != nil {
			s.mu.Unlock()
			return Record{}, failure("APP_DIAGNOSTICS_AUDIT_FAILED", 500)
		}
		s.mu.Unlock()
		return Record{}, err
	}
	if !s.runtime.State().Ready {
		s.mu.Unlock()
		return Record{}, failure("APP_RENDERER_UNAVAILABLE", 503)
	}
	if in.ProjectID != "" && in.ProjectID != string(g.credential.ProjectID) {
		s.mu.Unlock()
		return Record{}, failure("APP_DIAGNOSTICS_CAPABILITY_INVALID", 403)
	}
	in.ProjectID = string(g.credential.ProjectID)
	if s.busy {
		s.mu.Unlock()
		return Record{}, failure("APP_DIAGNOSTICS_BUSY", 429)
	}
	// Do not accumulate more captures while a rejected file awaits cleanup.
	if err := s.store.retryDiscardedSnapshots(""); err != nil {
		s.mu.Unlock()
		return Record{}, failure("APP_DIAGNOSTICS_CLEANUP_FAILED", 500)
	}
	if !s.lastCapture.IsZero() && s.now().Sub(s.lastCapture) < 2*time.Second {
		s.mu.Unlock()
		return Record{}, failure("APP_DIAGNOSTICS_RATE_LIMITED", 429)
	}
	state := s.runtime.State()
	if in.WindowID == "" {
		if len(state.Windows) != 1 {
			s.mu.Unlock()
			return Record{}, failure("APP_WINDOW_AMBIGUOUS", 409)
		}
		in.WindowID = state.Windows[0].ID
	}
	var generation uint64
	for _, w := range state.Windows {
		if w.ID == in.WindowID {
			generation = w.Generation
		}
	}
	if generation == 0 {
		s.mu.Unlock()
		return Record{}, failure("APP_WINDOW_GONE", 409)
	}
	audit := Audit{WindowID: in.WindowID, RequestID: uuid.NewString(), GrantID: g.credential.ID, SessionID: g.credential.SessionID, At: s.now(), Outcome: "accepted"}
	if err := s.store.audit(audit); err != nil {
		s.mu.Unlock()
		return Record{}, failure("APP_DIAGNOSTICS_AUDIT_FAILED", 500)
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	g.cancel = cancel
	s.busy = true
	s.lastCapture = s.now()
	s.mu.Unlock()
	snapshot, captureErr := s.runtime.Capture(callCtx, in)
	s.mu.Lock()
	defer s.mu.Unlock()
	defer cancel()
	s.busy = false
	g.cancel = nil
	_, authErr := s.authorize(context.WithoutCancel(ctx), token)
	if captureErr != nil || callCtx.Err() != nil || authErr != nil {
		audit.At = s.now()
		audit.Outcome = "failed"
		if err := s.store.audit(audit); err != nil {
			return Record{}, failure("APP_DIAGNOSTICS_AUDIT_FAILED", 500)
		}
		if authErr != nil {
			return Record{}, authErr
		}
		if callCtx.Err() != nil {
			return Record{}, failure("APP_SNAPSHOT_TIMEOUT", 504)
		}
		var typed *Error
		if errors.As(captureErr, &typed) {
			return Record{}, typed
		}
		return Record{}, failure("APP_RENDERER_UNAVAILABLE", 503)
	}
	if !domain.ValidAppDiagnosticsCaptureSnapshot(snapshot) || snapshot.InstanceID != state.InstanceID || snapshot.WindowID != in.WindowID || snapshot.Generation != generation {
		audit.Outcome = "invalid_payload"
		_ = s.store.audit(audit)
		return Record{}, failure("APP_DIAGNOSTICS_INVALID_PAYLOAD", 502)
	}
	currentGeneration := uint64(0)
	for _, w := range s.runtime.State().Windows {
		if w.ID == in.WindowID {
			currentGeneration = w.Generation
		}
	}
	if currentGeneration != generation {
		audit.Outcome = "renderer_changed"
		if err := s.store.audit(audit); err != nil {
			return Record{}, failure("APP_DIAGNOSTICS_AUDIT_FAILED", 500)
		}
		return Record{}, failure("APP_RENDERER_CHANGED", 409)
	}

	encoded, err := json.Marshal(snapshot)
	if err != nil {
		audit.Outcome = "invalid_payload"
		_ = s.store.audit(audit)
		return Record{}, failure("APP_DIAGNOSTICS_INVALID_PAYLOAD", 502)
	}
	if len(encoded) > 1<<20 {
		audit.Outcome = "invalid_payload"
		if err := s.store.audit(audit); err != nil {
			return Record{}, failure("APP_DIAGNOSTICS_AUDIT_FAILED", 500)
		}
		return Record{}, failure("APP_DIAGNOSTICS_PAYLOAD_TOO_LARGE", 413)
	}
	partial := snapshot.Completeness == "partial"
	audit.Bytes = len(encoded)
	audit.Partial = &partial
	// The collector and retained metadata-only snapshots share redaction schema v1.
	audit.RedactionSchemaVersion = 1
	record := Record{ID: uuid.NewString(), GrantID: g.credential.ID, RequestID: audit.RequestID, CreatedAt: s.now(), Snapshot: snapshot}
	if err := s.store.write("snapshots", record.ID, record); err != nil {
		audit.Outcome = "storage_failed"
		_ = s.store.audit(audit)
		return Record{}, failure("APP_DIAGNOSTICS_STORAGE_FAILED", 500)
	}
	audit.Outcome = "completed"
	audit.At = s.now()
	if err := s.store.audit(audit); err != nil {
		_ = s.store.discardSnapshot(record.ID, record.GrantID)
		return Record{}, failure("APP_DIAGNOSTICS_AUDIT_FAILED", 500)
	}
	if err := s.store.prune(s.now()); err != nil {
		_ = s.store.discardSnapshot(record.ID, record.GrantID)
		audit.Outcome = "storage_failed"
		if err := s.store.audit(audit); err != nil {
			return Record{}, failure("APP_DIAGNOSTICS_AUDIT_FAILED", 500)
		}
		return Record{}, failure("APP_DIAGNOSTICS_STORAGE_FAILED", 500)
	}
	if err := s.authorizeDelivery(callCtx, g); err != nil {
		cleanupErr := s.store.discardSnapshot(record.ID, record.GrantID)
		// Keep the rejection cause in the audit even when cleanup is the error
		// returned to the caller. An audit write failure still takes precedence.
		deliveryErr := s.failDelivery(audit, err)
		if cleanupErr != nil {
			var auditErr *Error
			if !errors.As(deliveryErr, &auditErr) || auditErr.Code != "APP_DIAGNOSTICS_AUDIT_FAILED" {
				deliveryErr = failure("APP_DIAGNOSTICS_CLEANUP_FAILED", 500)
			}
		}
		return Record{}, deliveryErr
	}
	return record, nil
}

// Read reauthorizes both the live grant and ownership; snapshot ids are not capabilities.
func (s *Service) Read(ctx context.Context, token, id string) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.authorize(ctx, token)
	if err != nil {
		return Record{}, err
	}
	var out Record
	if _, err := uuid.Parse(id); err != nil {
		return Record{}, failure("APP_SNAPSHOT_NOT_FOUND", 404)
	}
	if err := s.store.read("snapshots", id, &out); err != nil {
		if errors.Is(err, errInvalidFile) || errors.Is(err, os.ErrNotExist) {
			return Record{}, failure("APP_SNAPSHOT_NOT_FOUND", 404)
		}
		s.store.recordReadError()
		if err := s.store.retentionSummary(s.now()); err != nil {
			return Record{}, failure("APP_DIAGNOSTICS_AUDIT_FAILED", 500)
		}
		return Record{}, failure("APP_DIAGNOSTICS_READ_UNAVAILABLE", 503)
	}
	if !validSnapshotRecord(id, out) || out.GrantID != g.credential.ID || !s.now().Before(out.CreatedAt.Add(24*time.Hour)) {
		return Record{}, failure("APP_SNAPSHOT_NOT_FOUND", 404)
	}
	audit := Audit{RequestID: uuid.NewString(), GrantID: g.credential.ID, SessionID: g.credential.SessionID, At: s.now(), Outcome: "read"}
	if err := s.store.audit(audit); err != nil {
		return Record{}, failure("APP_DIAGNOSTICS_AUDIT_FAILED", 500)
	}
	if err := s.authorizeDelivery(ctx, g); err != nil {
		return Record{}, s.failDelivery(audit, err)
	}
	return out, nil
}

// Revoke cancels the pending operation and removes this grant's files.
func (s *Service) Revoke(ctx context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.authorize(ctx, token)
	if err != nil {
		return err
	}
	return s.revoke(g)
}
func (s *Service) revoke(g *grantState) error {
	out, err := s.revokeResult(g)
	if err == nil && out.Cleanup == "failed" {
		return failure("APP_DIAGNOSTICS_CLEANUP_FAILED", 500)
	}
	return err
}
func (s *Service) revokeResult(g *grantState) (RevokeResult, error) {
	if g.cancel != nil {
		g.cancel()
	}
	delete(s.grants, g.credential.ID)
	if owner, ok := s.runtime.(interface{ NotifyGrantRevoked(string, string) }); ok {
		owner.NotifyGrantRevoked(g.epoch, g.credential.ID)
	}

	out := RevokeResult{Revoked: true, Cleanup: "complete"}
	cleanupErr := s.store.removeGrant(g.credential.ID, s.now())
	audit := Audit{RequestID: uuid.NewString(), GrantID: g.credential.ID, SessionID: g.credential.SessionID, At: s.now(), Outcome: "revoked"}
	if cleanupErr != nil {
		out.Cleanup = "failed"
		audit.ReasonCode = "APP_DIAGNOSTICS_CLEANUP_FAILED"
	} else if s.store.pendingRevocations[g.credential.ID] {
		out.Cleanup = "pending"
		audit.ReasonCode = "APP_DIAGNOSTICS_CLEANUP_PENDING"
	}
	if err := s.store.audit(audit); err != nil {
		return out, failure("APP_DIAGNOSTICS_AUDIT_FAILED", 500)
	}
	return out, nil
}

// Sweep is called periodically by the daemon and on runtime disconnect.
func (s *Service) Sweep(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var failures []error
	for _, g := range s.grants {
		// Use the same deterministic fences as HTTP authorization. Transient lookup
		// errors leave the credential and existing snapshots intact.
		row, err := s.sessions.Get(ctx, g.credential.SessionID)
		state := s.runtime.State()
		var lookup *apierr.Error
		missing := errors.As(err, &lookup) && lookup.Code == "SESSION_NOT_FOUND"
		invalid := missing || !s.now().Before(g.credential.ExpiresAt) || state.Epoch != g.epoch || state.InstanceID != g.credential.InstanceID
		if err == nil {
			invalid = invalid || row.ID != g.credential.SessionID || row.Kind != domain.KindOrchestrator || row.IsTerminated || row.ProjectID != g.credential.ProjectID || launchID(row) != g.launch
		}
		if invalid {
			if err := s.revoke(g); err != nil {
				failures = append(failures, err)
			}
		} else if err != nil {
			failures = append(failures, failure("APP_DIAGNOSTICS_SESSION_UNAVAILABLE", 503))
		}
	}
	if err := s.store.prune(s.now()); err != nil {
		failures = append(failures, failure("APP_DIAGNOSTICS_STORAGE_FAILED", 500))
	}
	return errors.Join(failures...)
}

// Close revokes all grants before closing confined storage.
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var first error
	for _, g := range s.grants {
		if err := s.revoke(g); err != nil && first == nil {
			first = err
		}
	}
	if err := s.store.root.Close(); err != nil && first == nil {
		first = fmt.Errorf("close diagnostic storage: %w", err)
	}
	return first
}

// List exposes only this grant's unexpired records and audits the read.
func (s *Service) List(ctx context.Context, token string) (SnapshotList, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.authorize(ctx, token)
	if err != nil {
		return SnapshotList{}, err
	}
	if err := s.store.pruneForGrant(s.now(), g.credential.ID); err != nil {
		return SnapshotList{}, failure("APP_DIAGNOSTICS_STORAGE_FAILED", 500)
	}
	docs, err := documents[Record](s.store, "snapshots", validSnapshotRecord)
	if err != nil && !errors.Is(err, errReadUnavailable) {
		return SnapshotList{}, failure("APP_DIAGNOSTICS_STORAGE_FAILED", 500)
	}
	out := []Record{}
	for _, doc := range docs {
		row := doc.value
		if row.GrantID == g.credential.ID {
			out = append(out, row)
		}
	}
	audit := Audit{RequestID: uuid.NewString(), GrantID: g.credential.ID, SessionID: g.credential.SessionID, At: s.now(), Outcome: "list"}
	if err := s.store.audit(audit); err != nil {
		return SnapshotList{}, failure("APP_DIAGNOSTICS_AUDIT_FAILED", 500)
	}
	if err := s.authorizeDelivery(ctx, g); err != nil {
		return SnapshotList{}, s.failDelivery(audit, err)
	}
	return SnapshotList{Partial: errors.Is(err, errReadUnavailable), Records: out, ReadErrorCount: s.store.readErrors}, nil
}

// Audits excludes other grants, including those issued to the same session.
func (s *Service) Audits(ctx context.Context, token string) (AuditLog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.authorize(ctx, token)
	if err != nil {
		return AuditLog{}, err
	}
	audit := Audit{RequestID: uuid.NewString(), GrantID: g.credential.ID, SessionID: g.credential.SessionID, At: s.now(), Outcome: "audit_read"}
	out, err := s.auditsForGrant(g, audit)
	if err != nil {
		return AuditLog{}, err
	}
	if err := s.authorizeDelivery(ctx, g); err != nil {
		return AuditLog{}, s.failDelivery(audit, err)
	}
	return out, nil
}
func (s *Service) auditsForGrant(g *grantState, audit Audit) (AuditLog, error) {
	if err := s.store.pruneForGrant(s.now(), g.credential.ID); err != nil {
		return AuditLog{}, failure("APP_DIAGNOSTICS_STORAGE_FAILED", 500)
	}
	if err := s.store.audit(audit); err != nil {
		return AuditLog{}, failure("APP_DIAGNOSTICS_AUDIT_FAILED", 500)
	}
	docs, err := documents[Audit](s.store, "audit", nil)
	if err != nil && !errors.Is(err, errReadUnavailable) {
		return AuditLog{}, failure("APP_DIAGNOSTICS_STORAGE_FAILED", 500)
	}
	if errors.Is(err, errReadUnavailable) {
		if err := s.store.retentionSummary(s.now()); err != nil {
			return AuditLog{}, failure("APP_DIAGNOSTICS_AUDIT_FAILED", 500)
		}
	}
	out := []Audit{}
	for _, doc := range docs {
		row := doc.value
		if row.GrantID == g.credential.ID {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	var oldest *time.Time
	if len(out) > 0 {
		first := out[0].At
		oldest = &first
	}
	return AuditLog{Partial: errors.Is(err, errReadUnavailable), Records: out, OldestRetainedAt: oldest, DroppedCount: s.store.dropped, ReadErrorCount: s.store.readErrors, InvalidSnapshotDeletedCount: s.store.invalidSnapshotDeleted, SkippedCount: s.store.skipped}, nil
}

// RevokeID rejects a guessed grant id rather than revoking a different resource.
func (s *Service) RevokeID(ctx context.Context, token, id string) (RevokeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.authorize(ctx, token)
	if err != nil {
		return RevokeResult{}, err
	}
	if id != g.credential.ID {
		return RevokeResult{}, failure("APP_SNAPSHOT_NOT_FOUND", 404)
	}
	return s.revokeResult(g)
}

// RevokeRuntime is called only by the authenticated main channel after native consent is withdrawn.
func (s *Service) RevokeRuntime(_ context.Context, epoch, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.grants[id]
	if !ok || g.epoch != epoch {
		return failure("APP_DIAGNOSTICS_CAPABILITY_INVALID", 403)
	}
	return s.revoke(g)
}

// GrantRuntime exposes only the opaque grant id to the authenticated main channel.
// The raw credential is published in owner-only daemon storage.
func (s *Service) GrantRuntime(ctx context.Context, id domain.SessionID, epoch string) (string, error) {
	credential, err := s.issueGrant(ctx, id, epoch)
	return credential.ID, err
}

// AuditRuntime serves the current native-consent owner without exporting a token.
// The Settings projection is deliberately smaller than the CLI audit envelope.
func (s *Service) AuditRuntime(ctx context.Context, epoch, id string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.grants[id]
	if s.closed || !ok || g.epoch != epoch {
		return nil, failure("APP_DIAGNOSTICS_CAPABILITY_INVALID", 403)
	}
	if _, err := s.authorizeGrant(ctx, g); err != nil {
		return nil, err
	}
	audit := Audit{RequestID: uuid.NewString(), GrantID: g.credential.ID, SessionID: g.credential.SessionID, At: s.now(), Outcome: "audit_read"}
	log, err := s.auditsForGrant(g, audit)
	if err != nil {
		return nil, err
	}
	type item struct {
		At      time.Time `json:"at"`
		Outcome string    `json:"outcome"`
		Bytes   int       `json:"bytes"`
		Partial bool      `json:"partial"`
	}
	records := []item{}
	dropped := 0
	if len(log.Records) > 100 {
		dropped = len(log.Records) - 100
		log.Records = log.Records[dropped:]
	}
	for _, row := range log.Records {
		outcome := row.Outcome
		switch outcome {
		case "granted", "accepted", "completed", "failed", "interrupted", "revoked", "read", "list", "audit_read":
		default:
			outcome = "unknown"
		}
		partial := row.Partial != nil && *row.Partial
		records = append(records, item{row.At.UTC(), outcome, row.Bytes, partial})
	}
	//nolint:gosec // dropped is a non-negative count bounded by the 10,000-record audit cap.
	data, err := json.Marshal(struct {
		Records      []item `json:"records"`
		Partial      bool   `json:"partial"`
		DroppedCount uint64 `json:"droppedCount"`
	}{records, log.Partial || dropped > 0, log.DroppedCount + uint64(dropped)})
	if err != nil {
		return nil, err
	}
	if err := s.authorizeDelivery(ctx, g); err != nil {
		return nil, s.failDelivery(audit, err)
	}
	return data, nil
}
