package chat_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

func startReplacementNativeReplay(t *testing.T, history string) (successorChatFixture, *nativeHistoryConversation, ports.ChatResumeConfig, int, error) {
	t.Helper()
	f, _ := settledSuccessorFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	initial := f.op.SuccessorController
	f.op.Phase = domain.ReplacementVerifyingResume
	f.op.ResumeEvidence = &domain.ReplacementResumeEvidence{StartedAt: now, BeforeOwner: initial.Owner, BeforeNativeID: initial.Owner.ProviderConversationID, BeforeCapabilityVerifier: initial.CapabilityVerifier}
	if saved, err := f.store.SaveReplacementRuntimeProgress(ctx, f.op); err != nil || !saved {
		t.Fatal(saved, err)
	}
	var err error
	f.op, _, err = f.store.GetOrchestratorReplacement(ctx, f.op.ID)
	if err != nil {
		t.Fatal(err)
	}
	stop := quiesceIdleSuccessor(t, f)
	p := f.op.ResumeEvidence
	p.StopConfirmed, p.StopReceipt, p.ResumeIntentAt, p.ResumeGeneration = true, &stop, &now, "replacement-resume-generation"
	if saved, err := f.store.SaveReplacementRuntimeProgress(ctx, f.op); err != nil || !saved {
		t.Fatal(saved, err)
	}
	rec, _, err := f.store.GetSession(ctx, f.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := f.store.UpdateBrowserCapabilityVerifier(ctx, rec.ID, rec.ControllerOwner(), "resume-verifier"); err != nil || !applied {
		t.Fatal(applied, err)
	}
	native := newFakeConversation()
	native.providerConversationID = p.BeforeNativeID
	if history == "different native" {
		native.providerConversationID = "fresh-native"
	}
	replay := &nativeHistoryConversation{fakeConversation: native}
	if history != "missing high water" {
		replay.events = []ports.ChatEvent{{Kind: ports.ChatEventUserMessageCompleted, ProviderEventID: "bootstrap-user", NativeUserMessageID: "native-bootstrap-user", ProviderItemID: "bootstrap-user", ProviderTurnID: "provider-turn-1", Text: f.provider.sentTexts()[0]}, {Kind: ports.ChatEventTurnStarted, ProviderEventID: "bootstrap-start", ProviderTurnID: "provider-turn-1"}, {Kind: ports.ChatEventTurnCompleted, ProviderEventID: "bootstrap-complete", ProviderTurnID: "provider-turn-1", TurnState: domain.TurnStateCompleted}}
	}
	var resumed ports.ChatResumeConfig
	service := chatsvc.New(chatsvc.Options{Store: f.store, Sessions: f.store, Reader: fullSnapshotReader(f.store), Drivers: fakeRegistry{driver: fakeDriver{conv: replay, resumeCfg: &resumed}}, NewID: sequentialIDs("resumed-successor"), Log: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { service.StopAll(context.Background()) })
	manager := lifecycle.New(f.store, nil)
	ready := 0
	_, err = service.Start(ctx, chatsvc.StartConfig{SessionID: rec.ID, ProjectID: rec.ProjectID, Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, WorkspacePath: t.TempDir(), ProviderConversationID: p.BeforeNativeID, ControllerGeneration: p.ResumeGeneration, HistoryMode: ports.ChatHistoryRequired, ExpectedControllerOwner: p.BeforeOwner, ControllerReady: func(started ports.ChatControllerStarted) (ports.ChatControllerCommit, error) {
		ready++
		if started.LiveReconnect || started.ProviderConversationID != p.BeforeNativeID {
			t.Fatal("resume callback bypassed native reload")
		}
		metadata := rec.Metadata
		metadata.ControllerGeneration, metadata.ProviderConversationID, metadata.BrowserCapabilityVerifier = started.ControllerGeneration, started.ProviderConversationID, "resume-verifier"
		err := manager.MarkSpawned(ctx, rec.ID, metadata)
		return ports.ChatControllerCommit{Conversation: started.Conversation}, err
	}})
	f.service, f.provider = service, native
	var readErr error
	f.op, _, readErr = f.store.GetOrchestratorReplacement(ctx, f.op.ID)
	if readErr != nil {
		t.Fatal(readErr)
	}
	return f, replay, resumed, ready, err
}

func resumedSuccessorFixture(t *testing.T) successorChatFixture {
	t.Helper()
	f, _, _, ready, err := startReplacementNativeReplay(t, "same native complete replay")
	if err != nil || ready != 1 {
		t.Fatal("fixture did not actually resume", ready, err)
	}
	ctx := context.Background()
	rec, _, err := f.store.GetSession(ctx, f.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := f.store.SetSessionHibernated(ctx, rec.ID, rec.Revision, nil); err != nil || !applied {
		t.Fatal(applied, err)
	}
	now := time.Now().UTC()
	proof := f.op.ResumeEvidence
	proof.ResumedAt, proof.HistoryVerified = &now, true
	proof.AfterOwner, proof.AfterNativeID, proof.AfterCapabilityVerifier = rec.ControllerOwner(), rec.Metadata.ProviderConversationID, rec.Metadata.BrowserCapabilityVerifier
	f.op.ResumeProbeClientID = "replacement:" + f.op.ID + ":probe"
	if saved, err := f.store.SaveReplacementRuntimeProgress(ctx, f.op); err != nil || !saved {
		t.Fatal(saved, err)
	}
	f.op, _, err = f.store.GetOrchestratorReplacement(ctx, f.op.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The restarted fake provider must allocate a new native turn identity.
	f.provider.mu.Lock()
	f.provider.turnSeq = 1
	f.provider.mu.Unlock()
	return f
}

func TestReplacementNativeResumeRequiresActualBootstrapReplay(t *testing.T) {
	for _, history := range []string{"same native complete replay", "different native", "missing high water"} {
		t.Run(history, func(t *testing.T) {
			f, replay, resumed, ready, err := startReplacementNativeReplay(t, history)
			if resumed.FreshIfMissing {
				t.Fatal("required replay allowed fresh replacement")
			}
			if history == "same native complete replay" {
				current, _, readErr := f.store.GetSession(context.Background(), f.target.ID)
				proof := f.op.ResumeEvidence
				if err != nil || readErr != nil || ready != 1 || replay.historyReads() == 0 || current.Metadata.ProviderConversationID != proof.BeforeNativeID || current.Metadata.ControllerGeneration != proof.ResumeGeneration || !f.service.HasLiveChatController(f.target.ID) {
					t.Fatal("actual same-native replay failed", ready, err, readErr)
				}
			} else if err == nil || ready != 0 || f.service.HasLiveChatController(f.target.ID) {
				t.Fatal("incomplete/different replay published owner", history, ready, err)
			}
		})
	}
}
