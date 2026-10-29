package agentlaunch

import "runtime"

// ValidateManagedCodexEnv checks new managed launches while leaving device and
// existing non-Linux compatibility paths alone.
func ValidateManagedCodexEnv(env map[string]string) error {
	if runtime.GOOS == "linux" && env[ManagedCodexAccountEnv] == "1" {
		return ValidateCodexFileStore(env["CODEX_HOME"])
	}
	return nil
}
