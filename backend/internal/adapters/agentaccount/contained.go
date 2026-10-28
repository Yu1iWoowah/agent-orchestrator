package agentaccount

// Prepare creates links only to AO-owned native conversation history. Settings,
// credentials, model caches, locks and IPC remain private to each new profile.
func (*Codex) Prepare(home string) error {
	return prepareContained(home, "codex", []string{"sessions", "archived_sessions"})
}

// Prepare never imports device settings or history into a new Claude profile.
func (*Claude) Prepare(home string) error {
	return prepareContained(home, "claude", []string{"projects", "sessions"})
}
