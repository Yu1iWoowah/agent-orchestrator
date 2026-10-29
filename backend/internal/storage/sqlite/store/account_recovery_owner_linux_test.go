package store_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/adapters/agentaccount"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentaccounts"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/shellterm"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type recoveredOwnerCLI struct{ agentaccount.Claude }

func (recoveredOwnerCLI) Identity(_ context.Context, home string) (domain.AgentAccountIdentityCheck, error) {
	return domain.AgentAccountIdentityCheck{SignedIn: home != "", LoggedIn: home != "", Email: filepath.Base(home) + "@example.test"}, nil
}
func (recoveredOwnerCLI) Limits(context.Context, string) (domain.AgentAccountLimits, error) {
	return domain.AgentAccountLimits{}, nil
}
func (recoveredOwnerCLI) Logout(context.Context, string) error { return nil }
func (recoveredOwnerCLI) LoginArgv(context.Context) ([]string, error) {
	return []string{"/bin/true"}, nil
}

type recoveredOwnerTerminals struct{}

func (recoveredOwnerTerminals) OpenCommandTerminal(_ context.Context, in shellterm.OpenCommandTerminalInput) (shellterm.ShellTerminal, error) {
	return shellterm.ShellTerminal{HandleID: in.HandleID}, nil
}
func (recoveredOwnerTerminals) CloseCommandTerminal(context.Context, string) error { return nil }
func (recoveredOwnerTerminals) CommandTerminalAlive(context.Context, string) (bool, error) {
	return true, nil
}

func TestRecoveredTransitionOwnersPermitAccountLifecycle(t *testing.T) {
	for _, scenario := range []string{"committed", "terminated", "legacy", "superseded"} {
		for _, action := range []string{"remove", "login-again"} {
			t.Run(scenario+"/"+action, func(t *testing.T) {
				ctx := context.Background()
				dir := t.TempDir()
				s := sqlitetest.MustOpenAt(t, dir)
				root := filepath.Join(t.TempDir(), "accounts")
				mustNoError(t, os.MkdirAll(root, 0700))
				svc := agentaccounts.New(s, []ports.AgentAccountProviderCLI{&recoveredOwnerCLI{}}, root, recoveredOwnerTerminals{}, nil)
				ids := []string{}
				for range 2 {
					login, err := svc.StartLogin(ctx, domain.AgentAccountProviderClaude)
					mustNoError(t, err)
					_, _, err = svc.VerifyLogin(ctx, login.OperationID)
					mustNoError(t, err)
					ids = append(ids, login.OperationID)
				}
				worker := seedUsageSession(t, s, domain.HarnessClaudeCode)
				bound := ids[0]
				if scenario == "committed" {
					bound = ids[1]
				}
				now := time.Now().UTC()
				mustNoError(t, s.BindSessionAgentAccount(ctx, worker.ID, domain.AgentAccountProviderClaude, bound, now))
				move, _, err := s.CreateSessionInterfaceTransition(ctx, domain.SessionInterfaceTransition{ID: "recover-owner", SessionID: worker.ID, SourceMode: domain.SessionModeTUI, TargetMode: domain.SessionModeTUI, HistoryPolicy: domain.SessionInterfaceTransitionHistoryStrict, Policy: domain.SessionInterfaceTransitionDrain, Phase: domain.SessionInterfaceTransitionTargetStarting, SourceAccountID: ids[0], TargetAccountID: ids[1], CreatedAt: now, UpdatedAt: now})
				mustNoError(t, err)
				changed, err := s.RecordAccountTransitionRuntime(ctx, move.ID, domain.AccountTransitionRuntimeReserved, "generation", "", now)
				mustNoError(t, err)
				if !changed {
					t.Fatal("reserve failed")
				}
				changed, err = s.RecordAccountTransitionRuntime(ctx, move.ID, domain.AccountTransitionRuntimeStarted, "generation", "handle", now)
				mustNoError(t, err)
				if !changed {
					t.Fatal("start failed")
				}
				_, err = s.AdvanceSessionInterfaceTransition(ctx, move.ID, domain.SessionInterfaceTransitionTargetStarting, domain.SessionInterfaceTransitionRecovery, "", "DAEMON_RESTARTED", "", now)
				mustNoError(t, err)
				raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "ao.db"))
				mustNoError(t, err)
				defer raw.Close()
				target := ids[0]
				if scenario == "superseded" {
					target = ids[1]
					successor := move
					successor.ID = "successful-successor"
					successor.TargetAccountID = "device-claude"
					successor.Phase = domain.SessionInterfaceTransitionCompleted
					successor.CreatedAt = now.Add(time.Second)
					successor.UpdatedAt = successor.CreatedAt
					_, _, err = s.CreateSessionInterfaceTransition(ctx, successor)
					mustNoError(t, err)
					bound = "device-claude"
					mustNoError(t, s.BindSessionAgentAccount(ctx, worker.ID, domain.AgentAccountProviderClaude, bound, now.Add(time.Second)))
				} else if scenario != "committed" {
					target = ids[1]
					if scenario == "terminated" {
						_, err = raw.ExecContext(ctx, "UPDATE sessions SET is_terminated=1 WHERE id=?", worker.ID)
						mustNoError(t, err)
					}
					launch, handle := "generation", "handle"
					if scenario == "legacy" {
						_, err = raw.ExecContext(ctx, "UPDATE session_interface_transitions SET target_runtime_state='',target_runtime_launch_id='',target_runtime_handle_id='' WHERE id=?", move.ID)
						mustNoError(t, err)
						launch, handle = "", ""
					}
					owners, err := s.ListAgentAccountUsageOwners(ctx, target)
					mustNoError(t, err)
					if scenario != "legacy" && (len(owners) != 1 || !owners[0].Blocking) {
						t.Fatalf("unconfirmed target lost its owner: %+v", owners)
					}
					if scenario == "legacy" && len(owners) != 0 {
						t.Fatalf("old daemon's confirmed stop still blocks lifecycle: %+v", owners)
					}
					pending, err := s.ListUnsettledAccountTransitions(ctx)
					mustNoError(t, err)
					if len(pending) != 1 || pending[0].ID != move.ID {
						t.Fatalf("startup cannot rediscover historical obligation: %+v", pending)
					}
					if scenario == "legacy" {
						// The shared view proves that the old daemon confirmed stop;
						// retirement must not stop a subsequently resumed source.
						changed, err = s.RetireAccountTransitionRuntime(ctx, move.ID, now)
					} else {
						changed, err = s.RecordAccountTransitionRuntime(ctx, move.ID, domain.AccountTransitionRuntimeStopped, launch, handle, now)
					}
					mustNoError(t, err)
					if !changed {
						t.Fatal("terminal/legacy shutdown receipt could not converge")
					}
				}
				pending, err := s.ListUnsettledAccountTransitions(ctx)
				mustNoError(t, err)
				wantPending := 0
				if scenario == "superseded" {
					wantPending = 1
				}
				if len(pending) != wantPending {
					t.Fatalf("settled or committed receipt repeatedly recovered: %+v", pending)
				}
				owners, err := s.ListAgentAccountUsageOwners(ctx, target)
				mustNoError(t, err)
				for _, owner := range owners {
					if owner.Blocking {
						t.Fatalf("historical transition permanently owns account: %+v", owner)
					}
				}
				if action == "remove" {
					mustNoError(t, svc.Remove(ctx, target))
				} else {
					_, err = svc.StartSignInAgain(ctx, target)
					mustNoError(t, err)
				}
				if _, found, err := s.GetSession(ctx, worker.ID); err != nil || !found {
					t.Fatalf("session history was deleted: %v", err)
				}
				if id, found, err := s.GetSessionAgentAccount(ctx, worker.ID, domain.AgentAccountProviderClaude); err != nil || !found || id != bound {
					t.Fatalf("binding history changed: %s %v", id, err)
				}
			})
		}
	}
}
