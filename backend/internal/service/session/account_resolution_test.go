package session

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestResolvedDeviceSpawnKeepsLegacyReadinessGate(t *testing.T) {
	for _, harness := range []domain.AgentHarness{domain.HarnessClaudeCode, domain.HarnessCodex} {
		for _, state := range []domain.AgentAuthenticationState{domain.AgentAuthenticationUnauthorized, domain.AgentAuthenticationUnknown} {
			st := newFakeStore()
			st.projects["mer"] = domain.ProjectRecord{ID: "mer"}
			commander := &fakeCommander{}
			readiness := &fakeAgentReadiness{snapshot: domain.AgentReadinessSnapshot{ID: string(harness), Installation: domain.AgentInstallationObservation{State: domain.AgentInstallationInstalled}, Authentication: domain.AgentAuthenticationObservation{State: state, Freshness: domain.AgentReadinessFresh}}}
			svc := NewWithDeps(Deps{Store: st, Manager: commander, AgentReadiness: readiness})
			svc.SetAccountResolver(accountResolutionFunc(func(_ context.Context, _ ports.AgentAccountResolutionRequest) (domain.ResolvedAgentAccount, error) {
				p, _ := domain.AgentAccountProviderFor(harness)
				return domain.ResolvedAgentAccount{AccountID: domain.DeviceAgentAccountID(p), Provider: p, Source: domain.AgentAccountSourceDevice}, nil
			}))
			_, _, _, err := svc.Spawn(context.Background(), ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker, Harness: harness})
			if readiness.calls != 1 {
				t.Fatalf("device skipped legacy readiness: %s %s", harness, state)
			}
			if harness == domain.HarnessCodex && state == domain.AgentAuthenticationUnauthorized {
				var apiError *apierr.Error
				if !errors.As(err, &apiError) || apiError.Code != "CODEX_ACCOUNT_AUTH_UNVERIFIED" || commander.spawned {
					t.Fatalf("device Codex gate changed: %v", err)
				}
			} else if err != nil || !commander.spawned {
				t.Fatalf("legacy device spawn blocked: %s %s %v", harness, state, err)
			}
		}
	}
}

type accountBoundSessionStore struct {
	*fakeStore
	binding string
}

func (s *accountBoundSessionStore) GetSessionAgentAccount(_ context.Context, _ domain.SessionID, _ domain.AgentAccountProvider) (string, bool, error) {
	return s.binding, s.binding != "", nil
}

type accountResolutionFunc func(context.Context, ports.AgentAccountResolutionRequest) (domain.ResolvedAgentAccount, error)

func (f accountResolutionFunc) ResolveAccount(ctx context.Context, in ports.AgentAccountResolutionRequest) (domain.ResolvedAgentAccount, error) {
	return f(ctx, in)
}
func TestExistingOrchestratorAccountOverrideIsIdempotentOnlyForItsBinding(t *testing.T) {
	for _, viaSpawn := range []bool{false, true} {
		for _, id := range []string{"", "A", "B"} {
			st := &accountBoundSessionStore{fakeStore: newFakeStore(), binding: "A"}
			st.projects["mer"] = domain.ProjectRecord{ID: "mer"}
			st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode}
			fc := &fakeCommander{}
			svc := &Service{store: st, manager: fc}
			var got domain.Session
			var err error
			if viaSpawn {
				got, _, _, err = svc.Spawn(context.Background(), ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindOrchestrator, AgentAccountID: id})
			} else {
				got, err = svc.SpawnOrchestrator(context.Background(), "mer", false, "", "", id)
			}
			if id == "B" {
				if err == nil {
					t.Fatal("different override reused existing account")
				}
			} else if err != nil || got.ID != "mer-1" {
				t.Fatalf("same/omitted override: %+v %v", got, err)
			}
			if fc.spawned || len(fc.retired) != 0 {
				t.Fatal("idempotent request changed runtime")
			}
		}
	}
}
func TestInvalidCleanOrchestratorAccountDoesNotRetireSource(t *testing.T) {
	st := newFakeStore()
	st.projects["mer"] = domain.ProjectRecord{ID: "mer", Config: domain.ProjectConfig{Orchestrator: domain.RoleOverride{Harness: domain.HarnessClaudeCode}}}
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode}
	fc := &fakeCommander{}
	svc := &Service{store: st, manager: fc, accountResolver: accountResolutionFunc(func(_ context.Context, in ports.AgentAccountResolutionRequest) (domain.ResolvedAgentAccount, error) {
		if in.AccountID != "missing" || in.Role != domain.AgentAccountRoleOrchestrator {
			t.Fatalf("resolution input: %+v", in)
		}
		return domain.ResolvedAgentAccount{}, ports.ErrAgentAccountUnknown
	})}
	if _, err := svc.SpawnOrchestrator(context.Background(), "mer", true, "", "", "missing"); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	if fc.spawned || len(fc.retired) != 0 {
		t.Fatal("invalid account retired live source")
	}
}

func TestStandaloneSpawnUsesTheSharedAccountResolver(t *testing.T) {
	st := newFakeStore()
	fc := &fakeCommander{}
	selected := domain.ResolvedAgentAccount{AccountID: "standalone-A", Provider: domain.AgentAccountProviderCodex, Source: domain.AgentAccountSourceExplicit}
	svc := &Service{store: st, manager: fc, accountResolver: accountResolutionFunc(func(_ context.Context, in ports.AgentAccountResolutionRequest) (domain.ResolvedAgentAccount, error) {
		if in.Harness != domain.HarnessCodex || in.Role != domain.AgentAccountRoleWorker || in.AccountID != "standalone-A" || in.ProjectConfig.HasLocalAccountReferences() {
			t.Fatalf("standalone selection inherited a project: %+v", in)
		}
		return selected, nil
	})}
	if _, _, _, err := svc.Spawn(context.Background(), ports.SpawnConfig{Kind: domain.KindWorker, Harness: domain.HarnessCodex, AgentAccountID: "standalone-A"}); err != nil {
		t.Fatal(err)
	}
	if !fc.spawned || fc.spawnedCfg.ResolvedAccount == nil || *fc.spawnedCfg.ResolvedAccount != selected {
		t.Fatalf("standalone choice lost before manager: %+v", fc.spawnedCfg)
	}
}
