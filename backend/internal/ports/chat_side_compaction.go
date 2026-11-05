package ports

import (
	"context"
	"encoding/json"
	"errors"

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

// ErrChatCompactionScopeUnsupported means the original host lacks the scope query.
var ErrChatCompactionScopeUnsupported = errors.New("chat host does not support compaction scope")

// ErrChatCompactionScopeNoHost means this conversation has no persistent host.
var ErrChatCompactionScopeNoHost = errors.New("compaction scope has no persistent host")
