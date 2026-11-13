package chat_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	sqlite "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// heldResume holds each resume at the provider, the long part of a start, until
// the test says how that attempt ends.
type heldResume struct {
	entered chan struct{}
	release chan error // nil: the resume succeeds with conv
	conv    *fakeConversation
}

// newHeldService builds a daemon's chat service whose fresh starts use first
// and whose resumes can be held at the provider.
func newHeldService(st *sqlite.Store, first *fakeConversation, idPrefix string) (*chatsvc.Service, *heldResume) {
	held := &heldResume{entered: make(chan struct{}, 4), release: make(chan error, 1), conv: newFakeConversation()}
	var ids atomic.Int32
	svc := chatsvc.New(chatsvc.Options{
		Store: st, Sessions: st, Reader: fullSnapshotReader(st),
		Drivers: fakeRegistry{driver: fakeDriver{
			start: func(ports.ChatStartConfig) (ports.ChatConversation, error) { return first, nil },
			resume: func(ports.ChatResumeConfig) (ports.ChatConversation, error) {
				held.entered <- struct{}{}
				if err := <-held.release; err != nil {
					return nil, err
				}
				return held.conv, nil
			},
		}},
		Log:   slog.New(slog.DiscardHandler),
		NewID: func() string { return fmt.Sprintf("%s-%d", idPrefix, ids.Add(1)) },
	})
	return svc, held
}

// startedService runs a session's first controller and records its provider
// thread, so a later start resumes it.
func startedService(t *testing.T) (*chatsvc.Service, *sqlite.Store, *heldResume, *fakeConversation, string) {
	t.Helper()
	st := openStore(t)
	first := newFakeConversation()
	svc, held := newHeldService(st, first, "starting")
	ctx := context.Background()
	workspace := t.TempDir()
	ctrl, err := svc.Start(ctx, chatsvc.StartConfig{SessionID: testSession, ProjectID: testProject, Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, WorkspacePath: workspace})
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	rec, found, err := st.GetSession(ctx, testSession)
	if err != nil || !found {
		t.Fatalf("read session: %v", err)
	}
	rec.Metadata.ProviderConversationID = first.ProviderConversationID()
	rec.Metadata.ControllerGeneration = ctrl.Generation()
	if err := st.UpdateSession(ctx, rec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background(), testSession) })
	return svc, st, held, first, workspace
}

// startingService stops the first controller and returns the service whose
// next resume can be held at the provider.
func startingService(t *testing.T) (*chatsvc.Service, *sqlite.Store, *heldResume, string) {
	t.Helper()
	svc, st, held, _, workspace := startedService(t)
	if err := svc.Stop(context.Background(), testSession); err != nil {
		t.Fatalf("stop: %v", err)
	}
	return svc, st, held, workspace
}

// resume starts the session again in the background and waits until it is
// held at the provider.
func resume(t *testing.T, svc *chatsvc.Service, held *heldResume, workspace string) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := svc.Start(context.Background(), chatsvc.StartConfig{
			SessionID: testSession, ProjectID: testProject, Kind: domain.KindOrchestrator,
			Harness: domain.HarnessCodex, WorkspacePath: workspace, ProviderConversationID: "thread-1",
		})
		done <- err
	}()
	// A failed assertion must not leave the resume holding the gate that the
	// cleanup's Stop waits for.
	t.Cleanup(func() {
		select {
		case held.release <- errors.New("test ended"):
		default:
		}
	})
	select {
	case <-held.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("resume did not reach the provider")
	}
	return done
}

// promptly fails the test when a request takes the daemon a second or more.
func promptly[T any](t *testing.T, name string, call func() (T, error)) (T, error) {
	t.Helper()
	started := time.Now()
	value, err := call()
	if took := time.Since(started); took >= time.Second {
		t.Fatalf("%s took %v while the controller was starting", name, took)
	}
	return value, err
}

func finishProviderTurn(conv *fakeConversation, providerTurn string) {
	conv.emit(
		ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: providerTurn, ProviderConversationID: conv.ProviderConversationID()},
		ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: providerTurn, TurnState: domain.TurnStateCompleted, ProviderConversationID: conv.ProviderConversationID()},
	)
}

func awaitSent(t *testing.T, conv *fakeConversation, want []string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Equal(conv.sentTexts(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("sent = %q, want %q", conv.sentTexts(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStartingControllerAnswersAtOnceAndDeliversQueuedInputInOrder(t *testing.T) {
	svc, _, held, workspace := startingService(t)
	ctx := context.Background()
	done := resume(t, svc, held, workspace)

	if _, err := promptly(t, "chat-view", func() (struct{}, error) {
		return struct{}{}, svc.SetChatView(ctx, testSession, "view-1", true)
	}); err != nil {
		t.Fatalf("chat-view = %v", err)
	}
	if _, err := promptly(t, "config-options", func() ([]ports.ChatConfigOption, error) {
		return svc.ConfigOptions(ctx, testSession)
	}); !errors.Is(err, chatsvc.ErrNoController) {
		t.Fatalf("config-options = %v, want ErrNoController", err)
	}
	if _, err := promptly(t, "models", func() ([]ports.ChatModel, error) {
		models, _, err := svc.Models(ctx, testSession)
		return models, err
	}); !errors.Is(err, chatsvc.ErrNoController) {
		t.Fatalf("models = %v, want ErrNoController", err)
	}
	snapshot, err := promptly(t, "snapshot", func() (chatsvc.Snapshot, error) { return svc.Snapshot(ctx, testSession) })
	if err != nil || snapshot.Controller != ports.ChatControllerConnecting {
		t.Fatalf("snapshot while starting = %q, %v; want connecting", snapshot.Controller, err)
	}

	first, err := promptly(t, "messages", func() (domain.ConversationTurn, error) {
		return svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "first", ClientMessageID: "msg-a", Origin: domain.MessageOriginHuman})
	})
	if err != nil || first.ID == "" || first.State != domain.TurnStateQueued {
		t.Fatalf("send while starting = %+v, %v; want a queued turn", first, err)
	}
	duplicate, err := promptly(t, "messages retry", func() (domain.ConversationTurn, error) {
		return svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "first", ClientMessageID: "msg-a", Origin: domain.MessageOriginHuman})
	})
	if err != nil || duplicate.ID != "" {
		t.Fatalf("retried send = %+v, %v; want the existing message", duplicate, err)
	}
	steered, err := promptly(t, "steer-or-send", func() (chatsvc.SteerOrSendResult, error) {
		return svc.SteerOrSend(ctx, testSession, ports.ChatUserMessage{Text: "second", ClientMessageID: "msg-b", Origin: domain.MessageOriginHuman}, false)
	})
	if err != nil || steered.Steered || steered.Turn.State != domain.TurnStateQueued {
		t.Fatalf("steer-or-send while starting = %+v, %v; want a queued turn", steered, err)
	}
	if _, err := promptly(t, "session send", func() (string, error) {
		return svc.RelaySessionChatTurn(ctx, testSession, "third", "msg-c", ports.MessageDeliveryOptions{})
	}); err != nil {
		t.Fatalf("session send while starting = %v", err)
	}
	if sent := held.conv.sentTexts(); len(sent) != 0 {
		t.Fatalf("sent before the controller started: %q", sent)
	}

	held.release <- nil
	if err := <-done; err != nil {
		t.Fatalf("resume: %v", err)
	}
	awaitSent(t, held.conv, []string{"first"})
	// The rest goes as the controller's usual batch, still in enqueue order.
	finishProviderTurn(held.conv, "provider-turn-1")
	deadline := time.Now().Add(5 * time.Second)
	for len(held.conv.sentTexts()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	sent := held.conv.sentTexts()
	if len(sent) != 2 {
		t.Fatalf("sent = %q, want the first message then one batch", sent)
	}
	second, third := strings.Index(sent[1], `"text":"second"`), strings.Index(sent[1], `"text":"third"`)
	if second < 0 || third < second || strings.Contains(sent[1], `"text":"first"`) {
		t.Fatalf("batch after the first turn = %q, want second then third, once each", sent[1])
	}
}

func TestFailedStartKeepsQueuedInputForTheNextStart(t *testing.T) {
	svc, st, held, workspace := startingService(t)
	ctx := context.Background()

	done := resume(t, svc, held, workspace)
	queued, err := svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "kept", ClientMessageID: "msg-kept", Origin: domain.MessageOriginHuman})
	if err != nil || queued.ID == "" {
		t.Fatalf("send while starting = %+v, %v", queued, err)
	}
	held.release <- errors.New("provider unavailable")
	if err := <-done; err == nil {
		t.Fatal("held resume unexpectedly succeeded")
	}
	turn, err := st.TurnByID(ctx, queued.ID)
	if err != nil || turn.State != domain.TurnStateQueued {
		t.Fatalf("queued input after a failed start = %q, %v; want queued", turn.State, err)
	}
	if snapshot, err := svc.Snapshot(ctx, testSession); err != nil || snapshot.Controller != ports.ChatControllerStopped {
		t.Fatalf("snapshot after a failed start = %q, %v; want stopped", snapshot.Controller, err)
	}

	done = resume(t, svc, held, workspace)
	held.release <- nil
	if err := <-done; err != nil {
		t.Fatalf("second resume: %v", err)
	}
	awaitSent(t, held.conv, []string{"kept"})
}

func awaitTurnState(t *testing.T, st *sqlite.Store, turnID string, want domain.TurnState) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		turn, err := st.TurnByID(context.Background(), turnID)
		if err == nil && turn.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn %s = %q, %v; want %q", turnID, turn.State, err, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStartAfterCrashDeliversInputQueuedWhileStarting(t *testing.T) {
	crashed, st, _, first, workspace := startedService(t)
	ctx := context.Background()
	// The daemon dies with one turn running and one queued behind it.
	running, err := crashed.Send(ctx, testSession, ports.ChatUserMessage{Text: "running at the crash", ClientMessageID: "msg-running", Origin: domain.MessageOriginHuman})
	if err != nil {
		t.Fatal(err)
	}
	first.emit(ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "provider-turn-1", ProviderConversationID: first.ProviderConversationID()})
	awaitTurnState(t, st, running.ID, domain.TurnStateRunning)
	queuedBefore, err := crashed.Send(ctx, testSession, ports.ChatUserMessage{Text: "queued before the crash", ClientMessageID: "msg-before", Origin: domain.MessageOriginHuman})
	if err != nil || queuedBefore.State != domain.TurnStateQueued {
		t.Fatalf("queue behind the running turn = %+v, %v", queuedBefore, err)
	}

	// The next daemon resumes the session while the user types.
	svc, held := newHeldService(st, first, "restarted")
	t.Cleanup(func() { _ = svc.Stop(context.Background(), testSession) })
	done := resume(t, svc, held, workspace)
	intake, err := svc.Send(ctx, testSession, ports.ChatUserMessage{Text: "typed while starting", ClientMessageID: "msg-intake", Origin: domain.MessageOriginHuman})
	if err != nil || intake.State != domain.TurnStateQueued {
		t.Fatalf("send while starting = %+v, %v", intake, err)
	}
	held.release <- nil
	if err := <-done; err != nil {
		t.Fatalf("resume: %v", err)
	}
	awaitSent(t, held.conv, []string{"typed while starting"})
	// What the crash left behind is settled as before.
	awaitTurnState(t, st, running.ID, domain.TurnStateFailed)
	awaitTurnState(t, st, queuedBefore.ID, domain.TurnStateFailed)
}
