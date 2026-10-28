// Package projectdocuments owns project living files and their durable write lease.
package projectdocuments

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sync"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// MissingHash is the creation precondition for a file that does not yet exist.
const MissingHash = "missing"

// MaxFileBytes bounds text reads and managed writes; downloads are streamed.
const MaxFileBytes = 2 << 20
const maxFiles = 4096

// Store persists document ownership using revision-based compare-and-swap.
type Store interface {
	GetProject(context.Context, string) (domain.ProjectRecord, bool, error)
	GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error)
	ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error)
	GetProjectDocumentState(context.Context, domain.ProjectID) (domain.ProjectDocumentState, error)
	SaveProjectDocumentState(context.Context, domain.ProjectID, domain.ProjectDocumentState) (bool, error)
	WithProjectDocumentPublication(context.Context, domain.ProjectID, domain.SessionRecord, int64, func(context.Context) error) (bool, error)
}

// CapabilityValidator authenticates a session against its durable verifier.
type CapabilityValidator interface {
	Valid(domain.SessionID, string, string) bool
}

// Deps provides the daemon-owned store, root, and capability authority.
type Deps struct {
	Store        Store
	DataDir      string
	Capabilities CapabilityValidator
}

// Service coordinates project document writes and lease transitions.
type Service struct {
	store        Store
	dataDir      string
	capabilities CapabilityValidator
	locksMu      sync.Mutex
	locks        map[domain.ProjectID]*sync.Mutex
}

// New constructs the daemon singleton for managed project documents.
func New(d Deps) *Service {
	return &Service{store: d.Store, dataDir: d.DataDir, capabilities: d.Capabilities, locks: make(map[domain.ProjectID]*sync.Mutex)}
}

// Actor proves which orchestrator is requesting a managed write.
type Actor struct {
	SessionID  domain.SessionID `json:"sessionId"`
	Capability string           `json:"capability"`
}

// Grant carries a writer token fenced by its durable epoch.
type Grant struct {
	Epoch int64  `json:"epoch"`
	Token string `json:"token"`
}

// LeaseView exposes ownership without disclosing writer credentials.
type LeaseView struct {
	SessionID domain.SessionID `json:"sessionId"`
	Epoch     int64            `json:"epoch"`
	Frozen    bool             `json:"frozen"`
}

// Info locates shared documents independently of source session lifetime.
type Info struct {
	SharedDir    string                          `json:"sharedDir"`
	DocumentPath string                          `json:"documentPath"`
	Files        []domain.ProjectDocumentEntry   `json:"files"`
	Lease        *LeaseView                      `json:"lease,omitempty"`
	Adoption     *domain.ProjectDocumentAdoption `json:"adoption,omitempty"`
}

// File describes a bounded document; binary bytes use the download endpoint.
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Hash    string `json:"hash"`
	Size    int64  `json:"size"`
	Binary  bool   `json:"binary"`
}

// WriteInput requires both controller authority and the expected file hash.
type WriteInput struct {
	Actor        Actor  `json:"actor"`
	Lease        Grant  `json:"lease"`
	Path         string `json:"path"`
	Content      string `json:"content"`
	ExpectedHash string `json:"expectedHash"`
}

// AdoptInput selects a canonical legacy session under a current writer lease.
type AdoptInput struct {
	Actor           Actor            `json:"actor"`
	Lease           Grant            `json:"lease"`
	SourceSessionID domain.SessionID `json:"sourceSessionId"`
}

func (s *Service) lock(id domain.ProjectID) func() {
	s.locksMu.Lock()
	mu := s.locks[id]
	if mu == nil {
		mu = &sync.Mutex{}
		s.locks[id] = mu
	}
	s.locksMu.Unlock()
	mu.Lock()
	return mu.Unlock
}
func hash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func secret() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func conflict(code, message string) error { return apierr.Conflict(code, message, nil) }
func (s *Service) location(ctx context.Context, id domain.ProjectID) (string, string, error) {
	p, ok, err := s.store.GetProject(ctx, string(id))
	if err != nil {
		return "", "", err
	}
	if !ok || !p.ArchivedAt.IsZero() {
		return "", "", apierr.NotFound("PROJECT_NOT_FOUND", "Project not found")
	}
	cfg := p.Config.OrchestratorHandoff
	if err = cfg.Validate(); err != nil {
		return "", "", apierr.Invalid("PROJECT_DOCUMENT_CONFIG_INVALID", err.Error(), nil)
	}
	shared := cfg.SharedDir
	if shared == "" {
		shared = "shared"
	}
	doc := cfg.DocumentPath
	if doc == "" {
		doc = "handoff/orchestrator-handoff.md"
	}
	if s.dataDir == "" {
		return "", "", apierr.Unavailable("PROJECT_DOCUMENT_DATA_UNAVAILABLE", "AO data directory is unavailable")
	}
	return filepath.Join(s.dataDir, "projects", projectKey(id), "orchestrator", filepath.FromSlash(shared)), doc, nil
}
func (s *Service) state(ctx context.Context, id domain.ProjectID) (domain.ProjectDocumentState, string, string, error) {
	root, doc, err := s.location(ctx, id)
	if err != nil {
		return domain.ProjectDocumentState{}, "", "", err
	}
	state, err := s.store.GetProjectDocumentState(ctx, id)
	if err != nil {
		return state, "", "", err
	}
	if state.SharedDir != "" && state.SharedDir != root {
		return state, "", "", conflict("PROJECT_DOCUMENT_ROOT_CHANGED", "The shared root changed; explicit migration is required")
	}
	state.SharedDir = root
	return state, root, doc, nil
}
func (s *Service) save(ctx context.Context, id domain.ProjectID, state *domain.ProjectDocumentState) error {
	ok, err := s.store.SaveProjectDocumentState(ctx, id, *state)
	if err != nil {
		return err
	}
	if !ok {
		return conflict("PROJECT_DOCUMENT_STATE_CHANGED", "Project document ownership changed; retry with current state")
	}
	state.Revision++
	return nil
}
