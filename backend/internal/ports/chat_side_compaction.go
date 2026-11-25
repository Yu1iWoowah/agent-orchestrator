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
	// SideCompactions lists compaction metadata, optionally limited to unresolved records.
	SideCompactions(context.Context, bool) ([]domain.SideCompaction, error)
	SideCompactionRecovery(context.Context) ([]domain.SideCompaction, error)
	SideCompactionContent(context.Context, string) (json.RawMessage, error)
	UnresolvedSideCompactionWithContent(context.Context, string) (domain.SideCompaction, bool, error)
	// ListUnresolvedSideCompactionsWithContent reads the first unresolved record and content per side.
	// A nil slice selects every side.
	ListUnresolvedSideCompactionsWithContent(context.Context, []string) ([]domain.SideCompaction, error)
	SideWithUnresolvedNativeCompaction(context.Context, string, string) (string, error)
	HasUnresolvedSideCompaction(context.Context, string) (bool, error)
	UnresolvedSideForClose(context.Context, string) (domain.SideConversation, bool, error)
	CloseSideCompactions(context.Context, string) error
	SaveSideCompactionContent(context.Context, string, json.RawMessage) error
	TransitionSideCompaction(context.Context, string, string, domain.SideCompactionState, bool, bool) error
	MarkSideCompactionHistorical(context.Context, string, string, string) error
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

// ChatNativeGuard checks a native operation while serializing it with registration.
type ChatNativeGuard interface {
	Run(context.Context, domain.AgentHarness, ChatResumeConfig, func() error) error
}

// ErrNativeConversationProtected requires explicit side closure before native reuse.
var ErrNativeConversationProtected = errors.New("native conversation is protected; close the side chat first")
