package sessionmanager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type changingReplacementResumeChat struct {
	*replacementPreflightChat
	change func()
}

func (c changingReplacementResumeChat) StartChat(ctx context.Context, cfg ChatStart) (ChatStarted, error) {
	c.change()
	return c.replacementPreflightChat.StartChat(ctx, cfg)
}

func TestReplacementNativeResumeRequiresStrongStopAndExactPinnedOwner(t *testing.T) {
	for _, change := range []string{"resume", "lookup only", "live controller", "wrong native", "wrong verifier", "missing stop receipt", "account drift", "project drift", "prelaunch project drift", "prelaunch archived", "expired deadline", "owner rotated after ready", "history failure"} {
		t.Run(change, func(t *testing.T) {
			m, store, chat, deferred := replacementSuccessorFixture(t)
			ctx := context.Background()
			seed, err := m.EnsureReplacementSuccessor(ctx, store.operation)
			if err != nil {
				t.Fatal(err)
			}
			(*deferred)[0]()
			rec := store.sessions[seed.ID]
			rec.Metadata.WorkspacePath = t.TempDir()
			now := time.Now().UTC()
			rec.HibernatedAt = &now
			store.sessions[seed.ID] = rec
			op := &store.operation
			op.DeadlineAt = now.Add(time.Hour)
			op.Phase, op.FrozenAt = domain.ReplacementVerifyingResume, &now
			op.SuccessorController = &domain.ReplacementSuccessorController{Owner: rec.ControllerOwner(), CapabilityVerifier: rec.Metadata.BrowserCapabilityVerifier, CommittedAt: now}
			op.Bootstrap = &domain.ReplacementBootstrap{TurnID: "bootstrap", SettledAt: &now}
			op.ResumeEvidence = &domain.ReplacementResumeEvidence{StartedAt: now, BeforeOwner: rec.ControllerOwner(), BeforeNativeID: rec.Metadata.ProviderConversationID, BeforeCapabilityVerifier: rec.Metadata.BrowserCapabilityVerifier, StopConfirmed: true, StopReceipt: &domain.ReplacementControllerStop{Owner: rec.ControllerOwner(), CapabilityVerifier: rec.Metadata.BrowserCapabilityVerifier, NativeID: rec.Metadata.ProviderConversationID, Method: "chat-hibernate", ConfirmedAt: now}, ResumeIntentAt: &now, ResumeGeneration: "replacement-resume-generation"}
			beforeStarts, beforeSeeds := len(chat.started), store.creates
			allowNew := change != "lookup only"
			switch change {
			case "live controller":
				chat.live = true
			case "wrong native":
				rec.Metadata.ProviderConversationID = "different-native"
				store.sessions[rec.ID] = rec
			case "wrong verifier":
				rec.Metadata.BrowserCapabilityVerifier = "different-verifier"
				store.sessions[rec.ID] = rec
			case "missing stop receipt":
				op.ResumeEvidence.StopReceipt = nil
			case "account drift":
				op.TargetAccountID = "another-account"
			case "project drift":
				project := store.projects[string(op.ProjectID)]
				project.Config.OrchestratorRules = "changed"
				store.projects[string(op.ProjectID)] = project
			case "history failure":
				chat.startErr = errors.New("required native history was incomplete")
			case "prelaunch project drift", "prelaunch archived":
				m.chat = changingReplacementResumeChat{replacementPreflightChat: chat, change: func() {
					project := store.projects[string(op.ProjectID)]
					if change == "prelaunch archived" {
						project.ArchivedAt = time.Now()
					} else {
						project.Config.OrchestratorRules = "changed immediately before native launch"
					}
					store.projects[string(op.ProjectID)] = project
				}}
			case "expired deadline":
				op.DeadlineAt = now.Add(-time.Second)
			case "owner rotated after ready":
				chat.afterReady = func() {
					current := store.sessions[seed.ID]
					current.Metadata.ControllerGeneration = "another-owner"
					store.sessions[seed.ID] = current
				}
			}
			var accountContext *replacementAccountObservingChat
			if change == "resume" {
				accountContext = &replacementAccountObservingChat{replacementPreflightChat: chat}
				m.chat = accountContext
			}
			observed, err := m.ResumeReplacementSuccessor(ctx, *op, allowNew)
			if change == "resume" {
				if !accountContext.found || accountContext.selected.AccountID != op.TargetAccountID {
					t.Fatalf("native resume lost pinned account context: found=%t selected=%s want=%s", accountContext.found, accountContext.selected.AccountID, op.TargetAccountID)
				}
				current := store.sessions[rec.ID]
				if err != nil || !observed.HistoryVerified || observed.ObservedAt.IsZero() || observed.NativeID != rec.Metadata.ProviderConversationID || observed.Owner != current.ControllerOwner() || observed.Owner.ControllerGeneration != "replacement-resume-generation" || observed.CapabilityVerifier == rec.Metadata.BrowserCapabilityVerifier || observed.CapabilityVerifier != current.Metadata.BrowserCapabilityVerifier {
					t.Fatal("resume proof not exact", observed, err)
				}
				cfg := chat.started[len(chat.started)-1]
				if cfg.HistoryMode != ports.ChatHistoryRequired || cfg.FreshIfProviderConversationMissing || !cfg.DisableFreshProviderFallback || cfg.ProviderConversationID != rec.Metadata.ProviderConversationID || cfg.ReconnectOnly {
					t.Fatal("resume weakened native history admission", cfg.HistoryMode, cfg.FreshIfProviderConversationMissing)
				}
			} else if err == nil || observed.HistoryVerified {
				t.Fatal("unverified resume accepted", change, observed, err)
			}
			prelaunch := change == "expired deadline" || change == "account drift" || change == "project drift" || change == "wrong native" || change == "wrong verifier" || change == "missing stop receipt" || change == "live controller"
			if prelaunch && !errors.Is(err, ports.ErrChatResumeNotLaunched) {
				t.Fatal("pure prelaunch failure lost its no-launch evidence", change, err)
			}
			if !prelaunch && errors.Is(err, ports.ErrChatResumeNotLaunched) {
				t.Fatal("unknown/provider failure was misclassified as not launched", change, err)
			}
			wantStarts := beforeStarts
			if change == "resume" || change == "history failure" || change == "owner rotated after ready" {
				wantStarts++
			}
			if len(chat.started) != wantStarts || store.creates != beforeSeeds || len(chat.turns) != 0 || len(chat.queued) != 0 || len(chat.stopped) != 0 {
				t.Fatal("resume spawned/sent/stopped unexpectedly", change, len(chat.started), wantStarts)
			}
		})
	}
}
