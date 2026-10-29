package store_test

import (
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestSettingsGrantLaunchIdentityFollowsOwnerWrites(t *testing.T) {
	s := newTestStore(t)
	seedProject(t, s, "grant-owner")
	rec := sampleRecord("grant-owner")
	rec.Harness, rec.Mode = domain.HarnessCodex, domain.SessionModeChat
	rec.Metadata.ControllerGeneration = "initial"
	rec, err := s.CreateSession(t.Context(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimLiveChatControllerGeneration(t.Context(), rec.ID, "reconnect"); err != nil {
		t.Fatal(err)
	}
	after, _, err := s.GetSession(t.Context(), rec.ID)
	if err != nil || after.Metadata.AgentSettingsLaunchID != "initial" {
		t.Fatalf("live claim lost the original native launch: %+v %v", after.Metadata, err)
	}
	// General lifecycle writes retain the native identity until they publish
	// a different controller generation for a new provider process.
	after.Metadata.LatestUserPrompt = "activity update"
	if err := s.UpdateSession(t.Context(), after); err != nil {
		t.Fatal(err)
	}
	after, _, _ = s.GetSession(t.Context(), rec.ID)
	if after.Metadata.AgentSettingsLaunchID != "initial" {
		t.Fatal("ordinary metadata update rotated a live grant")
	}
	after.Metadata.ControllerGeneration = "replacement"
	if err := s.UpdateSession(t.Context(), after); err != nil {
		t.Fatal(err)
	}
	after, _, err = s.GetSession(t.Context(), rec.ID)
	if err != nil || after.Metadata.AgentSettingsLaunchID != "replacement" {
		t.Fatalf("new native owner retained the old grant identity: %+v %v", after.Metadata, err)
	}
	changed, err := s.CommitSessionControllerEpoch(t.Context(), rec.ID, domain.SessionModeChat, domain.SessionModeTUI, "native", time.Now())
	if err != nil || !changed {
		t.Fatal("interface transition failed", err)
	}
	after, _, err = s.GetSession(t.Context(), rec.ID)
	if err != nil || after.Metadata.AgentSettingsLaunchID != "" {
		t.Fatalf("retired Chat owner kept its private launch identity: %+v %v", after.Metadata, err)
	}
}
