// Package agentsettings reads durable session preferences without waking an
// agent, refreshing credentials, or treating another account's catalog as proof.
package agentsettings

import (
	"context"
	"encoding/json"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Store reads preference and capability evidence without starting a provider.
type Store interface {
	SessionAgentSettings(context.Context, domain.SessionID) (domain.SessionAgentSettings, error)
	GetAgentModelCatalog(context.Context, string, string) (ports.CachedAgentModelCatalog, bool, error)
}

// CapabilityState records the strength of saved capability evidence.
type CapabilityState string

// Capability evidence states.
const (
	CapabilityUnknown     CapabilityState = "unknown"
	CapabilitySupported   CapabilityState = "supported"
	CapabilityUnsupported CapabilityState = "unsupported"
)

// ModelCapabilities lists advertised choices, never inferred model features.
type ModelCapabilities struct {
	Model     string          `json:"model"`
	IsDefault bool            `json:"isDefault"`
	Efforts   []string        `json:"efforts"`
	Fast      CapabilityState `json:"fast" enum:"unknown,supported,unsupported"`
}

// Capabilities ties saved model evidence to its account and owner context.
type Capabilities struct {
	State                CapabilityState     `json:"state" enum:"unknown,supported,unsupported"`
	Reason               string              `json:"reason,omitempty"`
	Source               string              `json:"source,omitempty"`
	AccountID            string              `json:"accountId,omitempty"`
	ControllerGeneration string              `json:"controllerGeneration"`
	Generation           int64               `json:"generation"`
	ObservedAt           *time.Time          `json:"observedAt,omitempty"`
	Models               []ModelCapabilities `json:"models"`
}

// Snapshot combines durable preferences with independently qualified evidence.
type Snapshot struct {
	domain.SessionAgentSettings
	Capabilities Capabilities `json:"capabilities"`
}

// Service provides read-only session settings.
type Service struct {
	store Store
	now   func() time.Time
}

// New constructs a reader with an injectable freshness clock.
func New(store Store, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, now: now}
}

// Read only consumes saved facts. A managed-account catalog is deliberately
// unknown until U3 supplies an account-scoped catalog contract.
func (s *Service) Read(ctx context.Context, id domain.SessionID) (Snapshot, error) {
	settings, err := s.store.SessionAgentSettings(ctx, id)
	if err != nil {
		return Snapshot{}, err
	}
	result := Snapshot{SessionAgentSettings: settings, Capabilities: Capabilities{State: CapabilityUnknown, Reason: "catalog_unavailable", ControllerGeneration: settings.OwnerGeneration, AccountID: settings.AccountID, Models: []ModelCapabilities{}}}
	if !settings.Harness.IsKnown() {
		result.Capabilities.Reason = "harness_unknown"
		return result, nil
	}
	if provider, ok := domain.AgentAccountProviderFor(settings.Harness); ok {
		if settings.AccountID == "" {
			result.Capabilities.Reason = "account_unbound"
			return result, nil
		}
		if settings.AccountID != domain.DeviceAgentAccountID(provider) {
			result.Capabilities.Reason = "account_catalog_unavailable"
			return result, nil
		}
	}
	cached, found, err := s.store.GetAgentModelCatalog(ctx, string(settings.Harness), "")
	if err != nil {
		return Snapshot{}, err
	}
	if !found {
		return result, nil
	}
	var catalog ports.AgentModelCatalog
	if err = json.Unmarshal([]byte(cached.CatalogJSON), &catalog); err != nil {
		result.Capabilities.Reason = "catalog_invalid"
		return result, nil //nolint:nilerr // An invalid optional cache is unknown evidence, not a failed settings read.
	}
	result.Capabilities.Source = cached.Source
	result.Capabilities.Generation = cached.Generation
	observed := cached.LastSuccessAt
	if !observed.IsZero() {
		result.Capabilities.ObservedAt = &observed
	}
	if cached.AgentID != string(settings.Harness) || catalog.AgentID != string(settings.Harness) || cached.ProjectID != "" {
		result.Capabilities.Reason = "catalog_scope_mismatch"
		return result, nil
	}
	if catalog.Stale || cached.RefreshError != "" || catalog.RefreshError != "" || cached.RefreshState != "idle" || (catalog.RefreshState != "" && catalog.RefreshState != "idle") {
		result.Capabilities.Reason = "catalog_stale"
		return result, nil
	}
	if observed.IsZero() {
		result.Capabilities.Reason = "catalog_unverified"
		return result, nil
	}
	if domain.AgentModelCatalogNeedsRevalidation(observed, s.now()) {
		result.Capabilities.Reason = "catalog_stale"
		return result, nil
	}
	if catalog.SelectionMode == ports.ModelSelectionModeList {
		result.Capabilities.Reason = "model_settings_unavailable"
		return result, nil
	}
	if len(catalog.Models) == 0 {
		return result, nil
	}
	result.Capabilities.State = CapabilitySupported
	result.Capabilities.Reason = ""
	for _, model := range catalog.Models {
		fast := CapabilityUnknown
		// Legacy bool false also means "field omitted". It is not negative
		// evidence. Positive support is safe to report; absence stays unknown.
		if model.SupportsFast {
			fast = CapabilitySupported
		}
		efforts := append([]string{}, model.Efforts...)
		result.Capabilities.Models = append(result.Capabilities.Models, ModelCapabilities{Model: model.ID, IsDefault: model.IsDefault, Efforts: efforts, Fast: fast})
	}
	return result, nil
}
