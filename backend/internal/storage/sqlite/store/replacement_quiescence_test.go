package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	fixturestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

func TestReplacementWakeFenceCannotBeReleasedByUnverifiedMarker(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	op, _, err := st.CreateOrchestratorReplacement(ctx, replacementOperation(t, st))
	if err != nil {
		t.Fatal(err)
	}
	assertFenced := func(id domain.SessionID, want bool) {
		t.Helper()
		got, err := st.ReplacementWakeFenced(ctx, id)
		if err != nil || got != want {
			t.Fatalf("wake fenced %s=%t want=%t err=%v", id, got, want, err)
		}
	}
	assertFenced(op.SourceSessionID, true)
	worker, err := st.CreateSession(ctx, domain.SessionRecord{ProjectID: op.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessCodex, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	assertFenced(worker.ID, false)
	op.Phase = domain.ReplacementNotifying
	op = fixturestore.SeedReplacementTestFacts(t, st, op)
	assertFenced(op.SourceSessionID, true)
	op, _, err = st.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	op.IntakeReleasedAt = &now
	if ok, err := st.SaveOrchestratorReplacement(ctx, op); err == nil || ok {
		t.Fatalf("unverified release marker accepted: saved=%t err=%v", ok, err)
	}
	assertFenced(op.SourceSessionID, true)
}

func TestReplacementTUIReleaseIntentChecksOperationAndSessionAtomically(t *testing.T) {
	for _, which := range []string{"current", "operation revision", "session revision", "capability", "busy", "missing intent", "wrong native launch"} {
		t.Run(which, func(t *testing.T) {
			ctx := context.Background()
			st := newTestStore(t)
			seedProject(t, st, "replace")
			rec := sampleRecord("replace")
			rec.Kind, rec.Mode, rec.Activity.State = domain.KindOrchestrator, domain.SessionModeTUI, domain.ActivityIdle
			rec.Metadata.RuntimeHandleID, rec.Metadata.RuntimeLaunchID = "source-handle", "source-launch"
			rec.Metadata.AgentSessionID, rec.Metadata.AgentSessionIDLaunchID = "source-native", "source-launch"
			rec.Metadata.BrowserCapabilityVerifier = "source-verifier"
			rec, err := st.CreateSession(ctx, rec)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			op := domain.OrchestratorReplacement{SchemaVersion: 1, ID: "11111111-1111-4111-8111-111111111111", OperationKey: "22222222-2222-4222-8222-222222222222", RequestHash: strings.Repeat("a", 64), ProjectID: rec.ProjectID, SourceSessionID: rec.ID, SourceOwner: rec.ControllerOwner(), SourceCapabilityVerifier: rec.Metadata.BrowserCapabilityVerifier, Phase: domain.ReplacementAccepted, CreatedAt: now, UpdatedAt: now, DeadlineAt: now.Add(time.Hour)}
			op, _, err = st.CreateOrchestratorReplacement(ctx, op)
			if err != nil {
				t.Fatal(err)
			}
			op.Phase, op.FrozenAt, op.SourceStopIntentAt = domain.ReplacementExportingTranscript, &now, &now
			op.Preparation = &domain.ReplacementPreparation{VerifiedAt: &now}
			op.Transcript = &domain.ReplacementTranscript{AttemptID: "33333333-3333-4333-8333-333333333333", AO: domain.TranscriptSourceCutoff{ConversationCutoffs: map[string]int64{}}}
			if which == "missing intent" {
				op.SourceStopIntentAt = nil
			}
			op = fixturestore.SeedReplacementTestFacts(t, st, op)
			op, _, err = st.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			sessionRevision, operationRevision := rec.Revision, op.Revision
			switch which {
			case "operation revision":
				operationRevision--
			case "session revision":
				sessionRevision--
			case "capability":
				rec.Metadata.BrowserCapabilityVerifier = "rotated"
			case "busy":
				rec.Activity.State = domain.ActivityActive
			case "wrong native launch":
				rec.Metadata.AgentSessionIDLaunchID = "other-launch"
			}
			if which == "capability" || which == "busy" || which == "wrong native launch" {
				if err := st.UpdateSession(ctx, rec); err != nil {
					t.Fatal(err)
				}
				rec, _, err = st.GetSession(ctx, rec.ID)
				if err != nil {
					t.Fatal(err)
				}
				sessionRevision = rec.Revision
			}
			ok, err := st.SetReplacementTUIRuntimeReleased(ctx, op.ID, operationRevision, rec.ID, sessionRevision)
			if err != nil || ok != (which == "current") {
				actual, _, _ := st.GetSession(ctx, rec.ID)
				t.Fatalf("%s release=%t err=%v allowed=%t revision=%d/%d state=%s owner=%+v/%+v verifier=%q/%q", which, ok, err, op.AllowsTUIQuiescence(actual), sessionRevision, actual.Revision, actual.Activity.State, op.SourceOwner, actual.ControllerOwner(), op.SourceCapabilityVerifier, actual.Metadata.BrowserCapabilityVerifier)
			}
			current, _, err := st.GetSession(ctx, rec.ID)
			if err != nil {
				t.Fatal(err)
			}
			if (current.Metadata.RuntimeReleasedLaunchID == "source-launch") != ok {
				t.Fatal("release intent differed from reservation result")
			}
		})
	}
}
