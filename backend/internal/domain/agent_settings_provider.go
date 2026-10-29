package domain

// AgentSettingsNativeValues contains only non-secret provider option values.
// An empty field means the provider did not offer it, not a reset instruction.
type AgentSettingsNativeValues struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
	Fast   string `json:"fast"`
}

// AgentSettingsProviderPlan is a qualified preimage for one quiescent provider.
// Account, binary and policy qualification belongs to the runtime journal.
// After is a resolved wire baseline for providers with per-turn configuration;
// it is never evidence of the provider's effective values.
type AgentSettingsProviderPlan struct {
	ProviderConversationID string                    `json:"providerConversationId"`
	Protocol               string                    `json:"protocol"`
	Source                 AgentSettings             `json:"source"`
	Target                 AgentSettings             `json:"target"`
	Before                 AgentSettingsNativeValues `json:"before"`
	After                  AgentSettingsNativeValues `json:"after"`
	Permission             string                    `json:"permission"`
}

// AgentSettingsQualification identifies the discovery inputs and authorization
// policy checked by the runtime boundary. Neither field contains credentials.
type AgentSettingsQualification struct {
	InputFingerprint string `json:"inputFingerprint"`
	PolicyVersion    string `json:"policyVersion"`
}

// AgentSettingsExecution pins a freshly validated executor without rewriting
// the operation's immutable request or its original controller generation.
type AgentSettingsExecution struct {
	Owner         AgentSettingsOwner         `json:"owner"`
	Revision      int64                      `json:"revision"`
	Qualification AgentSettingsQualification `json:"qualification"`
	Plan          AgentSettingsProviderPlan  `json:"plan"`
}
