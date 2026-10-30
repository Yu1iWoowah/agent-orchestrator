package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func replacementControllerFixture(t *testing.T, dir string) (*sqlite.Store, domain.OrchestratorReplacement, domain.SessionRecord, domain.SessionRecord) {
	t.Helper()
	store, op, seed := replacementSpawnIntent(t, dir)
	ctx := context.Background()
	created, _, err := store.CreateClientRequestSession(ctx, seed)
	if err != nil {
		t.Fatal(err)
	}
	if applied, err := store.UpdateBrowserCapabilityVerifier(ctx, created.ID, created.ControllerOwner(), "prepared-target-verifier"); err != nil || !applied {
		t.Fatal(applied, err)
	}
	before, _, err := store.GetSession(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	ready := before
	ready.Metadata.ControllerGeneration, ready.Metadata.ProviderConversationID = "target-generation", "target-native-root"
	ready.UpdatedAt = time.Now().UTC()
	op, _, err = store.GetOrchestratorReplacement(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	return store, op, before, ready
}

func TestReplacementSuccessorControllerReceiptCommitsWithOwner(t *testing.T) {
	for _, path := range []string{"session", "provider boundary"} {
		t.Run(path, func(t *testing.T) {
			for _, change := range []string{"valid", "wrong verifier", "source native reused", "receipt ENOSPC"} {
				t.Run(change, func(t *testing.T) {
					dir := t.TempDir()
					store, op, before, ready := replacementControllerFixture(t, dir)
					ctx := context.Background()
					var boundary domain.ConversationBranch
					var parent domain.ConversationRecord
					if path == "provider boundary" {
						var err error
						parent, err = store.CreateConversation(ctx, "target-conversation", domain.ConversationScopeSession, op.ProjectID, ready.ID, time.Now())
						if err != nil {
							t.Fatal(err)
						}
						boundary = domain.ConversationBranch{ID: "target-branch", ConversationID: parent.ID, SessionID: ready.ID, ParentBranchID: parent.ActiveBranchID, ProviderScopeID: "target-branch", Strategy: domain.ConversationBranchStrategyNative, CreatedAt: ready.UpdatedAt}
					}
					if change == "wrong verifier" {
						ready.Metadata.BrowserCapabilityVerifier = "different-verifier"
					}
					if change == "source native reused" {
						ready.Metadata.ProviderConversationID = op.Transcript.NativeCutoffs[0].NativeID
					}
					if change == "receipt ENOSPC" {
						raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ao.db"))
						if err != nil {
							t.Fatal(err)
						}
						defer raw.Close()
						if _, err := raw.ExecContext(ctx, `CREATE TRIGGER reject_successor_controller_receipt BEFORE UPDATE ON orchestrator_replacements WHEN json_type(NEW.state_json,'$.successorController') IS 'object' BEGIN SELECT RAISE(ABORT,'receipt ENOSPC'); END`); err != nil {
							t.Fatal(err)
						}
					}
					var err error
					if path == "session" {
						err = store.UpdateSession(ctx, ready)
					} else {
						boundary.ProviderConversationID = ready.Metadata.ProviderConversationID
						err = store.CommitChatSpawn(ctx, ready, boundary)
					}
					current, _, readErr := store.GetSession(ctx, ready.ID)
					if readErr != nil {
						t.Fatal(readErr)
					}
					stored, _, readErr := store.GetOrchestratorReplacement(ctx, op.ID)
					if readErr != nil {
						t.Fatal(readErr)
					}
					if change == "valid" {
						proof := stored.SuccessorController
						if err != nil || proof == nil || proof.Owner != ready.ControllerOwner() || proof.CapabilityVerifier != ready.Metadata.BrowserCapabilityVerifier || !proof.CommittedAt.Equal(ready.UpdatedAt) || current.ControllerOwner() != proof.Owner || stored.Revision != op.Revision+1 {
							t.Fatalf("owner returned before receipt: err=%v proof=%+v revision=%d before=%d", err, proof, stored.Revision, op.Revision)
						}
						changed := *proof
						changed.CapabilityVerifier = "rewritten"
						stored.SuccessorController = &changed
						if saved, err := store.SaveOrchestratorReplacement(ctx, stored); saved || !errors.Is(err, domain.ErrReplacementStateInvalid) {
							t.Fatal("generic Save rewrote native controller receipt", saved, err)
						}
					} else {
						if err == nil || stored.SuccessorController != nil || stored.Revision != op.Revision || !reflect.DeepEqual(current, before) {
							t.Fatalf("%s left partial owner/receipt: err=%v receipt=%+v owner=%+v before=%+v", change, err, stored.SuccessorController, current.ControllerOwner(), before.ControllerOwner())
						}
						if path == "provider boundary" {
							conv, err := store.ConversationForSession(ctx, ready.ID)
							if err != nil || conv.ActiveBranchID != parent.ActiveBranchID {
								t.Fatal("failed receipt advanced branch", err)
							}
						}
					}
				})
			}
		})
	}
}

func TestReplacementSuccessorControllerReceiptCannotBeAssignedByGenericSave(t *testing.T) {
	store, op, before, ready := replacementControllerFixture(t, t.TempDir())
	op.SuccessorController = &domain.ReplacementSuccessorController{Owner: ready.ControllerOwner(), CapabilityVerifier: ready.Metadata.BrowserCapabilityVerifier, CommittedAt: ready.UpdatedAt}
	if saved, err := store.SaveOrchestratorReplacement(context.Background(), op); saved || !errors.Is(err, domain.ErrReplacementStateInvalid) {
		t.Fatal("generic Save manufactured controller authority", saved, err)
	}
	after, _, err := store.GetSession(context.Background(), ready.ID)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatal("rejected receipt changed native owner", err)
	}
}

// Audit 78 I1: both native publication paths must reject unsealed raw mutations.
func TestReplacementControllerBindRejectsUnsealedState(t *testing.T) {
	for _, boundary := range []bool{false, true} {
		t.Run(map[bool]string{false: "session", true: "provider boundary"}[boundary], func(t *testing.T) {
			dir := t.TempDir()
			store, op, before, ready := replacementControllerFixture(t, dir)
			ctx := context.Background()
			var branch domain.ConversationBranch
			if boundary {
				conv, err := store.CreateConversation(ctx, "unsealed-target", domain.ConversationScopeSession, op.ProjectID, ready.ID, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				branch = domain.ConversationBranch{ID: "unsealed-branch", ConversationID: conv.ID, SessionID: ready.ID, ParentBranchID: conv.ActiveBranchID, ProviderScopeID: "unsealed-branch", ProviderConversationID: ready.Metadata.ProviderConversationID, Strategy: domain.ConversationBranchStrategyNative, CreatedAt: ready.UpdatedAt}
			}
			raw, err := sql.Open("sqlite", filepath.Join(dir, "ao.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			if _, err := raw.ExecContext(ctx, `UPDATE orchestrator_replacements SET state_json=json_set(state_json,'$.error',json('{"code":"UNSEALED","message":"not a managed write"}')) WHERE id=?`, op.ID); err != nil {
				t.Fatal(err)
			}
			if boundary {
				err = store.CommitChatSpawn(ctx, ready, branch)
			} else {
				err = store.UpdateSession(ctx, ready)
			}
			if !errors.Is(err, domain.ErrReplacementRepairUnverified) {
				t.Fatal("controller publication laundered unsealed operation", err)
			}
			current, _, err := store.GetSession(ctx, ready.ID)
			if err != nil || !reflect.DeepEqual(current, before) {
				t.Fatal("unsealed operation published a native owner", err)
			}
			if _, _, err := store.GetOrchestratorReplacement(ctx, op.ID); !errors.Is(err, domain.ErrReplacementRepairUnverified) {
				t.Fatal("corruption was sealed as valid history", err)
			}
		})
	}
}

func TestReplacementControllerBindIgnoresUnrelatedSessionWithUnknownOperationFields(t *testing.T) {
	dir := t.TempDir()
	store, op, _, _ := replacementControllerFixture(t, dir)
	ctx := context.Background()
	worker, err := store.CreateSession(ctx, domain.SessionRecord{ProjectID: op.ProjectID, Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat, CreatedAt: time.Now(), UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	conv, err := store.CreateConversation(ctx, "unrelated-worker", domain.ConversationScopeSession, op.ProjectID, worker.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(dir, "ao.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, `UPDATE orchestrator_replacements SET state_json=json_set(state_json,'$.futureNativeStop',true) WHERE id=?`, op.ID); err != nil {
		t.Fatal(err)
	}
	worker.Metadata.ControllerGeneration, worker.Metadata.ProviderConversationID = "worker-generation", "worker-native"
	branch := domain.ConversationBranch{ID: "unrelated-worker-branch", ConversationID: conv.ID, SessionID: worker.ID, ParentBranchID: conv.ActiveBranchID, ProviderScopeID: "unrelated-worker-branch", ProviderConversationID: worker.Metadata.ProviderConversationID, Strategy: domain.ConversationBranchStrategyNative, CreatedAt: time.Now()}
	if err := store.CommitChatSpawn(ctx, worker, branch); err != nil {
		t.Fatal("unknown operation field blocked unrelated controller update", err)
	}
	if _, _, err := store.GetOrchestratorReplacement(ctx, op.ID); err == nil {
		t.Fatal("unrelated update must not repair operation")
	}
}
