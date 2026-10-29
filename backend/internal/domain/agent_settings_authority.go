package domain

// AgentSettingsAuthority is the non-secret scope accepted with a managed
// operation. It is not a credential and cannot authenticate an HTTP caller.
type AgentSettingsAuthority struct {
	ProjectID       ProjectID    `json:"projectId"`
	ActorHarness    AgentHarness `json:"actorHarness"`
	ActorMode       SessionMode  `json:"actorMode"`
	ActorGeneration string       `json:"actorGeneration"`
	IssuedAt        int64        `json:"issuedAt"`
	ExpiresAt       int64        `json:"expiresAt"`
}

// Matches checks the durable actor owner; role and sole-active-orchestrator
// checks are performed by the authorized service boundary.
func (a AgentSettingsAuthority) Matches(rec SessionRecord) bool {
	generation := rec.Metadata.RuntimeLaunchID
	if NormalizeSessionMode(rec.Mode) == SessionModeChat {
		generation = rec.Metadata.ControllerGeneration
	}
	return !rec.IsTerminated && a.ProjectID != "" && rec.ProjectID == a.ProjectID && rec.Harness == a.ActorHarness && NormalizeSessionMode(rec.Mode) == a.ActorMode && generation != "" && generation == a.ActorGeneration
}
