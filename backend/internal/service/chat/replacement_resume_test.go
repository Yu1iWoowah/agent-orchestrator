package chat_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/lifecycle"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
)

type replacementLaunchProofConversation struct {
	*nativeHistoryConversation
	live     bool
	verifier string
}

func (c *replacementLaunchProofConversation) ReconnectedLive() bool            { return c.live }
func (c *replacementLaunchProofConversation) LaunchCapabilityVerifier() string { return c.verifier }

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
	if !strings.Contains(history, "missing high water") {
		replay.events = []ports.ChatEvent{{Kind: ports.ChatEventUserMessageCompleted, ProviderEventID: "bootstrap-user", NativeUserMessageID: "native-bootstrap-user", ProviderItemID: "bootstrap-user", ProviderTurnID: "provider-turn-1", Text: f.provider.sentTexts()[0]}, {Kind: ports.ChatEventTurnStarted, ProviderEventID: "bootstrap-start", ProviderTurnID: "provider-turn-1"}, {Kind: ports.ChatEventTurnCompleted, ProviderEventID: "bootstrap-complete", ProviderTurnID: "provider-turn-1", TurnState: domain.TurnStateCompleted}}
	}
	var resumed ports.ChatResumeConfig
	live := strings.HasPrefix(history, "live")
	verifier := "resume-verifier"
	if history == "live missing launch proof" {
		verifier = ""
	}
	if history == "live wrong launch verifier" {
		verifier = "unpublished-verifier"
	}
	var conv ports.ChatConversation = &replacementLaunchProofConversation{nativeHistoryConversation: replay, live: live, verifier: verifier}
	driver := fakeDriver{conv: conv, resumeCfg: &resumed}
	var selected ports.ChatDriver = driver
	if live {
		selected = grantReconnectingDriver{fakeDriver: driver}
	}
	service := chatsvc.New(chatsvc.Options{Store: f.store, Sessions: f.store, Reader: fullSnapshotReader(f.store), Drivers: fakeRegistry{driver: selected}, NewID: sequentialIDs("resumed-successor"), Log: slog.New(slog.DiscardHandler)})
	t.Cleanup(func() { service.StopAll(context.Background()) })
	manager := lifecycle.New(f.store, nil)
	ready := 0
	_, err = service.Start(ctx, chatsvc.StartConfig{ReconnectOnly: live, SessionID: rec.ID, ProjectID: rec.ProjectID, Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, WorkspacePath: t.TempDir(), ProviderConversationID: p.BeforeNativeID, ControllerGeneration: p.ResumeGeneration, HistoryMode: ports.ChatHistoryRequired, HistoryPolicy: domain.SessionInterfaceTransitionHistoryStrict, DisableFreshProviderFallback: history != "ordinary", ExpectedControllerOwner: p.BeforeOwner, ControllerReady: func(started ports.ChatControllerStarted) (ports.ChatControllerCommit, error) {
		ready++
		if started.LiveReconnect != live || started.ProviderConversationID != p.BeforeNativeID {
			t.Fatal("resume callback bypassed native reload")
		}
		metadata := rec.Metadata
		metadata.ControllerGeneration, metadata.ProviderConversationID, metadata.BrowserCapabilityVerifier = started.ControllerGeneration, started.ProviderConversationID, "resume-verifier"
		if history == "capability changes at ready" {
			current, _, readErr := f.store.GetSession(ctx, rec.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if ok, err := f.store.UpdateBrowserCapabilityVerifier(ctx, rec.ID, current.ControllerOwner(), "unpublished-verifier"); err != nil || !ok {
				t.Fatal(ok, err)
			}
		}
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

func TestReplacementLiveResumeLookupRequiresStrictReplayIdentity(t *testing.T) {
	f, _, _, ready, err := startReplacementNativeReplay(t, "same native complete replay")
	if err != nil || ready != 1 {
		t.Fatal(ready, err)
	}
	lookup, ok := any(f.service).(interface {
		LookupReplacementChatResume(context.Context, domain.SessionID, string, string) (ports.ReplacementResumeObservation, error)
	})
	if !ok {
		t.Fatal("no strict replay lookup available")
	}
	p := f.op.ResumeEvidence
	for _, tc := range []struct {
		generation, native string
		valid              bool
	}{{p.ResumeGeneration, p.BeforeNativeID, true}, {"stale-generation", p.BeforeNativeID, false}, {p.ResumeGeneration, "another-native", false}} {
		got, err := lookup.LookupReplacementChatResume(context.Background(), f.target.ID, tc.generation, tc.native)
		if tc.valid {
			if err != nil || !got.HistoryVerified || got.ObservedAt.IsZero() {
				t.Fatal(got, err)
			}
		} else if err == nil || got.HistoryVerified {
			t.Fatal("invalid identity gained replay evidence", got, err)
		}
	}
}

func TestReplacementReconnectOnlyStillVerifiesNativeHighWater(t *testing.T) {
	for _, history := range []string{"live complete", "live missing high water", "live missing launch proof", "live wrong launch verifier", "ordinary"} {
		t.Run(history, func(t *testing.T) {
			f, replay, _, ready, err := startReplacementNativeReplay(t, history)
			if history == "live missing high water" || history == "live missing launch proof" || history == "live wrong launch verifier" {
				if err == nil || ready != 0 {
					t.Fatal("live connection bypassed required replay", ready, err)
				}
				return
			}
			if err != nil || ready != 1 || replay.reads.Load() == 0 {
				t.Fatal("ready without native replay", ready, err)
			}
			p := f.op.ResumeEvidence
			got, err := f.service.LookupReplacementChatResume(context.Background(), f.target.ID, p.ResumeGeneration, p.BeforeNativeID)
			if history == "ordinary" {
				if err == nil || got.HistoryVerified {
					t.Fatal("ordinary controller gained replacement proof", got, err)
				}
			} else if err != nil || !got.HistoryVerified {
				t.Fatal("strict live replay proof missing", got, err)
			}
		})
	}
}

func TestReplacementLiveResumeLookupRejectsCapabilityOnlyRotation(t *testing.T) {
	f, _, _, ready, err := startReplacementNativeReplay(t, "same native complete replay")
	if err != nil || ready != 1 {
		t.Fatal(ready, err)
	}
	ctx := context.Background()
	rec, _, err := f.store.GetSession(ctx, f.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.store.UpdateBrowserCapabilityVerifier(ctx, rec.ID, rec.ControllerOwner(), "unpublished-verifier"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	p := f.op.ResumeEvidence
	got, err := f.service.LookupReplacementChatResume(ctx, rec.ID, p.ResumeGeneration, p.BeforeNativeID)
	if err == nil || got.HistoryVerified {
		t.Fatal("unpublished capability gained positive resume proof", got, err)
	}
}

func TestReplacementReplayReadyCannotOverwriteRotatedCapability(t *testing.T) {
	f, _, _, _, err := startReplacementNativeReplay(t, "capability changes at ready")
	if err == nil {
		t.Fatal("stale ready overwrote a capability rotation")
	}
	rec, _, err := f.store.GetSession(context.Background(), f.target.ID)
	if err != nil || rec.Metadata.BrowserCapabilityVerifier != "unpublished-verifier" || f.op.ResumeEvidence.ResumedAt != nil {
		t.Fatal("ready failure lost fence or minted proof", err)
	}
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
