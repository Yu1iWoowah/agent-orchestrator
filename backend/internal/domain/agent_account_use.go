package domain

// AgentAccountUsageOwner is a local metadata projection, never a profile path
// or provider transcript. Blocking is derived from execution and transition
// facts at read time; it is not a stored session status.
type AgentAccountUsageOwner struct {
	Kind string `json:"kind" enum:"session,reviewer,transition"`
	// ProjectID is empty for standalone executions.
	ProjectID    ProjectID    `json:"projectId"`
	SessionID    SessionID    `json:"sessionId"`
	SessionName  string       `json:"sessionName,omitempty"`
	ReviewID     string       `json:"reviewId,omitempty"`
	PRURL        string       `json:"prUrl,omitempty"`
	Harness      AgentHarness `json:"harness"`
	TransitionID string       `json:"transitionId,omitempty"`
	Blocking     bool         `json:"blocking"`
}
