package agentsettings

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type spawnObservationStore struct {
	catalogEvidenceStore
	pendingCalls int
	failPending  bool
}

func (s *spawnObservationStore) PendingAgentSettingsOperation(context.Context, domain.SessionID) (domain.AgentSettingsOperation, bool, error) {
	s.pendingCalls++
	if s.failPending {
		return domain.AgentSettingsOperation{}, false, errors.New("pending-decode-injected")
	}
	return domain.AgentSettingsOperation{}, false, nil
}

type countingObservationSource struct{ calls int }

func (s *countingObservationSource) ObserveAgentSettings(context.Context, domain.SessionID) (domain.AgentSettingsObservation, error) {
	s.calls++
	return domain.AgentSettingsObservation{}, nil
}

func TestSpawnValidationDoesNotReadObservationState(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "observer untouched", true: "invalid pending ignored"}[fail], func(t *testing.T) {
			now := time.Now()
			raw, _ := json.Marshal(ports.AgentModelCatalog{AgentID: "codex", Models: []ports.AgentModelInfo{{ID: "native", SupportsFast: true}}})
			store := &spawnObservationStore{catalogEvidenceStore: catalogEvidenceStore{settings: domain.SessionAgentSettings{SessionID: "s", Harness: domain.HarnessCodex, AccountID: "device-codex"}, catalog: ports.CachedAgentModelCatalog{AgentID: "codex", CatalogJSON: string(raw), LastSuccessAt: now, RefreshState: "idle", InputFingerprint: "stable"}}, failPending: fail}
			observer := &countingObservationSource{}
			svc := New(store, func() time.Time { return now })
			svc.SetCatalogFingerprintSource(fixedCatalogFingerprint("stable"))
			svc.SetObservationSource(observer)
			if _, err := svc.ValidateSpawn(t.Context(), "s", domain.AgentSettings{Model: "native", FastMode: domain.AgentFastOn}, false); err != nil {
				t.Fatalf("unrelated observation state blocked spawn: %v", err)
			}
			if store.pendingCalls != 0 || observer.calls != 0 {
				t.Fatalf("spawn read pending=%d observer=%d", store.pendingCalls, observer.calls)
			}
		})
	}
}

func TestUnboundACPObservationReportsAccountUnbound(t *testing.T) {
	store := catalogEvidenceStore{settings: domain.SessionAgentSettings{SessionID: "s", Harness: domain.HarnessOpenCode, OwnerGeneration: "g", ProviderConversationID: "native", BranchID: "branch"}}
	observer := &countingObservationSource{}
	svc := New(store, time.Now)
	svc.SetObservationSource(observer)
	got, err := svc.Read(t.Context(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if got.Effective.FastMode.State != domain.AgentSettingUnknown || got.Effective.FastMode.Reason != "account_unbound" {
		t.Fatalf("unbound ACP reason is imprecise: %+v", got.Effective)
	}
}
