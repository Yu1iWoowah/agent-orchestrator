package ports

import (
	"context"
	"io"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// RCAInlineAdmission is an already-reserved, native-only raw reader. Release
// belongs to the physical transport after reads stop, not request cancellation.
type RCAInlineAdmission interface {
	Context() context.Context
	Read(io.Reader, int64) string
	Release()
}

// RCAClaudeRuntime is named non-secret runtime evidence. Empty versions are
// UNKNOWN, never inferred from a path or a requested model.
type RCAClaudeRuntime struct {
	BinaryPath, BridgePath, OS               string
	BinaryVersion, BridgeVersion, SDKVersion string
}

// RCAClaudeLaunch is AO-owned main Chat context, not exporter input.
type RCAClaudeLaunch struct {
	SessionID            domain.SessionID
	ProjectID            domain.ProjectID
	Kind                 domain.SessionKind
	DataRoot, Generation string
}

// RCAClaudeEnvironment receives only keys, never credential/settings values.
// It returns an additive RCA-only env delta; it cannot fail native launch.
type RCAClaudeEnvironment func(context.Context, RCAClaudeRuntime, []string) map[string]string

// RCAClaudeLaunchControl is called only by the ordinary main Chat path.
type RCAClaudeLaunchControl interface {
	InvalidateClaudeLaunch(domain.SessionID)
	ClaudeControllerStopped(domain.SessionID, string)
	PrepareClaudeEnvironment(context.Context, RCAClaudeLaunch, RCAClaudeRuntime, []string) map[string]string
	ClaudeLaunchReady(context.Context, RCAClaudeLaunch, bool)
}
