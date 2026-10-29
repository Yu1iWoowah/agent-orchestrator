package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplacementPinnedAOCutoffExcludesLateQueueButDetectsChangedIncludedFacts(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	op := replacementOperation(t, st)
	now := time.Now().UTC()
	conv, err := st.CreateConversation(ctx, "project-conversation", domain.ConversationScopeProject, op.ProjectID, op.SourceSessionID, now)
	if err != nil {
		t.Fatal(err)
	}
	appendMessage := func(id string) {
		t.Helper()
		if fresh, err := st.AppendUserMessage(ctx, conv.ID, op.SourceSessionID, op.SourceOwner.ControllerGeneration, domain.ConversationMessage{ID: id + "-message", ClientMessageID: id + "-client", Text: id, Origin: domain.MessageOriginHuman}, id+"-turn", now); err != nil || !fresh {
			t.Fatal(fresh, err)
		}
	}
	appendMessage("included")
	source, found, err := st.GetSession(ctx, op.SourceSessionID)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	source.Metadata.LatestAssistantUpdate = "verified native answer"
	source.Metadata.ConversationCheckpointState = domain.ConversationCheckpointComplete
	source.Metadata.ConversationCheckpointGeneration = op.SourceOwner.ControllerGeneration
	source.Metadata.ConversationCheckpointNativeID = "native-source"
	source.Metadata.ConversationCheckpointTurnID = "verified-provider-turn"
	if err := st.UpdateSession(ctx, source); err != nil {
		t.Fatal(err)
	}
	op, _, err = st.CreateOrchestratorReplacement(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	cut, err := st.CaptureTranscriptSourceCutoff(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if cut.ConversationCutoffs[conv.ID] <= 0 {
		t.Fatal("capture omitted durable projection boundary")
	}
	op.FrozenAt = &now
	op.Phase = domain.ReplacementExportingTranscript
	op.Transcript = &domain.ReplacementTranscript{AttemptID: "33333333-3333-4333-8333-333333333333", AO: cut}
	if ok, err := st.SaveOrchestratorReplacement(ctx, op); err != nil || !ok {
		t.Fatal(ok, err)
	}
	before, err := st.LoadTranscriptSourceSnapshot(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	appendMessage("late")
	now = now.Add(-time.Hour) // Admission order must not depend on wall-clock time.
	appendMessage("late-clock-rollback")
	after, err := st.LoadTranscriptSourceSnapshot(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Fingerprint != before.Fingerprint || after.CutoffSequence != before.CutoffSequence || len(after.Turns) != 1 || len(after.Messages) != 1 || after.Messages[0].Text != "included" {
		t.Fatal("late intake changed fixed export range", before.Fingerprint, after.Fingerprint)
	}
	if after.Session.Metadata.LatestUserPrompt != source.Metadata.LatestUserPrompt || after.Session.Metadata.LatestAssistantUpdate != source.Metadata.LatestAssistantUpdate {
		t.Fatal("held input replaced text of a verified native checkpoint")
	}
	if after.Session.Metadata.ConversationCheckpointState != domain.ConversationCheckpointComplete || after.Session.Metadata.ConversationCheckpointTurnID != "verified-provider-turn" {
		t.Fatal("held input cleared verified provider checkpoint")
	}
	live, err := st.LoadConversationSnapshot(ctx, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(live.Turns) != 3 || len(live.Messages) != 3 {
		t.Fatal("pinned export dropped canonical late queue")
	}
	if err := st.RecordProviderInput(ctx, "included-turn", "changed included fact"); err != nil {
		t.Fatal(err)
	}
	changed, err := st.LoadTranscriptSourceSnapshot(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Fingerprint == before.Fingerprint {
		t.Fatal("fixed range ignored an included-row mutation")
	}
}

func TestReplacementPinnedEmptyAOCutoffIgnoresNewLateConversation(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	op := replacementOperation(t, st)
	now := time.Now().UTC()
	op, _, err := st.CreateOrchestratorReplacement(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	cut, err := st.CaptureTranscriptSourceCutoff(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	op.FrozenAt = &now
	op.Phase = domain.ReplacementExportingTranscript
	op.Transcript = &domain.ReplacementTranscript{AttemptID: "33333333-3333-4333-8333-333333333333", AO: cut}
	if ok, err := st.SaveOrchestratorReplacement(ctx, op); err != nil || !ok {
		t.Fatal(ok, err)
	}
	before, err := st.LoadTranscriptSourceSnapshot(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := st.CreateConversation(ctx, "late-first-conversation", domain.ConversationScopeProject, op.ProjectID, op.SourceSessionID, now)
	if err != nil {
		t.Fatal(err)
	}
	if fresh, err := st.AppendUserMessage(ctx, conv.ID, op.SourceSessionID, op.SourceOwner.ControllerGeneration, domain.ConversationMessage{ID: "late-message", ClientMessageID: "late-client", Text: "late", Origin: domain.MessageOriginHuman}, "late-turn", now); err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	after, err := st.LoadTranscriptSourceSnapshot(ctx, op.SourceSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Fingerprint != after.Fingerprint || len(after.Messages) != 0 || len(after.Turns) != 0 || len(after.Branches) != 0 {
		t.Fatal("new post-cutoff conversation entered old export scope")
	}
}

func TestReplacementCutoffIsValidatedAndImmutable(t *testing.T) {
	for _, variant := range []string{"missing freeze", "missing map", "negative cutoff", "invalid attempt", "replace cutoff", "remove cutoff"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			st := newTestStore(t)
			op, _, err := st.CreateOrchestratorReplacement(ctx, replacementOperation(t, st))
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			op.FrozenAt = &now
			op.Transcript = &domain.ReplacementTranscript{AttemptID: "33333333-3333-4333-8333-333333333333", AO: domain.TranscriptSourceCutoff{ConversationCutoffs: map[string]int64{"c": 1}}}
			switch variant {
			case "missing freeze":
				op.FrozenAt = nil
			case "missing map":
				op.Transcript.AO.ConversationCutoffs = nil
			case "negative cutoff":
				op.Transcript.AO.ConversationCutoffs["c"] = -1
			case "invalid attempt":
				op.Transcript.AttemptID = "invalid"
			default:
				if ok, err := st.SaveOrchestratorReplacement(ctx, op); err != nil || !ok {
					t.Fatal(ok, err)
				}
				op, _, err = st.GetOrchestratorReplacement(ctx, op.ID)
				if err != nil {
					t.Fatal(err)
				}
				if variant == "remove cutoff" {
					op.Transcript = nil
				} else {
					op.Transcript.AO.ConversationCutoffs["c"] = 2
				}
			}
			if ok, err := st.SaveOrchestratorReplacement(ctx, op); ok || !errors.Is(err, domain.ErrReplacementStateInvalid) {
				t.Fatalf("invalid cutoff saved: %t %v", ok, err)
			}
		})
	}
}

func TestReplacementCapturedEvidenceFactsCannotBeRewritten(t *testing.T) {
	for _, which := range []string{"native cutoff", "native hash", "capture hash", "capture time", "export time", "frozen time", "stop owner", "stop time", "stop intent", "remove stop intent", "receipt hash", "transcript hash", "remove receipt", "remove stop"} {
		t.Run(which, func(t *testing.T) {
			ctx := context.Background()
			st := newTestStore(t)
			op, _, err := st.CreateOrchestratorReplacement(ctx, replacementOperation(t, st))
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			op.FrozenAt = &now
			op.Phase = domain.ReplacementTranscriptVerified
			op.SourceStopIntentAt = &now
			op.SourceStop = &domain.ReplacementControllerStop{Owner: op.SourceOwner, CapabilityVerifier: op.SourceCapabilityVerifier, NativeID: "native-source", Method: "chat-hibernate", ConfirmedAt: now}
			op.Transcript = &domain.ReplacementTranscript{AttemptID: "33333333-3333-4333-8333-333333333333", AO: domain.TranscriptSourceCutoff{ConversationCutoffs: map[string]int64{}}, CapturedAt: &now, ExportedAt: &now, SnapshotFingerprint: strings.Repeat("a", 64), NativeCutoffs: []domain.TranscriptEpochCutoff{{SHA256: strings.Repeat("a", 64), NativeID: "native-source", Schema: "fixture/1", Bytes: 10, LeafID: "leaf"}}, Receipt: &domain.HandoffEvidenceReceipt{Directory: "/tmp/evidence", ManifestHash: strings.Repeat("b", 64), InputHash: strings.Repeat("c", 64)}, TranscriptHash: strings.Repeat("d", 64)}
			if ok, err := st.SaveOrchestratorReplacement(ctx, op); err != nil || !ok {
				t.Fatal(ok, err)
			}
			op, _, err = st.GetOrchestratorReplacement(ctx, op.ID)
			if err != nil {
				t.Fatal(err)
			}
			later := now.Add(time.Second)
			switch which {
			case "native cutoff":
				op.Transcript.NativeCutoffs[0].Bytes++
			case "native hash":
				op.Transcript.NativeCutoffs[0].SHA256 = strings.Repeat("e", 64)
			case "capture hash":
				op.Transcript.SnapshotFingerprint = strings.Repeat("e", 64)
			case "capture time":
				op.Transcript.CapturedAt = &later
			case "export time":
				op.Transcript.ExportedAt = &later
			case "frozen time":
				op.FrozenAt = &later
			case "stop intent":
				op.SourceStopIntentAt = &later
			case "remove stop intent":
				op.SourceStopIntentAt = nil
			case "stop owner":
				op.SourceStop.Owner.ControllerGeneration = "another-generation"
			case "stop time":
				op.SourceStop.ConfirmedAt = later
			case "receipt hash":
				op.Transcript.Receipt.ManifestHash = strings.Repeat("f", 64)
			case "transcript hash":
				op.Transcript.TranscriptHash = strings.Repeat("e", 64)
			case "remove receipt":
				op.Transcript.Receipt = nil
			case "remove stop":
				op.SourceStop = nil
			}
			if ok, err := st.SaveOrchestratorReplacement(ctx, op); ok || !errors.Is(err, domain.ErrReplacementStateInvalid) {
				t.Fatalf("captured %s changed: ok=%t err=%v", which, ok, err)
			}
		})
	}
}
