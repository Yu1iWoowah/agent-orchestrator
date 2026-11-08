package project

import (
	"context"

	engine "github.com/aoagents/agent-orchestrator/backend/internal/devimport"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	scmobserve "github.com/aoagents/agent-orchestrator/backend/internal/observe/scm"
)

// ImportTarget keeps live registry replacements in the same commit-to-policy
// notification order as normal project settings and registration.
func (m *Service) ImportTarget(store engine.Store) engine.Store {
	return reportingImportTarget{Store: store, projects: m}
}

type reportingImportTarget struct {
	engine.Store
	projects *Service
}

func (s reportingImportTarget) ImportWorkspaceProject(ctx context.Context, row domain.ProjectRecord, repos []domain.WorkspaceRepoRecord) error {
	return s.projects.commitProjectRegistration(row, func() error {
		return s.Store.ImportWorkspaceProject(ctx, row, repos)
	})
}

// SCMStore accepts only the observer's origin backfill. Its old whole-project
// snapshot must never restore stale policy after a settings/import commit.
func (m *Service) SCMStore(store scmobserve.Store) scmobserve.Store {
	return reportingSCMStore{Store: store, projects: m}
}

type reportingSCMStore struct {
	scmobserve.Store
	projects *Service
}

func (s reportingSCMStore) UpsertProject(ctx context.Context, backfill domain.ProjectRecord) error {
	m := s.projects
	m.configMu.Lock()
	defer m.configMu.Unlock()
	current, exists, err := m.store.GetProject(ctx, backfill.ID)
	if err != nil || !exists {
		return err
	}
	if current.RepoOriginURL != "" || backfill.RepoOriginURL == "" {
		return nil
	}
	current.RepoOriginURL = backfill.RepoOriginURL
	return m.store.UpsertProject(ctx, current)
}
