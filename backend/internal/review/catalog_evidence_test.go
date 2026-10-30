package review

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestReviewerStrictCatalogNeedsLastSuccessfulEvidence(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		last *time.Time
		want error
	}{
		{"never succeeded", nil, ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"zero success", new(time.Time{}), ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"future success", new(now.Add(time.Hour)), ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"previous day", new(now.Add(-24 * time.Hour)), ports.ErrAgentSettingsCapabilitiesUnavailable},
		{"current success", &now, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := ports.AgentModelCatalog{InputFingerprint: "current-v1", FetchedAt: now, ValidatedAt: now, LastSuccessAt: tc.last, RefreshState: "idle", Models: []ports.AgentModelInfo{{ID: "review-model", IsDefault: true, SupportsFast: true}}}
			e := &Engine{models: fixedReviewerCatalog{catalog: catalog}}
			_, err := e.reviewerModelProof(context.Background(), domain.ReviewerCodex, domain.AgentConfig{Model: "review-model", FastMode: domain.AgentFastOn}, true)
			if !errors.Is(err, tc.want) {
				t.Fatalf("unqualified catalog accepted: %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReviewerCatalogUsesOwnerCalendarDayRule(t *testing.T) {
	zone := time.FixedZone("Taipei", 8*60*60)
	for _, tc := range []struct {
		name      string
		now, last time.Time
		stale     bool
	}{
		{"same day beyond six hours", time.Date(2026, 10, 31, 18, 0, 0, 0, zone), time.Date(2026, 10, 31, 9, 0, 0, 0, zone), false},
		{"same instant across zones", time.Date(2026, 10, 31, 0, 10, 0, 0, zone), time.Date(2026, 10, 30, 16, 5, 0, 0, time.UTC), false},
		{"previous local day", time.Date(2026, 10, 31, 0, 10, 0, 0, zone), time.Date(2026, 10, 30, 15, 55, 0, 0, time.UTC), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := ports.AgentModelCatalog{LastSuccessAt: &tc.last, Models: []ports.AgentModelInfo{{ID: "review-model", SupportsFast: true}}}
			err := validateReviewerCatalog(catalog, domain.AgentConfig{Model: "review-model", FastMode: domain.AgentFastOn}, true, tc.now)
			if (err != nil) != tc.stale {
				t.Fatalf("calendar evidence: %v, stale=%v", err, tc.stale)
			}
		})
	}
}
