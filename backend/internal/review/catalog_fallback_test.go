package review

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type lastKnownGoodReviewerModels struct {
	catalog      ports.AgentModelCatalog
	refreshes    []bool
	fingerprints []string
	accountID    string
}

func (m *lastKnownGoodReviewerModels) CatalogFingerprint(ctx context.Context, _, scope string) (string, error) {
	if m.accountID != "" {
		account, ok := ports.AgentAccountFromContext(ctx)
		if !ok || account.AccountID != m.accountID || scope != ports.AccountModelCatalogScope(m.accountID) {
			return "", ports.ErrAgentAccountChanged
		}
	}
	if len(m.fingerprints) > 0 {
		fingerprint := m.fingerprints[0]
		m.fingerprints = m.fingerprints[1:]
		return fingerprint, nil
	}
	return "saved-fingerprint", nil
}

func (m *lastKnownGoodReviewerModels) ModelCatalog(_ context.Context, _, _ string, refresh bool) (ports.AgentModelCatalog, error) {
	m.refreshes = append(m.refreshes, refresh)
	catalog := m.catalog
	catalog.Stale = true
	if refresh {
		catalog.RefreshState = "error"
		catalog.RefreshError = "credential_expired"
	}
	return catalog, nil
}

func TestReviewerLastKnownGoodCatalog(t *testing.T) {
	for _, tc := range []struct {
		harness       domain.ReviewerHarness
		model, effort string
		models        []ports.AgentModelInfo
	}{
		{domain.ReviewerClaudeCode, "", "xhigh", []ports.AgentModelInfo{{ID: "opus", IsDefault: true}, {ID: "claude-opus-5-5", Efforts: []string{"xhigh"}}}},
		{domain.ReviewerCodex, "native-model", "high", []ports.AgentModelInfo{{ID: "native-model", IsDefault: true, Efforts: []string{"high"}}}},
	} {
		for _, supported := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/supported=%t", tc.harness, supported), func(t *testing.T) {
				effort := tc.effort
				if !supported {
					effort = "low"
				}
				config := domain.AgentConfig{Model: tc.model, Effort: effort}
				worker := liveWorker()
				launcher := &fakeLauncher{handle: "review-handle"}
				engine := newEngineForTest(&fakeStore{}, fakeSessions{rec: worker, ok: true}, prAt("sha1"), fakeProjects{cfg: domain.ProjectConfig{Reviewers: []domain.ReviewerConfig{{Harness: tc.harness, AgentConfig: config}}}}, launcher)
				models := &lastKnownGoodReviewerModels{catalog: ports.AgentModelCatalog{LastSuccessAt: new(time.Now()), InputFingerprint: "saved-fingerprint", Models: tc.models}}
				engine.models = models
				_, err := engine.TriggerWithOptions(context.Background(), worker.ID, TriggerOptions{})
				if !slices.Equal(models.refreshes, []bool{false, true}) {
					t.Fatalf("expected cached lookup then refresh: %v", models.refreshes)
				}
				if !supported {
					if !errors.Is(err, ports.ErrAgentSettingsUnsupported) || launcher.spawned {
						t.Fatalf("cached effort validation: err=%v spawned=%v", err, launcher.spawned)
					}
					return
				}
				if err != nil || !launcher.spawned || launcher.gotSpec.AgentConfig.Model != config.Model || launcher.gotSpec.AgentConfig.Effort != config.Effort {
					t.Fatalf("last-known-good launch: err=%v spawned=%v config=%+v", err, launcher.spawned, launcher.gotSpec.AgentConfig)
				}
			})
		}
	}
}

func TestReviewerLastKnownGoodQualification(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		last   *time.Time
		model  string
		effort string
		want   error
	}{
		{"previous day", new(now.Add(-24 * time.Hour)), "opus", "xhigh", nil},
		{"no successful discovery", nil, "opus", "xhigh", ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"zero successful discovery", new(time.Time{}), "opus", "xhigh", ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"future successful discovery", new(now.Add(time.Hour)), "opus", "xhigh", ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"missing model despite allow custom", &now, "missing", "", ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"unsupported effort", &now, "opus", "low", ports.ErrAgentSettingsUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			models := &lastKnownGoodReviewerModels{catalog: ports.AgentModelCatalog{
				LastSuccessAt: tc.last, InputFingerprint: "saved-fingerprint", AllowCustom: true,
				Models: []ports.AgentModelInfo{{ID: "opus", IsDefault: true}, {ID: "claude-opus-5-5", Efforts: []string{"xhigh"}}},
			}}
			engine := &Engine{models: models}
			_, err := engine.reviewerModelProof(context.Background(), domain.ReviewerClaudeCode, domain.AgentConfig{Model: tc.model, Effort: tc.effort}, reviewerCatalogAdmission)
			if !errors.Is(err, tc.want) || !slices.Equal(models.refreshes, []bool{false, true}) {
				t.Fatalf("LKG qualification: err=%v want=%v lookups=%v", err, tc.want, models.refreshes)
			}
		})
	}
}

func TestReviewerLastKnownGoodPreservesProofBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		mutation             bool
		fast                 domain.AgentFastMode
		fingerprint          string
		changedFingerprint   bool
		missingAliasEvidence bool
		accountID            string
		want                 error
	}{
		{name: "fixed reviewer account", accountID: "reviewer-A"},
		{name: "wrong account", accountID: "reviewer-B", want: ports.ErrAgentSettingsCapabilitiesUnavailable},
		{name: "different inputs", fingerprint: "previous-input", want: ports.ErrAgentSettingsCapabilitiesUnavailable},
		{name: "inputs changed during lookup", changedFingerprint: true, want: ports.ErrAgentSettingsCapabilitiesUnavailable},
		{name: "unknown alias effort", missingAliasEvidence: true, want: ports.ErrAgentSettingsCapabilitiesUnavailable},
		{name: "strict settings mutation", mutation: true, want: ports.ErrAgentSettingsCapabilitiesUnavailable},
		{name: "explicit fast", fast: domain.AgentFastOff, want: ports.ErrAgentSettingsCapabilitiesUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			models := &lastKnownGoodReviewerModels{accountID: tc.accountID, catalog: ports.AgentModelCatalog{
				LastSuccessAt: new(time.Now().Add(-24 * time.Hour)), InputFingerprint: "saved-fingerprint",
				Models: []ports.AgentModelInfo{{ID: "opus", IsDefault: true}, {ID: "claude-opus-5-5", Efforts: []string{"xhigh"}}},
			}}
			if tc.fingerprint != "" {
				models.catalog.InputFingerprint = tc.fingerprint
			}
			if tc.changedFingerprint {
				models.fingerprints = []string{"saved-fingerprint", "changed-input"}
			}
			if tc.missingAliasEvidence {
				models.catalog.Models = models.catalog.Models[:1]
			}
			ctx := ports.WithAgentAccount(context.Background(), domain.ResolvedAgentAccount{AccountID: "reviewer-A", Provider: domain.AgentAccountProviderClaude, Source: domain.AgentAccountSourceRole})
			purpose := reviewerCatalogAdmission
			if tc.mutation {
				purpose = reviewerCatalogMutation
			}
			_, err := (&Engine{models: models}).reviewerModelProof(ctx, domain.ReviewerClaudeCode, domain.AgentConfig{Model: "opus", Effort: "xhigh", FastMode: tc.fast}, purpose)
			if !errors.Is(err, tc.want) {
				t.Fatalf("LKG boundary: got %v, want %v", err, tc.want)
			}
		})
	}
}
