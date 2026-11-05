package chat

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

// Applicability uses the original launch facts; account ownership, native
// store location and provider sandboxing do not restrict policy B.
func sideCompactionApplicable(row domain.SideCompaction, platform string) (bool, string) {
	if platform != "linux" {
		return false, "scope_platform_unsupported"
	}
	if row.Provider == domain.HarnessCodex && !row.ReadOnly && row.PermissionMode != domain.PermissionModeAcceptEdits && row.PermissionMode != domain.PermissionModeAuto {
		return false, "codex_full_access"
	}
	return true, ""
}
