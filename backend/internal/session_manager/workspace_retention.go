package sessionmanager

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// checkWorkspaceReclaim is shared by every removal entry, including paths that
// first stop a live session. Callers already holding the workspace gate use it
// directly; others use beginWorkspaceReclaim. Only Kill's initial preflight may
// ignore its own handoff so it can settle that handoff without deleting files.
func (m *Manager) checkWorkspaceReclaim(ctx context.Context, rec domain.SessionRecord, rows []ports.WorkspaceRepoInfo, settleOwnHandoff bool) error {
	exceptWriter := domain.SessionID("")
	if settleOwnHandoff {
		exceptWriter = rec.ID
	}
	paths := make([]string, 0, 1+len(rows))
	paths = append(paths, rec.Metadata.WorkspacePath)
	for _, row := range rows {
		paths = append(paths, row.Path)
	}
	for _, path := range paths {
		reason, err := m.workspaceUseReason(ctx, rec.ProjectID, path, rec.ID, exceptWriter)
		if err != nil || reason == unavailableTUIWriterWorkspaceReason {
			return apierr.Conflict("SESSION_WORKSPACE_STATUS_UNAVAILABLE", "Workspace ownership could not be verified; resources are retained", nil)
		}
		if reason != "" {
			code := "SESSION_WORKSPACE_IN_USE"
			if reason == unconfirmedTUIWriterWorkspaceReason {
				code = "AGENT_SETTINGS_WRITER_UNCONFIRMED"
			}
			return apierr.Conflict(code, "Cannot reclaim workspace: "+reason, nil)
		}
	}
	return nil
}

// beginWorkspaceReclaim holds the same gate as spawn/restore until the caller
// finishes removal. Preflight callers release it before provider I/O or scripts
// and acquire it again immediately before touching the workspace.
func (m *Manager) beginWorkspaceReclaim(ctx context.Context, rec domain.SessionRecord, rows []ports.WorkspaceRepoInfo, settleOwnHandoff bool) (func(), error) {
	release := m.acquireWorkspaceGate(rec.ProjectID)
	if err := m.checkWorkspaceReclaim(ctx, rec, rows, settleOwnHandoff); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (m *Manager) preflightWorkspaceReclaim(ctx context.Context, rec domain.SessionRecord, rows []ports.WorkspaceRepoInfo, settleOwnHandoff bool) error {
	release, err := m.beginWorkspaceReclaim(ctx, rec, rows, settleOwnHandoff)
	if err == nil {
		release()
	}
	return err
}

// checkWorkspaceAdmission checks the adapter's actual canonical path before it
// can reattach a preserved worktree. Older adapters without path planning fail
// closed on any writer obligation in the project instead of guessing a path.
func (m *Manager) checkWorkspaceAdmission(ctx context.Context, cfg ports.WorkspaceConfig) error {
	path := cfg.Path
	if resolver, ok := m.workspace.(ports.WorkspacePathResolver); ok {
		resolved, err := resolver.ResolveWorkspacePath(ctx, cfg)
		if err != nil {
			return err
		}
		path = resolved
	}
	recs, err := m.cleanupRecords(ctx, cfg.ProjectID)
	if err != nil {
		return apierr.Conflict("SESSION_WORKSPACE_STATUS_UNAVAILABLE", "Workspace ownership could not be verified; another launch is not safe", nil)
	}
	for _, rec := range recs {
		if path != "" && (rec.Metadata.WorkspacePath == "" || normalizeWorkspacePath(rec.Metadata.WorkspacePath) != normalizeWorkspacePath(path)) {
			continue
		}
		// A same-session handoff owns its active transition, but even that
		// owner cannot bypass an abandoned-writer veto after settlement.
		reason := ""
		if rec.ID == cfg.SessionID {
			abandoned, err := m.abandonedTUISettingsWriter(ctx, rec.ID)
			if err != nil {
				reason = unavailableTUIWriterWorkspaceReason
			} else if abandoned {
				reason = unconfirmedTUIWriterWorkspaceReason
			}
		} else {
			reason = m.workspaceRetentionReason(ctx, rec.ID)
		}
		if reason == unavailableTUIWriterWorkspaceReason {
			return apierr.Conflict("SESSION_WORKSPACE_STATUS_UNAVAILABLE", "Workspace ownership could not be verified; another launch is not safe", nil)
		}
		if reason == unconfirmedTUIWriterWorkspaceReason {
			return apierr.Conflict("AGENT_SETTINGS_WRITER_UNCONFIRMED", "Cannot launch in a retained workspace: "+reason, nil)
		}
		if reason != "" {
			return apierr.Conflict("SESSION_WORKSPACE_IN_USE", "Cannot launch in a retained workspace: "+reason, nil)
		}
	}
	return nil
}

func (m *Manager) restoreAdmittedWorkspace(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	if err := m.checkWorkspaceAdmission(ctx, cfg); err != nil {
		return ports.WorkspaceInfo{}, err
	}
	return m.workspace.Restore(ctx, cfg)
}
