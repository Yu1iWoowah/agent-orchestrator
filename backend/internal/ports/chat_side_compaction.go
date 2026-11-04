package ports

import (
	"context"
	"encoding/json"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// SideCompactionStore owns atomic registration and unresolved side content.
type SideCompactionStore interface {
	RegisterSideCompaction(context.Context, domain.SideCompaction) error
	SideCompactions(context.Context, bool) ([]domain.SideCompaction, error)
	SaveSideCompactionContent(context.Context, string, json.RawMessage) error
	TransitionSideCompaction(context.Context, string, string, domain.SideCompactionState, bool, bool) error
}

// ChatCompactionScope contains facts from the original provider launch.
type ChatCompactionScope struct {
	NativeHome, NativeHomeError string
	Processes                   *domain.CompactionProcessScope
}

// ChatCompactionScopeReader queries the original host without provider I/O.
type ChatCompactionScopeReader interface {
	CompactionScope(context.Context) (ChatCompactionScope, error)
}
