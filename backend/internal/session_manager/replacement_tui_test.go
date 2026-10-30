package sessionmanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/claudecode"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/sessionguard"
)

type replacementTUIStore struct {
	*replacementFenceStore
	configDir string
}

func (s *replacementTUIStore) LoadTranscriptSourceSnapshot(ctx context.Context, id domain.SessionID) (domain.TranscriptSourceSnapshot, error) {
	rec, _, err := s.GetSession(ctx, id)
	return domain.TranscriptSourceSnapshot{Session: rec, Fingerprint: "fixture-snapshot", NativeSessions: []domain.TranscriptNativeSession{{Harness: rec.Harness, NativeID: rec.Metadata.AgentSessionID, ConfigDir: s.configDir}}}, err
}

type replacementTUIRuntime struct {
	*fakeRuntime
	commands    int
	beforeExact func()
}

func (r *replacementTUIRuntime) RunningWorkloadCommands(context.Context, ports.RuntimeHandle, ports.SupervisedProcessRef) (int, error) {
	return r.commands, nil
}

func (r *replacementTUIRuntime) IsExactSupervisedProcessAlive(ctx context.Context, handle ports.RuntimeHandle, ref ports.SupervisedProcessRef) (bool, error) {
	if r.beforeExact != nil {
		r.beforeExact()
	}
	return r.fakeRuntime.IsExactSupervisedProcessAlive(ctx, handle, ref)
}

type replacementTUIAgent struct {
	semanticSignalingAgent
	reader    *claudecode.Plugin
	captures  int
	reads     int
	afterRead func(int)
}

func (a *replacementTUIAgent) CaptureNativeTranscript(ctx context.Context, r ports.NativeTranscriptRequest) (ports.NativeTranscriptSegment, error) {
	a.captures++
	return a.reader.CaptureNativeTranscript(ctx, r)
}
func (a *replacementTUIAgent) ReadNativeTranscript(ctx context.Context, r ports.NativeTranscriptRequest) (ports.NativeTranscriptSegment, error) {
	segment, err := a.reader.ReadNativeTranscript(ctx, r)
	a.reads++
	if err == nil && a.afterRead != nil {
		a.afterRead(a.reads)
	}
	return segment, err
}

type replacementTUIFixture struct {
	manager            *Manager
	store              *replacementTUIStore
	runtime            *replacementTUIRuntime
	agent              *replacementTUIAgent
	messenger          *fakeMessenger
	op                 domain.OrchestratorReplacement
	prompt, nativePath string
}

func newReplacementTUIFixture(t *testing.T) replacementTUIFixture {
	t.Helper()
	m, base, rt, _ := newManager()
	source := pastStartupGate(mkLive("mer-1"))
	source.Kind, source.Harness, source.Mode, source.Activity.State = domain.KindOrchestrator, domain.HarnessClaudeCode, domain.SessionModeTUI, domain.ActivityIdle
	source.Metadata.RuntimeLaunchID, source.Metadata.AgentSessionIDLaunchID = "source-launch", "source-launch"
	source.Metadata.AgentSessionID, source.Metadata.BrowserCapabilityVerifier = "11111111-1111-4111-8111-111111111111", "source-verifier"
	base.sessions[source.ID] = source
	prompt := "write the handoff for this nonce"
	hash := sha256.Sum256([]byte(prompt))
	now := time.Now().UTC()
	op := domain.OrchestratorReplacement{ID: "22222222-2222-4222-8222-222222222222", ProjectID: source.ProjectID, SourceSessionID: source.ID, SourceOwner: source.ControllerOwner(), SourceCapabilityVerifier: source.Metadata.BrowserCapabilityVerifier, Phase: domain.ReplacementPreparingHandoff, Revision: 4,
		Preparation: &domain.ReplacementPreparation{AttemptID: "33333333-3333-4333-8333-333333333333", ControlClientID: "replacement:22222222-2222-4222-8222-222222222222:prepare:33333333-3333-4333-8333-333333333333", PromptHash: hex.EncodeToString(hash[:]), PromptIntentAt: &now},
	}
	store := &replacementTUIStore{replacementFenceStore: &replacementFenceStore{fakeStore: base, operation: op}, configDir: t.TempDir()}
	runtime := &replacementTUIRuntime{fakeRuntime: rt}
	rt.aliveByHandle = map[string]bool{"h1": true}
	agent := &replacementTUIAgent{reader: claudecode.New()}
	messenger := &fakeMessenger{}
	m.store, m.runtime, m.agents = store, runtime, singleAgent{agent}
	m.messenger = sessionguard.New(store, messenger, m.logger)
	m.messenger.SetInputLease(m)
	m.messenger.SetStartupSignalGate(m.harnessStartupSignalGatesInput)
	path := filepath.Join(store.configDir, "projects", "workspace", source.Metadata.AgentSessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f := replacementTUIFixture{m, store, runtime, agent, messenger, op, prompt, path}
	f.writeNative(t, false, false, "")
	return f
}

func (f replacementTUIFixture) writeNative(t *testing.T, control, child bool, change string) {
	t.Helper()
	rows := []map[string]any{
		{"uuid": "u", "parentUuid": nil, "type": "user", "promptId": "original", "message": map[string]any{"content": "original user"}},
		{"uuid": "a", "parentUuid": "u", "type": "assistant", "promptId": "original", "message": map[string]any{"content": "original answer", "stop_reason": "end_turn"}},
	}
	if control {
		body := f.prompt
		if change == "wrong payload" {
			body += " changed"
		}
		envelope := domain.WrapReportDelivery(f.op.Preparation.ControlClientID, body)
		if change == "outer whitespace" {
			envelope = "\n \t" + envelope + " \n"
		}
		rows = append(rows,
			map[string]any{"uuid": "prepare-u", "parentUuid": "a", "type": "user", "promptId": "prepare-prompt", "message": map[string]any{"content": envelope}, "isSidechain": child},
			map[string]any{"uuid": "prepare-a", "parentUuid": "prepare-u", "type": "assistant", "promptId": "prepare-prompt", "message": map[string]any{"content": "handoff written", "stop_reason": "end_turn"}, "isSidechain": child},
		)
		if child {
			rows[2]["agentId"], rows[3]["agentId"] = "native-child", "native-child"
		}
	}
	if change == "verified nonce" {
		rows[3]["parentUuid"] = "nonce-attachment"
		attachment := map[string]any{"uuid": "nonce-attachment", "parentUuid": "prepare-u", "type": "attachment", "attachment": map[string]any{"type": "hook_additional_context", "hookEvent": "UserPromptSubmit", "content": []string{domain.NativeSubmissionContext("verified-submission")}}}
		rows = append(rows[:3], attachment, rows[3])
	}
	if change == "duplicate control" {
		rows = append(rows, map[string]any{"uuid": "second-u", "parentUuid": "prepare-a", "type": "user", "promptId": "second-control", "message": map[string]any{"content": domain.WrapReportDelivery(f.op.Preparation.ControlClientID, f.prompt)}}, map[string]any{"uuid": "second-a", "parentUuid": "second-u", "type": "assistant", "promptId": "second-control", "message": map[string]any{"content": "duplicate answer", "stop_reason": "end_turn"}})
	}
	if change == "interrupted" {
		rows = append(rows, map[string]any{"uuid": "interrupt", "parentUuid": "prepare-a", "type": "user", "promptId": "prepare-prompt", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
	}
	if change == "later main input" {
		rows = append(rows, map[string]any{"uuid": "later-u", "parentUuid": "prepare-a", "type": "user", "promptId": "later", "message": map[string]any{"content": "new instruction"}}, map[string]any{"uuid": "later-a", "parentUuid": "later-u", "type": "assistant", "promptId": "later", "message": map[string]any{"content": "new answer", "stop_reason": "end_turn"}})
	}
	if change == "pending hook" {
		rows = append(rows, map[string]any{"uuid": "pending", "parentUuid": "prepare-a", "type": "system", "subtype": "stop_hook_summary", "preventedContinuation": true})
	}
	var b strings.Builder
	for _, row := range rows {
		row["sessionId"], row["version"] = f.op.SourceOwner.AgentSessionID, "2.1.0"
		if row["isSidechain"] == nil {
			row["isSidechain"] = false
		}
		if change == "unknown provenance" && row["uuid"] == "prepare-u" {
			delete(row, "isSidechain")
		}
		data, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(f.nativePath, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f replacementTUIFixture) acceptControl() {
	rec := f.store.sessions[f.op.SourceSessionID]
	rec.Metadata.ConversationCheckpointState = domain.ConversationCheckpointCoordination
	rec.Metadata.ConversationCheckpointGeneration, rec.Metadata.ConversationCheckpointNativeID, rec.Metadata.ConversationCheckpointTurnID = rec.Metadata.RuntimeLaunchID, rec.Metadata.AgentSessionID, f.op.Preparation.ControlClientID
	f.store.sessions[rec.ID] = rec
}

func TestReplacementTUIObservationRequiresSettledMainNativeControl(t *testing.T) {
	for _, which := range []string{"settled main", "verified nonce", "outer whitespace", "child only", "unknown provenance", "pending hook", "unflushed nonce", "wrong payload", "duplicate control", "interrupted", "later main input", "running workload", "runtime unknown", "active"} {
		t.Run(which, func(t *testing.T) {
			f := newReplacementTUIFixture(t)
			f.acceptControl()
			f.writeNative(t, true, which == "child only", which)
			rec := f.store.sessions[f.op.SourceSessionID]
			switch which {
			case "unflushed nonce":
				rec.Metadata.NativeCheckpointEvidence = domain.AppendNativeCheckpoint("", rec.Metadata.AgentSessionID, domain.NativeCheckpointObservation{Generation: rec.Metadata.RuntimeLaunchID, Submission: true, SubmissionID: "not-flushed", Coordination: true})
			case "verified nonce":
				rec.Metadata.NativeCheckpointEvidence = domain.AppendNativeCheckpoint("", rec.Metadata.AgentSessionID, domain.NativeCheckpointObservation{Generation: rec.Metadata.RuntimeLaunchID, Submission: true, SubmissionID: "verified-submission", Coordination: true})
				rec.Metadata.NativeCheckpointEvidence = domain.AppendNativeCheckpoint(rec.Metadata.NativeCheckpointEvidence, rec.Metadata.AgentSessionID, domain.NativeCheckpointObservation{Generation: rec.Metadata.RuntimeLaunchID, PromptID: "prepare-prompt", Text: "handoff written"})
			case "running workload":
				f.runtime.commands = 1
			case "runtime unknown":
				f.runtime.fencedResult = ports.FencedProbeResult{Liveness: ports.FencedUnknown, Reason: ports.FencedReasonProbeFailed}
			case "active":
				rec.Activity.State = domain.ActivityActive
			}
			f.store.sessions[rec.ID] = rec
			observation, err := f.manager.ObserveReplacementSource(context.Background(), f.op)
			if which == "settled main" || which == "verified nonce" || which == "outer whitespace" {
				if err != nil || observation.State != ports.ChatControllerReady || observation.Provenance != domain.NativeThreadMain || observation.ControlTurn == nil || observation.ControlTurn.State != domain.TurnStateCompleted || observation.ControlTurn.ProviderTurnID != "prepare-prompt" {
					t.Fatalf("native control unverified: %+v %v", observation, err)
				}
			} else if err == nil && observation.ControlTurn != nil && observation.ControlTurn.State == domain.TurnStateCompleted {
				t.Fatalf("%s was accepted as completed control: %+v", which, observation)
			}
			if which == "unknown provenance" && err == nil {
				t.Fatal("unknown provenance must produce a visible error")
			}
			if which == "interrupted" && (err != nil || observation.ControlTurn == nil || observation.ControlTurn.State != domain.TurnStateInterrupted) {
				t.Fatalf("interrupt was not retained: %+v %v", observation, err)
			}
			if which == "active" && f.agent.captures != 0 {
				t.Fatal("active source needlessly parsed native transcript")
			}
		})
	}
}

func TestReplacementTUIObservationRevalidatesOwnerAndNativeWitness(t *testing.T) {
	for _, which := range []string{"capability", "native append", "checkpoint advance"} {
		t.Run(which, func(t *testing.T) {
			f := newReplacementTUIFixture(t)
			f.acceptControl()
			f.writeNative(t, true, false, "")
			mutated := false
			f.agent.afterRead = func(reads int) {
				if reads != 2 {
					return
				}
				mutated = true
				if which == "capability" {
					rec := f.store.sessions[f.op.SourceSessionID]
					rec.Metadata.BrowserCapabilityVerifier = "rotated-after-native-read"
					f.store.sessions[rec.ID] = rec
					return
				}
				if which == "checkpoint advance" {
					rec := f.store.sessions[f.op.SourceSessionID]
					rec.Metadata.ConversationCheckpointTurnID = "newer-submission"
					rec.Activity.LastActivityAt = time.Now()
					f.store.sessions[rec.ID] = rec
					return
				}
				file, err := os.OpenFile(f.nativePath, os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.WriteString(`{"type":"system","subtype":"stop_hook_summary","uuid":"late-pending","parentUuid":"prepare-a","sessionId":"11111111-1111-4111-8111-111111111111","isSidechain":false,"version":"2.1.0","preventedContinuation":true}` + "\n")
				closeErr := file.Close()
				if err != nil || closeErr != nil {
					t.Fatal(err, closeErr)
				}
			}
			observation, err := f.manager.ObserveReplacementSource(context.Background(), f.op)
			if !mutated {
				t.Fatal("observation gap injection did not run")
			}
			if observation.State == ports.ChatControllerReady || which != "checkpoint advance" && err == nil {
				t.Fatalf("%s gap accepted stale readiness: %+v %v", which, observation, err)
			}
		})
	}
}

func TestReplacementTUIPaneWriteWithoutSemanticReceiptRemainsUncertain(t *testing.T) {
	f := newReplacementTUIFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.messenger.onSend = func(domain.SessionID, string) { cancel() }
	write, err := f.manager.EnsureReplacementPreparation(ctx, f.op, f.prompt, true)
	if err == nil || !write.Uncertain || write.TurnID != "" || len(f.messenger.msgs) != 1 {
		t.Fatalf("pane write was mistaken for receipt: %+v %v writes=%d", write, err, len(f.messenger.msgs))
	}
	lookup, err := f.manager.EnsureReplacementPreparation(context.Background(), f.op, f.prompt, false)
	if err != nil || !lookup.Uncertain || len(f.messenger.msgs) != 1 {
		t.Fatalf("uncertain write was blindly resent: %+v %v", lookup, err)
	}
}

func TestReplacementTUIPreparationRefusesLaunchLostImmediatelyBeforeWrite(t *testing.T) {
	f := newReplacementTUIFixture(t)
	f.runtime.beforeExact = func() { f.runtime.aliveByHandle["h1"] = false }
	f.messenger.onSend = func(domain.SessionID, string) { f.acceptControl() }
	if receipt, err := f.manager.EnsureReplacementPreparation(context.Background(), f.op, f.prompt, true); err == nil || receipt.TurnID != "" || len(f.messenger.msgs) != 0 {
		t.Fatalf("handoff prompt reached an exited runtime: receipt=%+v err=%v writes=%d", receipt, err, len(f.messenger.msgs))
	}
}

func TestReplacementTUIEnterRetryRechecksOwnedLaunch(t *testing.T) {
	f := newReplacementTUIFixture(t)
	f.manager.sendConfirm = sendConfirmConfig{pollInterval: time.Millisecond, attemptDeadline: 2 * time.Millisecond, maxAttempts: 3}
	checks := 0
	f.runtime.beforeExact = func() {
		checks++
		if checks > 1 {
			f.runtime.aliveByHandle["h1"] = false
		}
	}
	f.messenger.onSend = func(_ domain.SessionID, message string) {
		if message == "" {
			f.acceptControl()
		}
	}
	if receipt, err := f.manager.EnsureReplacementPreparation(context.Background(), f.op, f.prompt, true); err == nil || receipt.TurnID != "" || len(f.messenger.msgs) != 1 || checks < 2 {
		t.Fatalf("confirmation pressed Enter after owned launch exited: receipt=%+v err=%v writes=%v checks=%d", receipt, err, f.messenger.msgs, checks)
	}
}

func TestReplacementTUIPreparationUsesSemanticAcceptanceAndNeverBlindlyResends(t *testing.T) {
	f := newReplacementTUIFixture(t)
	ctx := context.Background()
	missing, err := f.manager.EnsureReplacementPreparation(ctx, f.op, f.prompt, false)
	if err != nil || !missing.Uncertain || len(f.messenger.msgs) != 0 {
		t.Fatalf("lookup-only sent another prompt: %+v %v", missing, err)
	}
	f.messenger.onSend = func(id domain.SessionID, message string) {
		if message != domain.WrapReportDelivery(f.op.Preparation.ControlClientID, f.prompt) {
			t.Fatalf("wrong control envelope: %q", message)
		}
		f.acceptControl()
		rec := f.store.sessions[id]
		rec.Activity.State = domain.ActivityActive
		f.store.sessions[id] = rec
	}
	accepted, err := f.manager.EnsureReplacementPreparation(ctx, f.op, f.prompt, true)
	if err != nil || accepted.Uncertain || accepted.TurnID != f.op.Preparation.ControlClientID || len(f.messenger.msgs) != 1 {
		t.Fatalf("semantic receipt missing: %+v %v", accepted, err)
	}
	again, err := f.manager.EnsureReplacementPreparation(ctx, f.op, f.prompt, false)
	if err != nil || again.TurnID != accepted.TurnID || again.Uncertain || len(f.messenger.msgs) != 1 {
		t.Fatalf("accepted prompt was resent: %+v %v", again, err)
	}
}
