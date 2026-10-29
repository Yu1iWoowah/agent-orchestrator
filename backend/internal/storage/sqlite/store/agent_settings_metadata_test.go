package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestAgentSettingsAcceptedAuthorityAndFailureCodeSurviveReplay(t *testing.T) {
	s, db, rec := settingsFixture(t)
	ctx := context.Background()
	op := settingsOperation(rec.ID)
	op.Authority = domain.AgentSettingsAuthority{ProjectID: "settings", ActorHarness: domain.HarnessCodex, ActorMode: domain.SessionModeChat, ActorGeneration: "actor-generation", IssuedAt: op.CreatedAt.Unix(), ExpiresAt: op.CreatedAt.Add(time.Hour).Unix()}
	op.Actor = domain.AgentSettingsActor{Kind: domain.AgentSettingsActorOrchestrator, SessionID: "actor"}
	op.RequestFingerprint = strings.Repeat("a", 64)
	accepted, err := s.BeginAgentSettingsOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := s.AgentSettingsOperationByKey(ctx, rec.ID, op.IdempotencyKey)
	if err != nil || !found || got.Authority != op.Authority || got.RequestFingerprint != op.RequestFingerprint {
		t.Fatalf("lost accepted authority: %+v %v", got, err)
	}
	public, _ := json.Marshal(got)
	if strings.Contains(string(public), "actor-generation") || strings.Contains(string(public), op.RequestFingerprint) {
		t.Fatal("private accepted metadata exposed")
	}
	failed, err := s.FinishAgentSettingsOperationWithCode(ctx, rec.ID, op.ID, accepted.Version, domain.AgentSettingsFailed, domain.AgentSettingsErrorValidation, "AGENT_SETTINGS_POLICY_DENIED", time.Now())
	if err != nil || failed.ErrorCode != "AGENT_SETTINGS_POLICY_DENIED" {
		t.Fatalf("lost failure code: %+v %v", failed, err)
	}
	var code string
	if err := db.QueryRow("SELECT error_code FROM session_agent_settings_audit WHERE operation_id=? AND phase='failed'", op.ID).Scan(&code); err != nil || code != failed.ErrorCode {
		t.Fatalf("audit code %s %v", code, err)
	}
	if _, err := s.FinishAgentSettingsOperationWithCode(ctx, rec.ID, op.ID, accepted.Version, domain.AgentSettingsFailed, domain.AgentSettingsErrorValidation, "provider stderr / token", time.Now()); err == nil {
		t.Fatal("raw provider text accepted as a stable error code")
	}
}
