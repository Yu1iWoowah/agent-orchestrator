package sessionmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
	sqlitestore "github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// Exercise the real journal writer while retaining the controller fixture's
// session/lifecycle fakes. Admission and every phase write go to SQLite too.
type accountPrivacyJournal struct {
	*transitionStore
	db *sqlitestore.Store
}

func (s accountPrivacyJournal) CreateSessionInterfaceTransition(ctx context.Context, rec domain.SessionInterfaceTransition) (domain.SessionInterfaceTransition, bool, error) {
	if _, _, err := s.db.CreateSessionInterfaceTransition(ctx, rec); err != nil {
		return domain.SessionInterfaceTransition{}, false, err
	}
	return s.transitionStore.CreateSessionInterfaceTransition(ctx, rec)
}

func (s accountPrivacyJournal) AdvanceSessionInterfaceTransition(ctx context.Context, id string, expected, next domain.SessionInterfaceTransitionPhase, nativeID, code, detail string, at time.Time) (bool, error) {
	if changed, err := s.db.AdvanceSessionInterfaceTransition(ctx, id, expected, next, nativeID, code, detail, at); err != nil || !changed {
		return changed, err
	}
	return s.transitionStore.AdvanceSessionInterfaceTransition(ctx, id, expected, next, nativeID, code, detail, at)
}

func TestAccountStopFailureCanary(t *testing.T) {
	for _, stage := range []string{"source-stop", "target-stop", "source-restore"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			m, st, rt, _, accounts, _ := newAccountTransitionManager(t, domain.SessionModeTUI)
			var logs bytes.Buffer
			m.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			marker := errors.New("fake-audit-credential-canary /fake/audit/private/profile")
			wantCode := "SOURCE_STOP_UNCERTAIN"
			switch stage {
			case "source-stop":
				rt.stopErrors = []error{marker, marker}
				rt.aliveByHandle = map[string]bool{"runtime-1": true}
			case "target-stop":
				accounts.bindErr = marker
				rt.stopErrors = []error{nil, marker, marker}
				rt.aliveByHandle = map[string]bool{"h1": true}
				wantCode = "TARGET_STOP_UNCONFIRMED"
			case "source-restore":
				rt.createErrSequence = []error{noEffectAccountRuntimeError{marker}, noEffectAccountRuntimeError{marker}}
				wantCode = "RECOVERY_REQUIRED"
			}
			db := sqlitetest.MustOpen(t)
			now := time.Now().UTC()
			if err := db.UpsertProject(ctx, domain.ProjectRecord{ID: "session", Path: t.TempDir(), RegisteredAt: now}); err != nil {
				t.Fatal(err)
			}
			if rec, err := db.CreateSession(ctx, domain.SessionRecord{ProjectID: "session", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeTUI, CreatedAt: now, UpdatedAt: now}); err != nil || rec.ID != "session-1" {
				t.Fatalf("seed journal session: %s %v", rec.ID, err)
			}
			m.store = accountPrivacyJournal{transitionStore: st, db: db}
			tr, err := m.StartAccountTransition(ctx, "session-1", "acct-b", domain.SessionInterfaceTransitionDrain)
			if err != nil {
				t.Fatal(err)
			}
			// An unconfirmed target deliberately keeps a nonterminal durable fence.
			// Wait for the worker, rather than racing its final write or Debug logger.
			m.transitionMu.Lock()
			run := m.transitions[tr.SessionID]
			m.transitionMu.Unlock()
			if run != nil {
				select {
				case <-run.done:
				case <-time.After(10 * time.Second):
					t.Fatal("transition worker did not finish")
				}
			}
			saved, found, err := db.GetSessionInterfaceTransition(ctx, tr.ID)
			if err != nil || !found || saved.ErrorCode != wantCode {
				t.Fatalf("fault missed %s: %+v %v", stage, saved, err)
			}
			status, err := m.InterfaceTransitionStatus(ctx, tr.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			journalJSON, _ := json.Marshal(saved)
			statusJSON, _ := json.Marshal(status)
			for name, value := range map[string]string{"SQLite journal": string(journalJSON), "status/API input": string(statusJSON), "Debug log": logs.String()} {
				for _, canary := range []string{"fake-audit-credential-canary", "/fake/audit/private/profile"} {
					if strings.Contains(value, canary) {
						t.Errorf("%s leaked into %s at %s", canary, name, stage)
					}
				}
			}
			if saved.ErrorDetail == "" {
				t.Fatal("failure has no actionable safe detail")
			}
			if accounts.boundTo(tr.SessionID) != "device-claude" {
				t.Fatal("failed transition changed binding")
			}
			if stage == "target-stop" && !m.SessionMutationInProgress(tr.SessionID) {
				t.Fatal("unconfirmed target lost its fence")
			}
		})
	}
}
