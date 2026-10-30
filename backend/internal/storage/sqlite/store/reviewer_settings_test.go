package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestReviewerSettingsAreIsolatedAndActivePassCannotChange(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	worker := seedUsageSession(t, s, domain.HarnessCodex)
	at := time.Now().UTC()
	review := domain.Review{ID: "settings-review", SessionID: worker.ID, ProjectID: worker.ProjectID, Harness: domain.ReviewerCodex, AgentSessionID: "review-native", ReviewerLaunchID: "review-launch", CreatedAt: at, UpdatedAt: at}
	mustNoError(t, s.UpsertReview(ctx, review))
	binding := domain.ResolvedAgentAccount{AccountID: "review-account", Provider: domain.AgentAccountProviderCodex, Source: domain.AgentAccountSourceRole}
	mustNoError(t, s.BindReviewerAgentAccount(ctx, review.ID, binding, at))
	settings, ok := any(s).(ports.ReviewerAgentSettingsStore)
	if !ok {
		t.Fatal("reviewer-owned settings are unavailable")
	}
	source := domain.AgentSettings{Model: "review-model", Effort: "low", FastMode: domain.AgentFastOff}
	state, err := settings.InitializeReviewerAgentSettings(ctx, review.ID, source, at)
	mustNoError(t, err)
	before, found, err := s.GetSession(ctx, worker.ID)
	mustNoError(t, err)
	if !found || state.Settings == nil || *state.Settings != source || state.Revision != 0 {
		t.Fatalf("initial reviewer state = %+v", state)
	}
	op := domain.ReviewerAgentSettingsOperation{ID: "review-settings-1", Owner: state.Owner, BaseRevision: 0, Source: &source, Target: domain.AgentSettings{Model: "next-review-model", Effort: "high", FastMode: domain.AgentFastOn}, Actor: domain.AgentSettingsActor{Kind: domain.AgentSettingsActorLocal}, RequestID: "request-1", IdempotencyKey: "key-1", RequestFingerprint: "intent-1", CreatedAt: at}
	applied, err := settings.CommitReviewerAgentSettings(ctx, op)
	mustNoError(t, err)
	if applied.Revision != 1 || applied.Target != op.Target {
		t.Fatalf("pinned operation = %+v", applied)
	}
	after, _, err := s.GetSession(ctx, worker.ID)
	mustNoError(t, err)
	if before.Metadata.Model != after.Metadata.Model || before.Metadata.Effort != after.Metadata.Effort || before.ReviewerConfig != after.ReviewerConfig {
		t.Fatal("reviewer selection wrote worker settings or reviewer defaults")
	}
	bound, found, err := s.GetReviewerAgentAccount(ctx, review.ID, binding.Provider)
	mustNoError(t, err)
	if !found || bound != binding {
		t.Fatalf("reviewer account changed: %+v", bound)
	}
	retried, err := settings.CommitReviewerAgentSettings(ctx, op)
	mustNoError(t, err)
	if retried.ID != applied.ID || retried.Revision != applied.Revision {
		t.Fatalf("retry made a second change: %+v", retried)
	}
	run := domain.ReviewRun{ID: "settings-pass", ReviewID: review.ID, SessionID: worker.ID, Harness: review.Harness, PRURL: "https://github.com/fake/repo/pull/1", TargetSHA: "head-1", Status: domain.ReviewRunRunning, CreatedAt: at}
	mustNoError(t, s.InsertReviewRun(ctx, run))
	pinned, found, err := s.GetReviewRun(ctx, run.ID)
	mustNoError(t, err)
	if !found || pinned.AgentSettings == nil || *pinned.AgentSettings != applied.Target || pinned.AgentSettingsRevision == nil || *pinned.AgentSettingsRevision != 1 {
		t.Fatalf("pass did not pin reviewer settings: %+v", pinned)
	}
	op.ID, op.IdempotencyKey, op.RequestFingerprint = "review-settings-2", "key-2", "intent-2"
	op.BaseRevision, op.Source, op.Target = 1, &applied.Target, source
	if _, err := settings.CommitReviewerAgentSettings(ctx, op); !errors.Is(err, ports.ErrAgentSettingsConflict) {
		t.Fatalf("active pass mutation = %v, want conflict", err)
	}
	current, err := settings.ReviewerAgentSettings(ctx, review.ID)
	mustNoError(t, err)
	if current.Revision != 1 || current.Settings == nil || *current.Settings != applied.Target {
		t.Fatalf("active pass changed the pinned preference: %+v", current)
	}
}

func TestReviewerSettingsRechecksManagedAuthorityAtCommit(t *testing.T) {
	for _, scenario := range []string{"accepted", "replaced actor", "worker actor", "expired", "revoked policy", "worker policy only", "ambiguous actor", "owner changed", "audit failure"} {
		t.Run(scenario, func(t *testing.T) {
			s, db, worker := settingsFixture(t)
			ctx, at := context.Background(), time.Now().UTC()
			actor := sampleRecord("settings")
			actor.ID, actor.Kind, actor.Mode, actor.Harness = "settings-actor", domain.KindOrchestrator, domain.SessionModeChat, domain.HarnessCodex
			actor.Metadata.ControllerGeneration = "actor-generation"
			actor, err := s.CreateSession(ctx, actor)
			mustNoError(t, err)
			review := domain.Review{ID: "managed-review", SessionID: worker.ID, ProjectID: worker.ProjectID, Harness: domain.ReviewerCodex, ReviewerLaunchID: "launch", AgentSessionID: "native", CreatedAt: at, UpdatedAt: at}
			mustNoError(t, s.UpsertReview(ctx, review))
			mustNoError(t, s.BindReviewerAgentAccount(ctx, review.ID, domain.ResolvedAgentAccount{AccountID: "managed-review-account", Provider: domain.AgentAccountProviderCodex, Source: domain.AgentAccountSourceRole}, at))
			source := domain.AgentSettings{Model: "old", Effort: "low", FastMode: domain.AgentFastOff}
			target := domain.AgentSettings{Model: "new", Effort: "high", FastMode: domain.AgentFastOn}
			state, err := s.InitializeReviewerAgentSettings(ctx, review.ID, source, at)
			mustNoError(t, err)
			allow := domain.AgentSettingsAllowance{Models: []domain.AgentModelSelection{"new"}, Efforts: []domain.AgentEffortSelection{"high"}, AllowFast: true}
			policy := &domain.AgentSettingsPolicy{Enabled: true, Reviewers: map[domain.ReviewerHarness]domain.AgentSettingsAllowance{domain.ReviewerCodex: allow}}
			raw, err := json.Marshal(policy)
			mustNoError(t, err)
			digest := sha256.Sum256(raw)
			op := domain.ReviewerAgentSettingsOperation{ID: "managed-op", Owner: state.Owner, Source: &source, Target: target, Actor: domain.AgentSettingsActor{Kind: domain.AgentSettingsActorOrchestrator, SessionID: actor.ID}, Authority: domain.AgentSettingsAuthority{ProjectID: worker.ProjectID, ActorHarness: actor.Harness, ActorMode: actor.Mode, ActorGeneration: "actor-generation", IssuedAt: at.Add(-time.Minute).Unix(), ExpiresAt: at.Add(time.Hour).Unix()}, PolicyRevision: hex.EncodeToString(digest[:]), IdempotencyKey: "managed-key", RequestFingerprint: "managed-intent", CreatedAt: at}
			switch scenario {
			case "replaced actor":
				_, err = db.Exec("UPDATE sessions SET controller_generation='replacement' WHERE id=?", actor.ID)
			case "worker actor":
				_, err = db.Exec("UPDATE sessions SET kind='worker' WHERE id=?", actor.ID)
			case "expired":
				op.Authority.ExpiresAt = at.Add(-time.Second).Unix()
			case "revoked policy":
				policy.Enabled = false
			case "worker policy only":
				policy.Reviewers, policy.Harnesses = nil, map[domain.AgentHarness]domain.AgentSettingsAllowance{domain.HarnessCodex: allow}
			case "ambiguous actor":
				other := actor
				other.ID = "another-orchestrator"
				_, err = s.CreateSession(ctx, other)
			case "owner changed":
				_, err = db.Exec("UPDATE review SET reviewer_launch_id='replacement' WHERE id=?", review.ID)
			case "audit failure":
				_, err = db.Exec("CREATE TRIGGER fail_reviewer_audit BEFORE INSERT ON reviewer_agent_settings_operations BEGIN SELECT RAISE(ABORT,'injected'); END")
			}
			mustNoError(t, err)
			_, err = s.UpdateProjectSettings(ctx, "settings", "settings", domain.ProjectConfig{AgentSettingsPolicy: policy})
			mustNoError(t, err)
			_, err = s.CommitReviewerAgentSettings(ctx, op)
			current, readErr := s.ReviewerAgentSettings(ctx, review.ID)
			mustNoError(t, readErr)
			if scenario == "accepted" {
				if err != nil || current.Revision != 1 || current.Settings == nil || *current.Settings != target {
					t.Fatalf("accepted commit = %+v, %v", current, err)
				}
			} else if err == nil || current.Revision != 0 || current.Settings == nil || *current.Settings != source {
				t.Fatalf("rejected commit changed preference: %+v, %v", current, err)
			}
		})
	}
}
