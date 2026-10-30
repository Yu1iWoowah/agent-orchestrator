package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func TestProductionChatLauncherExposesReplacementCapabilities(t *testing.T) {
	var launcher sessionmanager.ChatLauncher = chatLauncher{}
	if _, ok := launcher.(ports.ReplacementChatRuntime); !ok {
		t.Fatal("production Chat launcher silently disables replacement capabilities")
	}
}

func TestReplacementProviderBridgeWaitsForPersistentHostReconcile(t *testing.T) {
	launcher := chatLauncher{persistentHostReconcileDone: make(chan struct{})}
	bridge, ok := any(launcher).(ports.ReplacementChatRuntime)
	if !ok {
		t.Fatal("replacement bridge unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	op := domain.OrchestratorReplacement{}
	calls := map[string]func() error{
		"observe source":    func() error { _, err := bridge.ObserveReplacementChatSource(ctx, op); return err },
		"prepare":           func() error { _, err := bridge.EnsureReplacementChatPreparation(ctx, op, "prompt", true); return err },
		"stop source":       func() error { _, err := bridge.QuiesceReplacementChatSource(ctx, op); return err },
		"observe successor": func() error { _, err := bridge.ObserveReplacementChatSuccessor(ctx, op); return err },
		"bootstrap":         func() error { _, err := bridge.EnsureReplacementChatBootstrap(ctx, op, "prompt", true); return err },
		"stop successor":    func() error { _, err := bridge.QuiesceReplacementChatSuccessor(ctx, op); return err },
		"probe":             func() error { _, err := bridge.EnsureReplacementChatProbe(ctx, op, "prompt", true); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, context.Canceled) {
				t.Fatal("provider action crossed startup barrier", err)
			}
		})
	}
}

func TestReplacementQueueBridgeAcceptsDurablyWithoutStartupController(t *testing.T) {
	for _, mode := range []domain.SessionMode{domain.SessionModeTUI, domain.SessionModeChat} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := context.Background()
			db := sqlitetest.MustOpen(t)
			now := time.Now().UTC()
			if err := db.UpsertProject(ctx, domain.ProjectRecord{ID: "replacement-bridge", Path: t.TempDir(), RegisteredAt: now}); err != nil {
				t.Fatal(err)
			}
			source, err := db.CreateSession(ctx, domain.SessionRecord{ProjectID: "replacement-bridge", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode, Mode: mode, Metadata: domain.SessionMetadata{RuntimeLaunchID: "source-launch", AgentSessionID: "source-native", ProviderConversationID: "source-native", BrowserCapabilityVerifier: "source-verifier"}, CreatedAt: now, UpdatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			op := domain.OrchestratorReplacement{SchemaVersion: 1, ID: "11111111-1111-4111-8111-111111111111", OperationKey: "22222222-2222-4222-8222-222222222222", RequestHash: strings.Repeat("a", 64), ProjectID: source.ProjectID, SourceSessionID: source.ID, SourceOwner: source.ControllerOwner(), SourceCapabilityVerifier: "source-verifier", Phase: domain.ReplacementAccepted, CreatedAt: now, UpdatedAt: now, DeadlineAt: now.Add(time.Hour)}
			var created bool
			op, created, err = db.CreateOrchestratorReplacement(ctx, op)
			if err != nil || !created {
				t.Fatal(created, err)
			}
			svc := chatsvc.New(chatsvc.Options{Sessions: db, Store: db, NewID: uuid.NewString})
			reconciled := make(chan struct{})
			bridge, ok := any(chatLauncher{svc: svc, persistentHostReconcileDone: reconciled}).(ports.ReplacementChatRuntime)
			if !ok {
				t.Fatal("replacement bridge unavailable")
			}
			deadline, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			turn, err := bridge.QueueReplacementIntake(deadline, source.ID, ports.ChatUserMessage{Text: "preserve user direction", ClientMessageID: "bridge-receipt", Origin: domain.MessageOriginHuman, AuthoredByUser: true})
			if err != nil || turn.ID == "" {
				t.Fatal("controller-independent durable queue was blocked", err)
			}
			stored, err := db.TurnByID(ctx, turn.ID)
			if err != nil || stored.State != domain.TurnStateQueued {
				t.Fatal("queue receipt lost", stored, err)
			}
			if _, err := svc.Controller(source.ID); !errors.Is(err, chatsvc.ErrNoController) {
				t.Fatal("queue started a controller", err)
			}
			close(reconciled)
			if mode == domain.SessionModeChat {
				if _, err := bridge.ObserveReplacementChatSource(ctx, op); !errors.Is(err, chatsvc.ErrNoController) {
					t.Fatal("source observation was not forwarded to actual service", err)
				}
				if _, err := bridge.EnsureReplacementChatPreparation(ctx, op, "prompt", false); !errors.Is(err, chatsvc.ErrNoController) {
					t.Fatal("preparation was not forwarded to actual service", err)
				}
			}
		})
	}
}
