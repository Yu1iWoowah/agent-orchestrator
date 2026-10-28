package ports

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ClaudeTranscriptSchemaV1 names the versioned Claude JSONL reader profile.
const ClaudeTranscriptSchemaV1 = "claude-code-jsonl/1"

// MaxTranscriptSourceBytes bounds the entire export source without truncation.
const MaxTranscriptSourceBytes = 64 << 20

// MaxTranscriptRecordBytes bounds a single native JSONL record.
const MaxTranscriptRecordBytes = 4 << 20

// MaxTranscriptRecords bounds the provenance ledger per source.
const MaxTranscriptRecords = 100000

// NativeTranscriptRequest pins one provider-owned file boundary. The caller
// supplies a settled native leaf; a path by itself is never completeness proof.
type NativeTranscriptRequest struct {
	KnownNativeIDs    []string
	VerifiedAncestors []NativeTranscriptRecord
	Source            NativeSessionRef
	Schema            string
	ByteCutoff        int64
	LeafID            string
	MaxSourceBytes    int64
	MaxRecordBytes    int
	MaxRecords        int
}

// TranscriptBlockKind identifies readable provider-neutral content.
type TranscriptBlockKind string

// Readable text, tool actions/results, and attachment references are distinct.
const (
	TranscriptText       TranscriptBlockKind = "text"
	TranscriptToolCall   TranscriptBlockKind = "tool_call"
	TranscriptToolResult TranscriptBlockKind = "tool_result"
	TranscriptAttachment TranscriptBlockKind = "attachment"
)

// NativeTranscriptBlock preserves content or an explicit omission reference.
type NativeTranscriptBlock struct {
	Kind      TranscriptBlockKind `json:"kind"`
	Text      string              `json:"text,omitempty"`
	CallID    string              `json:"callId,omitempty"`
	ToolName  string              `json:"toolName,omitempty"`
	Status    string              `json:"status,omitempty"`
	Reference string              `json:"reference,omitempty"`
}

// NativeTranscriptRecord keeps a ledger entry even for intentionally excluded
// records. Child text is not copied into the main-line body.
type NativeTranscriptRecord struct {
	Meta           bool                          `json:"isMeta,omitempty"`
	Companion      bool                          `json:"turnCompanion,omitempty"`
	AttachmentType string                        `json:"attachmentType,omitempty"`
	HookEvent      string                        `json:"hookEvent,omitempty"`
	RecordHash     string                        `json:"recordHash"`
	State          string                        `json:"state,omitempty"`
	Line           int                           `json:"line"`
	ID             string                        `json:"id,omitempty"`
	ParentID       string                        `json:"parentId,omitempty"`
	PromptID       string                        `json:"promptId,omitempty"`
	NativeID       string                        `json:"nativeId"`
	AgentID        string                        `json:"agentId,omitempty"`
	Type           string                        `json:"type"`
	Provenance     domain.NativeThreadProvenance `json:"provenance"`
	ProvenanceRule string                        `json:"provenanceRule"`
	Excluded       string                        `json:"excluded,omitempty"`
	Timestamp      string                        `json:"timestamp,omitempty"`
	Blocks         []NativeTranscriptBlock       `json:"blocks,omitempty"`
}

// TranscriptCounts accounts for each read record and tool-result block.
type TranscriptCounts struct {
	UnresolvedToolCalls int `json:"unresolvedToolCalls"`
	AncestorRecords     int `json:"ancestorRecords"`
	Records             int `json:"records"`
	MainRecords         int `json:"mainRecords"`
	ChildRecords        int `json:"childRecords"`
	MetadataRecords     int `json:"metadataRecords"`
	Duplicates          int `json:"duplicates"`
	UserRecords         int `json:"userRecords"`
	AssistantRecords    int `json:"assistantRecords"`
	ToolCalls           int `json:"toolCalls"`
	ToolResults         int `json:"toolResults"`
	OmittedToolResults  int `json:"omittedToolResults"`
	Attachments         int `json:"attachments"`
	UnknownTimestamps   int `json:"unknownTimestamps"`
}

// NativeTranscriptSegment is one verified file boundary and its identity ledger.
type NativeTranscriptSegment struct {
	TailState        string                   `json:"tailState"`
	NativeID         string                   `json:"nativeId"`
	SourcePath       string                   `json:"sourcePath"`
	ByteCutoff       int64                    `json:"byteCutoff"`
	SHA256           string                   `json:"sha256"`
	Schema           string                   `json:"schema"`
	ReaderVersion    string                   `json:"readerVersion"`
	ProviderVersions []string                 `json:"providerVersions"`
	LeafID           string                   `json:"leafId"`
	Counts           TranscriptCounts         `json:"counts"`
	MainRecordIDs    []string                 `json:"mainRecordIds"`
	Records          []NativeTranscriptRecord `json:"records"`

	// CheckStability is a bounded, non-parsing check of this read's file identity,
	// stat and tail fingerprint. Publication requires a fresh runtime witness.
	CheckStability func(context.Context) error `json:"-"`
}

// NativeTranscriptReader is read-only and never starts a provider controller.
type NativeTranscriptReader interface {
	ReadNativeTranscript(context.Context, NativeTranscriptRequest) (NativeTranscriptSegment, error)
}

// ErrNativeTranscriptIncomplete prevents partial evidence from being published.
var ErrNativeTranscriptIncomplete = errors.New("native transcript incomplete")

// NativeTranscriptError contains diagnostics, never provider message contents.
type NativeTranscriptError struct{ Code, Reason string }

func (e *NativeTranscriptError) Error() string { return e.Code + ": " + e.Reason }
func (e *NativeTranscriptError) Unwrap() error { return ErrNativeTranscriptIncomplete }
