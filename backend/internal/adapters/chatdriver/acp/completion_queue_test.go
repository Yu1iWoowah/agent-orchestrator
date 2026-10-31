package acp

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// Keep ACP ownership until the test releases it, even after the controller
// receives completion. Only the wire start is fake; SendTurn uses the adapter.
type completionQueueConversation struct {
	ports.ChatConversation
	acp    *conversation
	events chan ports.ChatEvent
	acks   chan string
	starts atomic.Int32
}

func (c *completionQueueConversation) Events() <-chan ports.ChatEvent { return c.events }
func (*completionQueueConversation) ProviderConversationID() string   { return "native" }
func (*completionQueueConversation) Capabilities() ports.ChatCapabilities {
	return ports.ChatCapabilities{ports.ChatCapabilityStreaming: true, ports.ChatCapabilityApprovals: true, ports.ChatCapabilityInterrupt: true, ports.ChatCapabilityResume: true}
}
func (c *completionQueueConversation) SendTurn(ctx context.Context, msg ports.ChatUserMessage) (ports.ChatTurnRef, error) {
	return c.acp.SendTurn(ctx, msg)
}
func (c *completionQueueConversation) StartDeferredTurn(id string) error {
	c.starts.Add(1)
	c.acp.mu.Lock()
	c.acp.prepared = nil
	c.acp.activeTurn = id
	c.acp.mu.Unlock()
	return nil
}
func (c *completionQueueConversation) DiscardDeferredTurn(id string)         { c.acp.DiscardDeferredTurn(id) }
func (c *completionQueueConversation) Close() error                          { close(c.events); return nil }
func (*completionQueueConversation) Interrupt(context.Context, string) error { return nil }
func (c *completionQueueConversation) AcknowledgeProviderEvent(_ context.Context, id string) error {
	c.acks <- id
	return nil
}

type completionQueueDriver struct {
	ports.ChatDriver
	conv *completionQueueConversation
}

func (d completionQueueDriver) Probe(context.Context) (ports.ChatCapabilities, error) {
	return d.conv.Capabilities(), nil
}
func (d completionQueueDriver) Start(context.Context, ports.ChatStartConfig) (ports.ChatConversation, error) {
	return d.conv, nil
}

type completionQueueRegistry struct {
	ports.ChatDriverRegistry
	driver completionQueueDriver
}

func (r completionQueueRegistry) Driver(domain.AgentHarness) (ports.ChatDriver, error) {
	return r.driver, nil
}
func (completionQueueRegistry) SupportsChat(domain.AgentHarness) bool { return true }

func TestCompletionWindowPreservesControllerQueue(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.MustOpenAt(t, t.TempDir())
	now := time.Now().UTC()
	if err := db.UpsertProject(ctx, domain.ProjectRecord{ID: "project", Path: t.TempDir(), RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	session, err := db.CreateSession(ctx, domain.SessionRecord{ProjectID: "project", Kind: domain.KindOrchestrator, Harness: domain.HarnessCodex, Mode: domain.SessionModeChat, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	conv := &completionQueueConversation{
		acp: &conversation{sessionID: "native"}, events: make(chan ports.ChatEvent, 16), acks: make(chan string, 16),
	}
	svc := chatsvc.New(chatsvc.Options{Store: db, Sessions: db, NewID: uuid.NewString,
		Drivers: completionQueueRegistry{driver: completionQueueDriver{conv: conv}}, Log: slog.New(slog.DiscardHandler)})
	ctrl, err := svc.Start(ctx, chatsvc.StartConfig{SessionID: session.ID, ProjectID: session.ProjectID, Harness: domain.HarnessCodex, WorkspacePath: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.StopAll(ctx) })
	fence := func(id string) {
		t.Helper()
		conv.events <- ports.ChatEvent{Kind: ports.ChatEventActivityCompleted, ProviderTurnID: "previous", ProviderEventID: id, ProviderItemID: id, ActivityKind: domain.ActivityKindSystem, ActivityStatus: domain.ActivityStatusCompleted, Summary: id}
		for {
			select {
			case ack := <-conv.acks:
				if ack == id {
					return
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("event fence %s did not commit", id)
			}
		}
	}
	conv.events <- ports.ChatEvent{Kind: ports.ChatEventTurnStarted, ProviderTurnID: "previous", ProviderInitiated: true}
	fence("started")
	turn, err := ctrl.Send(ctx, ports.ChatUserMessage{Text: "preserve this queued input", ClientMessageID: "queued"})
	if err != nil || turn.State != domain.TurnStateQueued {
		t.Fatalf("queue: %+v %v", turn, err)
	}
	conv.acp.mu.Lock()
	conv.acp.activeTurn = "previous"
	conv.acp.mu.Unlock()
	conv.events <- ports.ChatEvent{Kind: ports.ChatEventTurnCompleted, ProviderTurnID: "previous", TurnState: domain.TurnStateCompleted}
	fence("completed-before-release")
	saved, err := db.TurnByID(ctx, turn.ID)
	if err != nil || saved.State != domain.TurnStateQueued || saved.AgentSettings != nil || conv.starts.Load() != 0 {
		t.Fatalf("completion window lost queued input: state=%s starts=%d err=%v", saved.State, conv.starts.Load(), err)
	}
	conv.acp.mu.Lock()
	conv.acp.activeTurn = ""
	conv.acp.mu.Unlock()
	// Repeated Ready notifications must not duplicate the dispatch.
	for range 3 {
		conv.events <- ports.ChatEvent{Kind: ports.ChatEventControllerState, ControllerState: ports.ChatControllerReady}
	}
	fence("ready")
	saved, err = db.TurnByID(ctx, turn.ID)
	if err != nil || saved.State != domain.TurnStateRunning || conv.starts.Load() != 1 {
		t.Fatalf("Ready lost or duplicated input: state=%s starts=%d err=%v", saved.State, conv.starts.Load(), err)
	}
}
