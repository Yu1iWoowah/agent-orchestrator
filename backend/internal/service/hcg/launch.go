package hcg

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/components/hcg/reporting"
)

type launchItem struct {
	delegationSnapshot reporting.PreparedDelegationLaunch
	delegationValid    bool
	notice             ports.HCGReportingLaunch
	done               chan struct{}
}

// This is an ephemeral queue for one fixed metadata consumer, not a durable
// outbox. Immutable receipts are retained for duplicate/late notifications until
// daemon shutdown; no conversation or usage event is retained here.
type metadataObserver struct {
	mu     sync.Mutex
	items  map[string]*launchItem
	queue  []*launchItem
	wake   chan struct{}
	done   chan struct{}
	cancel context.CancelFunc
}

func (s *Service) startMetadata(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	s.metadata = &metadataObserver{items: map[string]*launchItem{}, wake: make(chan struct{}, 1), done: make(chan struct{}), cancel: cancel}
	if s.delegation == nil {
		close(s.metadata.done)
		return
	}
	if projects, err := s.store.ListProjects(ctx); err == nil {
		for _, p := range projects {
			s.delegation.ProjectPolicyChanged(p.ID, delegationPolicy(p.Config.DelegationPolicySnapshot()))
		}
	}
	go s.runMetadata(ctx)
}

// Close stops only this service's metadata work, never a native process.
func (s *Service) Close() {
	if s.delegation != nil {
		s.delegation.Close()
	}
	if s.metadata != nil {
		s.metadata.cancel()
		<-s.metadata.done
	}
}

func (s *Service) validNotice(n ports.HCGReportingLaunch) bool {
	if n.DataRoot != s.root || n.Owner.Kind != domain.ConversationOwnerSession || n.Owner.ID != string(n.SessionID) || (n.Kind != domain.KindWorker && n.Kind != domain.KindOrchestrator) || n.CommittedOwner.IsTerminated || n.CommittedOwner.Harness != n.Harness || n.CommittedOwner.Mode != n.Mode {
		return false
	}
	switch n.Origin {
	case ports.HCGReportingMainChat:
		return n.Mode == domain.SessionModeChat && n.CommittedOwner.ControllerGeneration != ""
	case ports.HCGReportingMainTUI:
		return n.Mode == domain.SessionModeTUI && n.CommittedOwner.RuntimeLaunchID != "" && n.Fresh
	default:
		return false
	}
}

func noticeIdentity(n ports.HCGReportingLaunch) reporting.Identity {
	id := reporting.Identity{DataRoot: n.DataRoot, ProjectID: string(n.ProjectID), SessionID: string(n.SessionID), Role: string(n.Kind), Mode: string(n.Mode), Harness: string(n.Harness), Worktree: n.WorkspacePath, ArtifactDir: n.ArtifactDir, ControllerGeneration: n.CommittedOwner.ControllerGeneration}
	if n.Mode == domain.SessionModeChat {
		id.NativeID = n.CommittedOwner.ProviderConversationID
		if n.Fresh {
			id.LaunchID = n.CommittedOwner.ControllerGeneration
		}
	} else {
		id.LaunchID = n.CommittedOwner.RuntimeLaunchID
		if n.CommittedOwner.AgentSessionIDLaunchID == id.LaunchID {
			id.NativeID = n.CommittedOwner.AgentSessionID
		}
	}
	return id
}

// ReportingLaunchReady is a nonblocking, metadata-only success notification.
// It freezes policy now, never after queue delay, and cannot run the tool.
func (s *Service) ReportingLaunchReady(n ports.HCGReportingLaunch) {
	if !s.validNotice(n) || s.metadata == nil {
		return
	}
	m := s.metadata
	keyBytes, _ := json.Marshal(struct {
		ID     domain.SessionID
		Owner  domain.SessionControllerOwner
		Origin ports.HCGReportingOrigin
	}{n.SessionID, n.CommittedOwner, n.Origin})
	key := string(keyBytes)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, duplicate := m.items[key]; duplicate {
		return
	}
	item := &launchItem{notice: n, done: make(chan struct{})}
	m.items[key] = item
	if s.delegation != nil {
		item.delegationSnapshot, item.delegationValid = s.delegation.CaptureLaunch(noticeIdentity(n), n.Fresh, n.DelegationConfigured)
	}
	if !item.delegationValid {
		close(item.done)
		return
	}
	m.queue = append(m.queue, item)
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (s *Service) runMetadata(ctx context.Context) {
	m := s.metadata
	defer close(m.done)
	defer func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, item := range m.queue {
			close(item.done)
		}
		m.queue = nil
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
			m.mu.Lock()
			batch := m.queue
			m.queue = nil
			m.mu.Unlock()
			for _, item := range batch {
				jobCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				s.storeReady(jobCtx, item)
				cancel()
				close(item.done)
			}
		}
	}
}

func (s *Service) storeReady(ctx context.Context, item *launchItem) {
	n := item.notice
	rec, exists, err := s.store.GetSession(ctx, n.SessionID)
	if err != nil || !exists || rec.IsTerminated {
		return
	}
	current := rec.ControllerOwner()
	if current.Harness != n.CommittedOwner.Harness || current.Mode != n.CommittedOwner.Mode || current.ControllerGeneration != n.CommittedOwner.ControllerGeneration || current.RuntimeLaunchID != n.CommittedOwner.RuntimeLaunchID {
		return
	}
	id := s.identity(rec)
	if rec.ProjectID != "" {
		if _, found, err := s.store.GetProject(ctx, string(rec.ProjectID)); err != nil || !found {
			return
		}
	}
	if id.ProjectID != string(n.ProjectID) || id.Role != string(n.Kind) || id.Mode != string(n.Mode) || id.Harness != string(n.Harness) {
		return
	}
	owner := n.CommittedOwner
	if n.Mode == domain.SessionModeChat {
		if id.ControllerGeneration != owner.ControllerGeneration || (owner.ProviderConversationID != "" && id.NativeID != "" && id.NativeID != owner.ProviderConversationID) {
			return
		}
	} else if id.LaunchID != owner.RuntimeLaunchID {
		return
	}
	if n.WorkspacePath != "" && n.WorkspacePath != id.Worktree || n.ArtifactDir != "" && n.ArtifactDir != id.ArtifactDir {
		return
	}
	if item.delegationValid && s.delegation != nil {
		_ = s.delegation.StoreLaunch(ctx, item.delegationSnapshot, id)
	}
}
