package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	fixturestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

type sourceIntakeReleaser interface {
	ReleaseReplacementSourceIntake(context.Context, string, int64, time.Time) (domain.OrchestratorReplacement, bool, error)
}

func TestReplacementSourceReleasePreservesOrdinaryQueueAndHoldsOldControl(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	op, _, err := st.CreateOrchestratorReplacement(ctx, replacementOperation(t, st))
	if err != nil {
		t.Fatal(err)
	}
	op.Phase = domain.ReplacementAwaitingHandoff
	op.Preparation = &domain.ReplacementPreparation{ControlClientID: "old-control"}
	op = fixturestore.SeedReplacementTestFacts(t, st, op)
	op, _, err = st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := st.CreateConversation(ctx, "source-input", domain.ConversationScopeProject, op.ProjectID, op.SourceSessionID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old-control", "ordinary"} {
		if fresh, err := st.AppendUserMessage(ctx, conv.ID, op.SourceSessionID, op.SourceOwner.ControllerGeneration, domain.ConversationMessage{ID: id, ClientMessageID: id, Text: id, Origin: domain.MessageOriginAutomation}, id, time.Now()); err != nil || !fresh {
			t.Fatal(fresh, err)
		}
	}
	op, _, err = st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	release, ok := any(st).(sourceIntakeReleaser)
	if !ok {
		t.Fatal("safe source intake release unavailable")
	}
	result, applied, err := release.ReleaseReplacementSourceIntake(ctx, op.ID, op.Revision, time.Now())
	if err != nil || !applied {
		t.Fatal(applied, err)
	}
	if fenced, err := st.ReplacementWakeFenced(ctx, op.SourceSessionID); err != nil || fenced {
		t.Fatal("source wake remains fenced", fenced, err)
	}
	batch, err := st.ClaimQueuedBatch(ctx, conv.ID, true, 0, time.Now())
	if err != nil || len(batch) != 1 || batch[0].TurnID != "ordinary" {
		t.Fatal("source release lost queue or replayed old control", batch, err)
	}
	if err := st.MarkTurnDispatching(ctx, "old-control"); !errors.Is(err, domain.ErrReplacementIntakeFenced) {
		t.Fatal("old preparation escaped", err)
	}
	again, applied, err := release.ReleaseReplacementSourceIntake(ctx, result.ID, result.Revision, time.Now())
	if err != nil || !applied || again.Revision != result.Revision {
		t.Fatal("source release replay changed revision", applied, err)
	}
}

func TestReplacementSourceReleaseRefusesChangedOrStoppingOwner(t *testing.T) {
	for _, change := range []string{"current", "stale revision", "generation", "capability", "hibernated", "stopping", "recovery", "active"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			st := newTestStore(t)
			op, _, err := st.CreateOrchestratorReplacement(ctx, replacementOperation(t, st))
			if err != nil {
				t.Fatal(err)
			}
			op.Phase = domain.ReplacementAwaitingSource
			now := time.Now().UTC()
			switch change {
			case "active":
				op.Phase = domain.ReplacementWaitingSourceIdle
			case "recovery":
				op.Phase = domain.ReplacementRecoveryRequired
			case "stopping":
				op.Phase = domain.ReplacementRecoveryRequired
				op.SourceStopIntentAt = &now
				op.FrozenAt = &now
				op.Transcript = &domain.ReplacementTranscript{AttemptID: "33333333-3333-4333-8333-333333333333", AO: domain.TranscriptSourceCutoff{ConversationCutoffs: map[string]int64{}}}
			}
			op = fixturestore.SeedReplacementTestFacts(t, st, op)
			op, _, err = st.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			source, _, err := st.GetSession(ctx, op.SourceSessionID)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "generation":
				source.Metadata.ControllerGeneration = "rotated"
			case "capability":
				source.Metadata.BrowserCapabilityVerifier = "rotated"
			case "hibernated":
				if applied, err := st.SetSessionHibernated(ctx, source.ID, source.Revision, &now); err != nil || !applied {
					t.Fatal(applied, err)
				}
			}
			if err := st.UpdateSession(ctx, source); err != nil {
				t.Fatal(err)
			}
			revision := op.Revision
			if change == "stale revision" {
				revision--
			}
			got, applied, err := sourceReleaseAPI(t, st).ReleaseReplacementSourceIntake(ctx, op.ID, revision, now)
			if err != nil || applied != (change == "current") {
				t.Fatal("unsafe source release", change, applied, err)
			}
			if change != "current" && got.SourceIntakeReleasedAt != nil {
				t.Fatal("refused release acquired receipt")
			}
			if change == "current" {
				source.Metadata.BrowserCapabilityVerifier = "after-release-rotation"
				if err := st.UpdateSession(ctx, source); err != nil {
					t.Fatal(err)
				}
				if fenced, err := st.ReplacementWakeFenced(ctx, source.ID); err != nil || !fenced {
					t.Fatal("rotated source retained input release", fenced, err)
				}
			}
		})
	}
}
func TestReplacementSourceReleaseCannotBeForgedOrCleared(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	op, _, err := st.CreateOrchestratorReplacement(ctx, replacementOperation(t, st))
	if err != nil {
		t.Fatal(err)
	}
	op.Phase = domain.ReplacementAwaitingSource
	if saved, err := st.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
		t.Fatal(saved, err)
	}
	op, _, err = st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	forged := op
	forged.SourceIntakeReleasedAt = &now
	if saved, err := st.SaveOrchestratorReplacement(ctx, forged); err == nil || saved {
		t.Fatal("generic save forged source release")
	}
	op, applied, err := sourceReleaseAPI(t, st).ReleaseReplacementSourceIntake(ctx, op.ID, op.Revision, now)
	if err != nil || !applied {
		t.Fatal(applied, err)
	}
	op.SourceIntakeReleasedAt = nil
	if saved, err := st.SaveOrchestratorReplacement(ctx, op); err == nil || saved {
		t.Fatal("generic save cleared source release")
	}
}

func sourceReleaseAPI(t *testing.T, st any) sourceIntakeReleaser {
	t.Helper()
	api, ok := st.(sourceIntakeReleaser)
	if !ok {
		t.Fatal("safe source intake release unavailable")
	}
	return api
}

func TestReplacementSourceReleaseAndCancelRestoreHumanProjection(t *testing.T) {
	for _, action := range []string{"release", "cancel"} {
		t.Run(action, func(t *testing.T) {
			st := newTestStore(t)
			ctx := context.Background()
			op, _, err := st.CreateOrchestratorReplacement(ctx, replacementOperation(t, st))
			if err != nil {
				t.Fatal(err)
			}
			op.Phase = domain.ReplacementAwaitingSource
			if saved, err := st.SaveOrchestratorReplacement(ctx, op); err != nil || !saved {
				t.Fatal(saved, err)
			}
			op, _, err = st.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			source, _, err := st.GetSession(ctx, op.SourceSessionID)
			if err != nil {
				t.Fatal(err)
			}
			source.Metadata.LatestUserPrompt = "verified source request"
			source.Metadata.LatestUserPromptAt = time.Now().Add(-time.Hour).UTC()
			source.Metadata.ConversationCheckpointState = domain.ConversationCheckpointComplete
			source.Metadata.ConversationCheckpointTurnID = "verified-source-turn"
			if err := st.UpdateSession(ctx, source); err != nil {
				t.Fatal(err)
			}
			conv, err := st.CreateConversation(ctx, "source-human-hold", domain.ConversationScopeProject, op.ProjectID, op.SourceSessionID, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			at := time.Now().Add(time.Second).UTC()
			if _, err := st.AppendUserMessage(ctx, conv.ID, source.ID, source.Metadata.ControllerGeneration, domain.ConversationMessage{ID: "held-human", ClientMessageID: "held-human", Text: "human edited instruction", Origin: domain.MessageOriginAutomation, AuthoredByUser: true, InteractionAt: at}, "held-human-turn", time.Now()); err != nil {
				t.Fatal(err)
			}
			before, _, err := st.GetSession(ctx, source.ID)
			if err != nil || before.Metadata.LatestUserPrompt != source.Metadata.LatestUserPrompt {
				t.Fatal("hold projected too early", before.Metadata, err)
			}
			if action == "release" {
				if _, applied, err := sourceReleaseAPI(t, st).ReleaseReplacementSourceIntake(ctx, op.ID, op.Revision, time.Now()); err != nil || !applied {
					t.Fatal(applied, err)
				}
			} else if _, applied, err := st.ResetReplacementBeforeStop(ctx, op.ID, op.Revision, true, time.Now(), time.Now().Add(time.Hour)); err != nil || !applied {
				t.Fatal(applied, err)
			}
			after, _, err := st.GetSession(ctx, source.ID)
			if err != nil || after.Metadata.LatestUserPrompt != "human edited instruction" || !after.Metadata.LatestUserPromptAt.Equal(at) || after.Metadata.ConversationCheckpointState != domain.ConversationCheckpointLegacy || after.Metadata.ConversationCheckpointTurnID != "" {
				t.Fatal("source projection not restored as unconfirmed", after.Metadata, err)
			}
			turn, err := st.TurnByID(ctx, "held-human-turn")
			if err != nil || turn.State != domain.TurnStateQueued || turn.ProviderTurnID != "" || turn.StartedAt != nil {
				t.Fatal("source release fabricated delivery", turn, err)
			}
		})
	}
}
