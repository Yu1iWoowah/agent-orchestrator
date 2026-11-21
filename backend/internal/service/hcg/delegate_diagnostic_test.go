package hcg

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestDelegationLaunchRequiresCommittedNativeIdentity(t *testing.T) {
	store, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	root, worktree := t.TempDir(), t.TempDir()
	on := true
	project := domain.ProjectRecord{ID: "p", Path: worktree, Config: domain.ProjectConfig{HCGDelegation: domain.HCGDelegationConfig{Enabled: &on}}}
	if err := store.UpsertProject(t.Context(), project); err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateSession(t.Context(), domain.SessionRecord{ProjectID: "p", Kind: domain.KindWorker, Mode: domain.SessionModeChat, Harness: domain.HarnessCodex, Metadata: domain.SessionMetadata{WorkspacePath: worktree, ControllerGeneration: "synthetic-generation", AgentSettingsLaunchID: "synthetic-generation", ProviderConversationID: "synthetic-native"}})
	if err != nil {
		t.Fatal(err)
	}
	durable, ok, err := store.GetSession(t.Context(), created.ID)
	if err != nil || !ok {
		t.Fatalf("reload: %v %v", ok, err)
	}
	t.Logf("owner_equal=%v launch_created=%q launch_durable=%q workspace_equal=%v", created.ControllerOwner() == durable.ControllerOwner(), created.Metadata.AgentSettingsLaunchID, durable.Metadata.AgentSettingsLaunchID, created.Metadata.WorkspacePath == durable.Metadata.WorkspacePath)
	svc := New(t.Context(), root, store)
	defer svc.Close()
	notice := ports.HCGReportingLaunch{DelegationConfigured: true, Origin: ports.HCGReportingMainChat, Owner: domain.SessionConversationOwner(created.ID), SessionID: created.ID, ProjectID: created.ProjectID, Kind: created.Kind, Harness: created.Harness, Mode: created.Mode, DataRoot: root, WorkspacePath: worktree, CommittedOwner: created.ControllerOwner(), Fresh: true}
	valid := svc.validNotice(notice)
	captured, accepted := svc.delegation.CaptureLaunch(noticeIdentity(notice), true, true)
	t.Logf("validNotice=%v capture=%v captured_launch=%q storeReady_facts_launch=%q", valid, accepted, noticeIdentity(notice).LaunchID, svc.identity(durable).LaunchID)
	if !valid || !accepted {
		t.Fatal("rejection is before StoreLaunch identity check")
	}
	err = svc.delegation.StoreLaunch(t.Context(), captured, svc.identity(durable))
	t.Logf("StoreLaunch_identity_rejected=%v", err != nil)
	if err == nil || durable.Metadata.AgentSettingsLaunchID != "" {
		t.Fatal("diagnostic hypothesis disproven")
	}
	if err := store.ClaimChatControllerGeneration(t.Context(), created.ID, created.Metadata.ControllerGeneration); err != nil {
		t.Fatal(err)
	}
	durable, _, err = store.GetSession(t.Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if durable.Metadata.AgentSettingsLaunchID != created.Metadata.ControllerGeneration {
		t.Fatal("committed identity absent")
	}
	if err := svc.delegation.StoreLaunch(t.Context(), captured, svc.identity(durable)); err != nil {
		t.Fatalf("formal success boundary rejected: %v", err)
	}
}
