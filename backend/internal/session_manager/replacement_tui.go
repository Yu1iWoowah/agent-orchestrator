package sessionmanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/transcriptexport"
)

type replacementTranscriptStore interface {
	LoadTranscriptSourceSnapshot(context.Context, domain.SessionID) (domain.TranscriptSourceSnapshot, error)
}

func (m *Manager) replacementTUIWriteContext(ctx context.Context, op domain.OrchestratorReplacement, original domain.SessionRecord) context.Context {
	exact := m.exactLaunchPreWrite(original.ID, original.Harness, runtimeHandle(original.Metadata), original.Metadata.RuntimeLaunchID, domain.ErrReplacementSourceChanged)
	return context.WithValue(ctx, replacementTUIWriteProofKey{}, func(writeCtx context.Context, current domain.SessionRecord) error {
		if current.ControllerOwner() != op.SourceOwner || current.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
			return domain.ErrReplacementSourceChanged
		}
		_, fresh, release, err := m.beginReplacementSourceAction(writeCtx, op)
		if err != nil {
			return err
		}
		defer release()
		if fresh.Metadata.RuntimeHandleID != original.Metadata.RuntimeHandleID {
			return domain.ErrReplacementSourceChanged
		}
		return exact(writeCtx, current)
	})
}

// Build deliberately removes message payload from its manifest. Retain only
// this observation's current-epoch read for control correlation, without adding
// payload to the durable ledger or reading/parsing the native file again.
type replacementTUIReader struct {
	ports.NativeTranscriptReader
	nativeID string
	segment  ports.NativeTranscriptSegment
}

func (r *replacementTUIReader) ReadNativeTranscript(ctx context.Context, request ports.NativeTranscriptRequest) (ports.NativeTranscriptSegment, error) {
	segment, err := r.NativeTranscriptReader.ReadNativeTranscript(ctx, request)
	if err == nil && segment.NativeID == r.nativeID {
		r.segment = segment
	}
	return segment, err
}

func tuiPreparationAccepted(rec domain.SessionRecord, op domain.OrchestratorReplacement) bool {
	p := op.Preparation
	return p != nil && p.PromptIntentAt != nil && p.PromptHash != "" &&
		rec.Metadata.ConversationCheckpointNativeID == rec.Metadata.AgentSessionID &&
		semanticMessageAccepted(rec, p.ControlClientID)
}

func tuiPendingPreparation(rec domain.SessionRecord, op domain.OrchestratorReplacement) *domain.ConversationTurn {
	if !tuiPreparationAccepted(rec, op) && (op.Preparation == nil || op.Preparation.ControlTurnID == "") {
		return nil
	}
	return &domain.ConversationTurn{ID: op.Preparation.ControlClientID, HandledBySessionID: rec.ID, State: domain.TurnStateRunning}
}

func tuiEvidenceError(code, reason string) error {
	return &ports.NativeTranscriptError{Code: code, Reason: reason}
}

func (m *Manager) observeReplacementTUISource(ctx context.Context, op domain.OrchestratorReplacement, rec domain.SessionRecord) (ports.ReplacementSourceObservation, error) {
	observation := ports.ReplacementSourceObservation{Owner: op.SourceOwner, NativeID: rec.Metadata.AgentSessionID, State: ports.ChatControllerBusy, Provenance: domain.NativeThreadMain, ObservedAt: m.clock(), ControlTurn: tuiPendingPreparation(rec, op)}
	if rec.Metadata.RuntimeLaunchID == "" || rec.Metadata.AgentSessionID == "" || rec.Metadata.AgentSessionIDLaunchID != rec.Metadata.RuntimeLaunchID {
		return observation, tuiEvidenceError("ORCHESTRATOR_HANDOFF_SOURCE_UNVERIFIED", "TUI native identity is not bound to its current launch")
	}
	ref := ports.FencedRuntimeRef{Handle: runtimeHandle(rec.Metadata), SessionID: rec.ID, Generation: rec.Metadata.RuntimeLaunchID, NativeIdentity: rec.Metadata.AgentSessionID}
	if probe := m.runtime.ProbeFencedRuntime(ctx, ref); probe.Liveness != ports.FencedAlive {
		return observation, tuiEvidenceError("ORCHESTRATOR_HANDOFF_SOURCE_UNVERIFIED", "Exact TUI source launch is not positively alive")
	}
	observation.PendingApproval = rec.Activity.State == domain.ActivityBlocked || rec.Activity.State == domain.ActivityWaitingInput
	if rec.Activity.State != domain.ActivityIdle {
		return observation, nil
	}
	inspector, ok := m.runtime.(ports.WorkloadCommandInspector)
	if !ok {
		return observation, tuiEvidenceError("ORCHESTRATOR_HANDOFF_SOURCE_UNVERIFIED", "TUI workload inventory is unavailable")
	}
	commands, err := inspector.RunningWorkloadCommands(ctx, ref.Handle, ports.SupervisedProcessRef{SessionID: rec.ID, LaunchID: ref.Generation})
	if err != nil {
		return observation, err
	}
	if commands != 0 {
		return observation, nil
	}
	store, ok := m.store.(replacementTranscriptStore)
	if !ok {
		return observation, tuiEvidenceError("TRANSCRIPT_SOURCE_INVALID", "Owned transcript inventory is unavailable")
	}
	agent, found := m.agents.Agent(rec.Harness)
	if !found {
		return observation, ErrUnknownHarness
	}
	reader, ok := agent.(ports.NativeTranscriptReader)
	if !ok {
		return observation, tuiEvidenceError("TRANSCRIPT_CAPTURE_UNAVAILABLE", "Claude native reader is unavailable")
	}
	snapshot, err := store.LoadTranscriptSourceSnapshot(ctx, rec.ID)
	if err != nil {
		return observation, err
	}
	if snapshot.Session.ID != rec.ID || snapshot.Session.ControllerOwner() != op.SourceOwner || snapshot.Session.Metadata.BrowserCapabilityVerifier != op.SourceCapabilityVerifier {
		return observation, domain.ErrReplacementSourceChanged
	}
	cuts, err := transcriptexport.CaptureBoundaries(ctx, snapshot, reader)
	if err != nil {
		return observation, err
	}
	observedReader := &replacementTUIReader{NativeTranscriptReader: reader, nativeID: rec.Metadata.AgentSessionID}
	verified, err := transcriptexport.Build(ctx, snapshot, cuts, observedReader)
	if err != nil {
		return observation, err
	}
	witness, err := verified.RevalidateForPublication(ctx)
	if err != nil {
		return observation, err
	}
	_, fresh, release, err := m.beginReplacementSourceAction(ctx, op)
	if err != nil {
		return observation, err
	}
	defer release()
	before, after := snapshot.Session.Metadata, fresh.Metadata
	if fresh.Activity.State != domain.ActivityIdle ||
		!fresh.Activity.LastActivityAt.Equal(snapshot.Session.Activity.LastActivityAt) ||
		after.ConversationCheckpointState != before.ConversationCheckpointState ||
		after.ConversationCheckpointGeneration != before.ConversationCheckpointGeneration ||
		after.ConversationCheckpointNativeID != before.ConversationCheckpointNativeID ||
		after.ConversationCheckpointTurnID != before.ConversationCheckpointTurnID ||
		after.NativeCheckpointEvidence != before.NativeCheckpointEvidence ||
		after.ConversationCheckpointUnsettled != before.ConversationCheckpointUnsettled ||
		after.LatestUserPrompt != before.LatestUserPrompt || !after.LatestUserPromptAt.Equal(before.LatestUserPromptAt) ||
		after.LatestAssistantUpdate != before.LatestAssistantUpdate || !after.LatestAssistantUpdateAt.Equal(before.LatestAssistantUpdateAt) {
		return observation, nil
	}
	if err := witness.Check(ctx); err != nil {
		return observation, err
	}
	control, err := verifiedTUIControl(rec, op, []ports.NativeTranscriptSegment{observedReader.segment})
	if err != nil {
		return observation, err
	}
	observation.ControlTurn = control
	observation.State, observation.ObservedAt = ports.ChatControllerReady, m.clock()
	if control != nil && !control.State.Terminal() {
		observation.State = ports.ChatControllerBusy
	}
	return observation, nil
}

func verifiedTUIControl(rec domain.SessionRecord, op domain.OrchestratorReplacement, segments []ports.NativeTranscriptSegment) (*domain.ConversationTurn, error) {
	p := op.Preparation
	if p == nil || p.PromptIntentAt == nil {
		return nil, nil
	}
	var matches []ports.NativeTranscriptRecord
	latestUser, tail := "", ""
	for _, segment := range segments {
		if segment.NativeID != rec.Metadata.AgentSessionID {
			continue
		}
		tail = segment.TailState
		records := map[string]ports.NativeTranscriptRecord{}
		for _, record := range segment.Records {
			records[record.ID] = record
		}
		for _, id := range segment.MainRecordIDs {
			r := records[id]
			if r.Type != "user" || r.Meta || r.Companion || r.State == "interrupted" || r.Excluded != "" || r.Provenance != domain.NativeThreadMain {
				continue
			}
			var text []string
			for _, block := range r.Blocks {
				if block.Kind == ports.TranscriptText {
					text = append(text, block.Text)
				}
			}
			body := strings.TrimSpace(strings.Join(text, "\n"))
			if body == "" {
				continue
			}
			latestUser = r.ID
			if key, ok := domain.ReportDeliveryID(body); !ok || key != p.ControlClientID {
				continue
			}
			prefix, suffix := fmt.Sprintf("<ao-report-delivery id=%q>\n", p.ControlClientID), "\n</ao-report-delivery>"
			if !strings.HasPrefix(body, prefix) || !strings.HasSuffix(body, suffix) {
				return nil, tuiEvidenceError("ORCHESTRATOR_HANDOFF_TURN_MISMATCH", "Native control envelope is invalid")
			}
			prompt := strings.TrimSuffix(strings.TrimPrefix(body, prefix), suffix)
			hash := sha256.Sum256([]byte(prompt))
			if hex.EncodeToString(hash[:]) != p.PromptHash {
				return nil, tuiEvidenceError("ORCHESTRATOR_HANDOFF_TURN_MISMATCH", "Native preparation does not match its persisted prompt hash")
			}
			matches = append(matches, r)
		}
	}
	if len(matches) == 0 {
		return tuiPendingPreparation(rec, op), nil
	}
	if len(matches) != 1 || latestUser != matches[0].ID {
		return nil, tuiEvidenceError("ORCHESTRATOR_HANDOFF_INTAKE_CHANGED", "New or ambiguous native instructions require preparation again")
	}
	state := domain.TurnStateCompleted
	if tail != "complete" {
		state = domain.TurnStateInterrupted
	}
	return &domain.ConversationTurn{ID: p.ControlClientID, HandledBySessionID: rec.ID, ProviderTurnID: matches[0].PromptID, State: state}, nil
}

func (m *Manager) ensureReplacementTUIPreparation(ctx context.Context, op domain.OrchestratorReplacement, rec domain.SessionRecord, prompt string, allowNew bool) (ports.ReplacementPreparationDelivery, error) {
	delivery := ports.ReplacementPreparationDelivery{Owner: op.SourceOwner, NativeID: rec.Metadata.AgentSessionID}
	p := op.Preparation
	hash := sha256.Sum256([]byte(prompt))
	if p == nil || p.PromptIntentAt == nil || p.PromptHash != hex.EncodeToString(hash[:]) {
		return delivery, domain.ErrReplacementStateInvalid
	}
	if key, ok := domain.ReportDeliveryID(domain.WrapReportDelivery(p.ControlClientID, prompt)); !ok || key != p.ControlClientID {
		return delivery, domain.ErrReplacementStateInvalid
	}
	if tuiPreparationAccepted(rec, op) {
		delivery.TurnID = p.ControlClientID
		return delivery, nil
	}
	if !allowNew || p.ControlTurnID != "" {
		delivery.Uncertain = true
		return delivery, nil
	}
	observation, err := m.observeReplacementTUISource(ctx, op, rec)
	if err != nil {
		return delivery, err
	}
	if observation.ControlTurn != nil {
		delivery.TurnID = observation.ControlTurn.ID
		return delivery, nil
	}
	if observation.State != ports.ChatControllerReady || observation.PendingApproval {
		return delivery, domain.ErrReplacementSourceBusy
	}
	ctx = m.replacementTUIWriteContext(ctx, op, rec)
	if err := m.SendSemantic(ctx, rec.ID, prompt, p.ControlClientID); err != nil {
		delivery.Uncertain = true
		return delivery, err
	}
	_, current, release, err := m.beginReplacementSourceAction(ctx, op)
	if err != nil {
		delivery.Uncertain = true
		return delivery, err
	}
	defer release()
	if !tuiPreparationAccepted(current, op) {
		delivery.Uncertain = true
		return delivery, nil
	}
	delivery.TurnID = p.ControlClientID
	return delivery, nil
}
