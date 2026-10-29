package agentlaunch

// ClearCodexCredentialEnv clears the credential overrides already recognized
// by AO's Codex account adapter: OPENAI_API_KEY (OpenAI client key), CODEX_API_KEY
// (Codex CLI key), and CODEX_ACCESS_TOKEN (account token override). Keep this
// policy shared by account probes, Chat and TUI launches: ambient or project
// values must not replace a managed profile's file-backed login.
func ClearCodexCredentialEnv(env map[string]string) {
	for _, key := range []string{"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN"} {
		env[key] = ""
	}
}
