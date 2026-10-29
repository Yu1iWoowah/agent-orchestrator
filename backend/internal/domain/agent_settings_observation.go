package domain

import "time"

// AgentSettingState describes provider evidence, independently of preference.
type AgentSettingState string

// Provider evidence states.
const (
	AgentSettingKnown       AgentSettingState = "known"
	AgentSettingUnknown     AgentSettingState = "unknown"
	AgentSettingNotProvided AgentSettingState = "not_provided"
)

// AgentSettingObservation is a provider-reported config value, not a billing
// or execution-speed guarantee. Unknown and absent controls carry no value.
type AgentSettingObservation struct {
	State  AgentSettingState `json:"state" enum:"known,unknown,not_provided"`
	Value  string            `json:"value,omitempty"`
	Reason string            `json:"reason,omitempty"`
}

// AgentSettingsEffective contains receipts, never copies of requested settings.
type AgentSettingsEffective struct {
	Model      AgentSettingObservation `json:"model"`
	Effort     AgentSettingObservation `json:"effort"`
	FastMode   AgentSettingObservation `json:"fastMode"`
	ObservedAt *time.Time              `json:"observedAt,omitempty"`
	Source     string                  `json:"source,omitempty"`
	Owner      *AgentSettingsOwner     `json:"owner,omitempty"`
}

// AgentSettingsOwner pins a receipt to the controller that obtained it.
type AgentSettingsOwner struct {
	SessionID              SessionID `json:"sessionId"`
	AccountID              string    `json:"accountId"`
	ControllerGeneration   string    `json:"controllerGeneration"`
	ProviderConversationID string    `json:"providerConversationId"`
	BranchID               string    `json:"branchId"`
}

// AgentSettingsTurnSnapshot describes the preference on the dispatch wire.
// Missing selection/revision is explicit unknown, never reconstructed history.
type AgentSettingsTurnSnapshot struct {
	Attempt        int                 `json:"attempt,omitempty"`
	State          AgentSettingState   `json:"state" enum:"known,unknown"`
	Source         string              `json:"source,omitempty" enum:"root,side,background"`
	Owner          *AgentSettingsOwner `json:"owner,omitempty"`
	TurnID         string              `json:"turnId,omitempty"`
	ProviderTurnID string              `json:"providerTurnId,omitempty"`
	Settings       *AgentSettings      `json:"settings,omitempty"`
	Revision       *int64              `json:"revision,omitempty"`
	CapturedAt     *time.Time          `json:"capturedAt,omitempty"`
	Reason         string              `json:"reason,omitempty"`
}

// Clone detaches pointer fields so a receipt consumer cannot change the frozen
// selection or owner retained by a controller or the side recovery store.
func (s AgentSettingsTurnSnapshot) Clone() AgentSettingsTurnSnapshot {
	if s.Owner != nil {
		owner := *s.Owner
		s.Owner = &owner
	}
	if s.Settings != nil {
		settings := *s.Settings
		s.Settings = &settings
	}
	if s.Revision != nil {
		revision := *s.Revision
		s.Revision = &revision
	}
	if s.CapturedAt != nil {
		at := *s.CapturedAt
		s.CapturedAt = &at
	}
	return s
}

// AgentSettingsObservation is internal: attribution comes from the live owner,
// not a client-supplied account or a fresh lookup after the provider was started.
type AgentSettingsObservation struct {
	Owner       AgentSettingsOwner
	Effective   AgentSettingsEffective
	CurrentTurn *AgentSettingsTurnSnapshot
}

// UnknownAgentSettingsEffective never supplies a value inferred from preference.
func UnknownAgentSettingsEffective(reason string) AgentSettingsEffective {
	unknown := AgentSettingObservation{State: AgentSettingUnknown, Reason: reason}
	return AgentSettingsEffective{Model: unknown, Effort: unknown, FastMode: unknown}
}
