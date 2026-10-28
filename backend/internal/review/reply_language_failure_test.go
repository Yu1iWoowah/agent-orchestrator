package review

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestReviewerReplyLanguageReadFailureDoesNotBlockLaunch(t *testing.T) {
	for _, restore := range []bool{false, true} {
		operation := "Spawn"
		if restore {
			operation = "RestoreTerminal"
		}
		for _, runtimeFailure := range []bool{false, true} {
			name := operation + "/runtime-success"
			if runtimeFailure {
				name = operation + "/runtime-failure"
			}
			t.Run(name, func(t *testing.T) {
				var logs bytes.Buffer
				previousLogger := slog.Default()
				slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
				t.Cleanup(func() { slog.SetDefault(previousLogger) })
				runtime := &fakeRuntime{}
				if runtimeFailure {
					runtime.createErr = errors.New("runtime unavailable")
				}
				reviewer := &fakeReviewer{}
				source := &replyLanguageSource{language: "zh-TW", err: errors.New("settings unavailable")}
				launcher := NewLauncher(fakeReviewerResolver{reviewer: reviewer, ok: true}, runtime, t.TempDir(), WithReplyLanguage(source))
				spec := launchSpec()
				spec.ReplyLanguage = "en" // Failed reads must clear stale launch preferences too.
				var err error
				if restore {
					_, err = launcher.RestoreTerminal(context.Background(), spec)
				} else {
					_, err = launcher.Spawn(context.Background(), spec)
				}
				if !runtime.created {
					t.Fatalf("preference read failure prevented runtime creation: %v", err)
				}
				if runtimeFailure {
					if err == nil || !strings.Contains(err.Error(), "runtime unavailable") {
						t.Fatalf("unrelated runtime error lost: %v", err)
					}
				} else if err != nil {
					t.Fatalf("preference read failure blocked reviewer: %v", err)
				}
				raw, readErr := os.ReadFile(reviewer.gotInv.SystemPromptFile)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if !strings.Contains(string(raw), "Code reviewer role") || strings.Contains(string(raw), "Agent Reply Language") {
					t.Fatal("failed preference read changed the reviewer role or injected language")
				}
				for _, want := range []string{"level=WARN", "reply language", "settings unavailable", "mer-1"} {
					if !strings.Contains(logs.String(), want) {
						t.Errorf("warning missing %q: %s", want, &logs)
					}
				}
				source.err = nil
				runtime.createErr = nil
				if restore {
					_, err = launcher.RestoreTerminal(context.Background(), spec)
				} else {
					_, err = launcher.Spawn(context.Background(), spec)
				}
				if err != nil {
					t.Fatalf("next launch did not recover: %v", err)
				}
				raw, readErr = os.ReadFile(reviewer.gotInv.SystemPromptFile)
				if readErr != nil || !strings.Contains(string(raw), "Traditional Chinese (zh-TW)") {
					t.Fatalf("next launch did not reload the preference: %v", readErr)
				}
			})
		}
	}
}

type replyLanguageSourceFunc func(context.Context) (string, error)

func (f replyLanguageSourceFunc) ReplyLanguage(ctx context.Context) (string, error) { return f(ctx) }

func TestReviewerReplyLanguageReadFailurePreservesCancellation(t *testing.T) {
	for _, restore := range []bool{false, true} {
		operation := "Spawn"
		if restore {
			operation = "RestoreTerminal"
		}
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source := replyLanguageSourceFunc(func(context.Context) (string, error) { cancel(); return "zh-TW", errors.New("settings unavailable") })
			runtime := &fakeRuntime{}
			launcher := NewLauncher(fakeReviewerResolver{reviewer: &fakeReviewer{}, ok: true}, runtime, t.TempDir(), WithReplyLanguage(source))
			var err error
			if restore {
				_, err = launcher.RestoreTerminal(ctx, launchSpec())
			} else {
				_, err = launcher.Spawn(ctx, launchSpec())
			}
			if !errors.Is(err, context.Canceled) || runtime.created {
				t.Fatalf("caller cancellation lost: runtime created=%t, err=%v", runtime.created, err)
			}
		})
	}
}
