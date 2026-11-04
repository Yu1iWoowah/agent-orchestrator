package ports

import (
	"context"
	"encoding/json"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type SideCompactionStore interface {
	RegisterSideCompaction(context.Context, domain.SideCompaction) error
	SideCompactions(context.Context, bool) ([]domain.SideCompaction, error)
	SaveSideCompactionContent(context.Context, string, json.RawMessage) error
	TransitionSideCompaction(context.Context, string, string, domain.SideCompactionState, bool, bool) error
}

type ChatCompactionScope struct {
	NativeHome string
	Processes  *domain.CompactionProcessScope
}

type ChatCompactionScopeReader interface {
	CompactionScope(context.Context) (ChatCompactionScope, error)
}
