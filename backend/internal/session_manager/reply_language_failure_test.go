package sessionmanager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestReplyLanguageReadFailureKeepsSessionPrompt(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []domain.SessionKind{domain.KindWorker, domain.KindOrchestrator} {
		for _, chat := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/chat=%t", kind, chat), func(t *testing.T) {
				store := newFakeStore()
				store.projects["mer"] = domain.ProjectRecord{ID: "mer", Path: t.TempDir()}
				source := &replyLanguageSource{}
				var logs bytes.Buffer
				manager := New(Deps{Store: store, ReplyLanguage: source, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
				baseline, err := manager.buildSystemPrompt(ctx, kind, "mer", "mer-1", "", chat)
				if err != nil || baseline == "" {
					t.Fatalf("baseline prompt: %q, %v", baseline, err)
				}
				// A failed read can return a partial value; it must not become instructions.
				source.language = "zh-TW"
				source.err = errors.New("settings unavailable")
				got, err := manager.buildSystemPrompt(ctx, kind, "mer", "mer-1", "", chat)
				if err != nil {
					t.Fatalf("preference read failure blocked session prompt: %v", err)
				}
				if got != baseline {
					t.Fatal("preference read failure changed the disabled prompt")
				}
				for _, want := range []string{"level=WARN", "reply language", "settings unavailable", "mer-1"} {
					if !strings.Contains(logs.String(), want) {
						t.Errorf("warning missing %q: %s", want, &logs)
					}
				}
				source.err = nil
				recovered, err := manager.buildSystemPrompt(ctx, kind, "mer", "mer-1", "", chat)
				if err != nil || !strings.Contains(recovered, "Traditional Chinese (zh-TW)") {
					t.Fatalf("next prompt did not recover the preference: %v", err)
				}
				projectErr := errors.New("project unavailable")
				store.getProjectErr = projectErr
				if _, err := manager.buildSystemPrompt(ctx, kind, "mer", "mer-1", "", chat); !errors.Is(err, projectErr) {
					t.Fatalf("unrelated project error lost: %v", err)
				}
			})
		}
	}
}

type replyLanguageSourceFunc func(context.Context) (string, error)

func (f replyLanguageSourceFunc) ReplyLanguage(ctx context.Context) (string, error) { return f(ctx) }

func TestReplyLanguageReadFailurePreservesSessionCancellation(t *testing.T) {
	for _, kind := range []domain.SessionKind{domain.KindWorker, domain.KindOrchestrator} {
		for _, chat := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/chat=%t", kind, chat), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				store := newFakeStore()
				store.projects["mer"] = domain.ProjectRecord{ID: "mer", Path: t.TempDir()}
				source := replyLanguageSourceFunc(func(context.Context) (string, error) { cancel(); return "zh-TW", errors.New("settings unavailable") })
				manager := New(Deps{Store: store, ReplyLanguage: source})
				prompt, err := manager.buildSystemPrompt(ctx, kind, "mer", "mer-1", "", chat)
				if !errors.Is(err, context.Canceled) || prompt != "" {
					t.Fatalf("caller cancellation lost: empty prompt=%t, err=%v", prompt == "", err)
				}
			})
		}
	}
}
