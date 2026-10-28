package claudecode

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type exportRecord struct {
	CompactSummary        bool   `json:"isCompactSummary"`
	PreventedContinuation bool   `json:"preventedContinuation"`
	Operation             string `json:"operation"`
	Meta                  bool   `json:"isMeta"`
	Companion             bool   `json:"turnCompanion"`
	UUID                  string `json:"uuid"`
	ParentUUID            string `json:"parentUuid"`
	SessionID             string `json:"sessionId"`
	AgentID               string `json:"agentId"`
	PromptID              string `json:"promptId"`
	Type                  string `json:"type"`
	Subtype               string `json:"subtype"`
	Sidechain             *bool  `json:"isSidechain"`
	Version               string `json:"version"`
	Timestamp             string `json:"timestamp"`
	Message               struct {
		Content    json.RawMessage `json:"content"`
		StopReason string          `json:"stop_reason"`
		Model      string          `json:"model"`
	} `json:"message"`
	Attachment struct {
		Type      string          `json:"type"`
		HookEvent string          `json:"hookEvent"`
		Content   json.RawMessage `json:"content"`
		Prompt    json.RawMessage `json:"prompt"`
	} `json:"attachment"`
	Data struct {
		Type    string `json:"type"`
		AgentID string `json:"agentId"`
	} `json:"data"`
}

func decodeTranscriptRecord(raw []byte, native string, line int, request ports.NativeTranscriptRequest) (ports.NativeTranscriptRecord, string, error) {
	if e := strictTranscriptJSON(raw); e != nil {
		return ports.NativeTranscriptRecord{}, "", e
	}
	var in exportRecord
	if err := json.Unmarshal(raw, &in); err != nil {
		return ports.NativeTranscriptRecord{}, "", transcriptFailure("TRANSCRIPT_RECORD_INVALID", "malformed native JSON")
	}
	out := ports.NativeTranscriptRecord{AttachmentType: in.Attachment.Type, HookEvent: in.Attachment.HookEvent, Meta: in.Meta, Companion: in.Companion, Line: line, ID: in.UUID, ParentID: in.ParentUUID, PromptID: in.PromptID, NativeID: in.SessionID, AgentID: in.AgentID, Type: in.Type, Timestamp: in.Timestamp}
	if in.Type == "assistant" {
		out.State = "pending"
		if in.Message.StopReason == "end_turn" || in.Message.StopReason == "max_tokens" {
			out.State = "complete"
		}
		if in.Message.Model == "<synthetic>" {
			out.State = "synthetic"
		}
	}
	if in.Type == "attachment" && in.Attachment.Type == "queued_command" {
		out.State = "queued"
	}
	if in.Timestamp != "" {
		if _, err := time.Parse(time.RFC3339Nano, in.Timestamp); err != nil {
			out.Timestamp = ""
		}
	}
	if in.Type == "summary" || in.Type == "system" && in.Subtype == "compact_boundary" {
		return out, "", transcriptFailure("TRANSCRIPT_ANCESTRY_INCOMPLETE", "compacted ancestry requires reconciliation")
	}
	metadataKind := transcriptMetadataKind(in)
	metadata := metadataKind != ""
	switch in.Type {
	case "user", "assistant", "attachment", "file-history-snapshot", "system", "progress", "queue-operation":
	default:
		return out, "", transcriptFailure("TRANSCRIPT_SCHEMA_UNSUPPORTED", "unknown native record type")
	}
	if in.Version != "" && !supportedTranscriptVersion(in.Version) || !metadata && in.Version == "" {
		return out, "", transcriptFailure("TRANSCRIPT_SCHEMA_UNSUPPORTED", "unverified native schema version")
	}
	if metadata && in.SessionID == "" {
		fileMetadata := in.Type == "file-history-snapshot" || in.Type == "system" && in.Subtype == "turn_duration"
		if !fileMetadata || in.UUID != "" || in.ParentUUID != "" || in.AgentID != "" || in.Data.AgentID != "" || in.Sidechain != nil && *in.Sidechain {
			return out, "", transcriptFailure("TRANSCRIPT_PROVENANCE_UNKNOWN", "metadata lacks session ownership")
		}
		out.NativeID = native
		out.Provenance = domain.NativeThreadMain
		out.ProvenanceRule = "owned-file-metadata"
		out.Excluded = "metadata"
		out.Meta = true
		out.ID = ""
		return out, "", nil
	}
	if in.SessionID != native {
		if in.Sidechain == nil || *in.Sidechain || in.AgentID != "" {
			return out, "", transcriptFailure("TRANSCRIPT_PROVENANCE_UNKNOWN", "foreign native record lacks root provenance")
		}
		fingerprint, e := canonicalTranscriptHash(raw)
		if e != nil {
			return out, "", e
		}
		for _, ancestor := range request.VerifiedAncestors {
			if ancestor.NativeID != in.SessionID || ancestor.ID != in.UUID {
				continue
			}
			if ancestor.RecordHash != fingerprint {
				return out, "", transcriptFailure("TRANSCRIPT_IDENTITY_CONFLICT", "copied ancestor bytes differ from verified source")
			}
			out = ancestor
			out.Line = line
			out.Excluded = "ancestor"
			out.ProvenanceRule = "verified-owned-ancestor"
			return out, in.Version, nil
		}
		if slices.Contains(request.KnownNativeIDs, in.SessionID) {
			return out, "", transcriptFailure("TRANSCRIPT_ANCESTOR_REQUIRED", "owned parent epoch must be verified first")
		}
		return out, "", transcriptFailure("TRANSCRIPT_PROVENANCE_UNKNOWN", "native record does not match its owned source")
	}
	if in.Type == "progress" && in.Data.Type == "agent_progress" && in.Data.AgentID != "" {
		out.AgentID = in.Data.AgentID
		out.Provenance = domain.NativeThreadChild
		out.ProvenanceRule = "agent-progress-envelope"
		out.Excluded = "child"
		return out, in.Version, nil
	}
	if in.Sidechain == nil {
		if in.Type != "queue-operation" || !metadata {
			return out, "", transcriptFailure("TRANSCRIPT_PROVENANCE_UNKNOWN", "native sidechain provenance is absent")
		}
	}
	if in.Sidechain != nil && *in.Sidechain {
		out.Provenance = domain.NativeThreadChild
		out.ProvenanceRule = "explicit-sidechain"
		out.Excluded = "child"
		return out, in.Version, nil
	}
	if in.AgentID != "" || metadata && in.Data.AgentID != "" {
		return out, "", transcriptFailure("TRANSCRIPT_PROVENANCE_UNKNOWN", "root flag conflicts with native child identity")
	}
	out.Provenance = domain.NativeThreadMain
	out.ProvenanceRule = "explicit-root-session"
	if metadata {
		out.Meta = true
		out.Excluded = "metadata"
		out.ProvenanceRule = "owned-session-metadata/" + metadataKind
		if in.Type == "system" && in.Subtype == "stop_hook_summary" && in.PreventedContinuation {
			if out.ID == "" {
				return out, "", transcriptFailure("TRANSCRIPT_ANCESTRY_INCOMPLETE", "pending continuation has no lineage identity")
			}
			out.State = "continuation-pending"
		}
		return out, in.Version, nil
	}
	if out.ID == "" {
		return out, "", transcriptFailure("TRANSCRIPT_PROVENANCE_UNKNOWN", "main record lacks native identity")
	}
	if in.CompactSummary {
		if in.Type != "user" {
			return out, "", transcriptFailure("TRANSCRIPT_SCHEMA_UNSUPPORTED", "invalid compaction summary envelope")
		}
		if in.ParentUUID == "" {
			return out, "", transcriptFailure("TRANSCRIPT_ANCESTRY_INCOMPLETE", "compaction summary has no original ancestry")
		}
		out.Meta = true
		out.Excluded = "compaction-summary"
		out.State = "compaction-summary"
		out.ProvenanceRule = "explicit-compaction-summary"
		return out, in.Version, nil
	}
	var err error
	switch in.Type {
	case "user", "assistant":
		out.Blocks, err = decodeTranscriptBlocks(in.Message.Content)
	case "attachment":
		switch in.Attachment.Type {
		case "queued_command":
			out.Blocks, err = decodeTranscriptBlocks(in.Attachment.Prompt)
		case "hook_additional_context":
			var texts []string
			if json.Unmarshal(in.Attachment.Content, &texts) == nil {
				for _, text := range texts {
					out.Blocks = append(out.Blocks, ports.NativeTranscriptBlock{Kind: ports.TranscriptText, Text: text})
				}
			} else {
				out.Blocks, err = decodeTranscriptBlocks(in.Attachment.Content)
			}
		case "hook_success", "skill_listing":
			out.Blocks = []ports.NativeTranscriptBlock{{Kind: ports.TranscriptAttachment, Reference: in.Attachment.Type}}
		default:
			err = transcriptFailure("TRANSCRIPT_SCHEMA_UNSUPPORTED", "unknown native attachment")
		}
	default:
		err = transcriptFailure("TRANSCRIPT_SCHEMA_UNSUPPORTED", "unverified native metadata envelope")
	}
	if in.Type == "user" && !in.Meta && IsInterruptMarker(in.Message.Content) {
		out.State = "interrupted"
	}
	return out, in.Version, err
}

func transcriptMetadataKind(in exportRecord) string {
	switch in.Type {
	case "file-history-snapshot":
		return in.Type
	case "system":
		switch in.Subtype {
		case "turn_duration", "stop_hook_summary", "local_command":
			return in.Type + "/" + in.Subtype
		}
	case "progress":
		if in.Data.Type == "hook_progress" {
			return "progress/hook_progress"
		}
	case "queue-operation":
		if in.Operation == "enqueue" || in.Operation == "dequeue" {
			return "queue-operation/" + in.Operation
		}
	}
	return ""
}

func decodeTranscriptBlocks(raw json.RawMessage) ([]ports.NativeTranscriptBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, transcriptFailure("TRANSCRIPT_SCHEMA_UNSUPPORTED", "native content is absent")
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if strings.ContainsRune(text, 0) {
			return nil, transcriptFailure("TRANSCRIPT_RECORD_INVALID", "native text contains NUL")
		}
		return []ports.NativeTranscriptBlock{{Kind: ports.TranscriptText, Text: text}}, nil
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil || len(blocks) == 0 {
		return nil, transcriptFailure("TRANSCRIPT_SCHEMA_UNSUPPORTED", "unreadable native content")
	}
	result := make([]ports.NativeTranscriptBlock, 0, len(blocks))
	for _, rawBlock := range blocks {
		var block struct {
			Type, Text, ID, Name string
			Input                json.RawMessage
			ToolUseID            string `json:"tool_use_id"`
			IsError              bool   `json:"is_error"`
		}
		if json.Unmarshal(rawBlock, &block) != nil {
			return nil, transcriptFailure("TRANSCRIPT_RECORD_INVALID", "invalid native content block")
		}
		out := ports.NativeTranscriptBlock{}
		switch block.Type {
		case "text":
			out.Kind = ports.TranscriptText
			out.Text = block.Text
		case "tool_use":
			if block.ID == "" || block.Name == "" || !json.Valid(block.Input) {
				return nil, transcriptFailure("TRANSCRIPT_SCHEMA_UNSUPPORTED", "tool call lacks identity or input")
			}
			out.Kind = ports.TranscriptToolCall
			out.CallID = block.ID
			out.ToolName = block.Name
			out.Text = string(block.Input)
			out.Status = "requested"
		case "tool_result":
			if block.ToolUseID == "" {
				return nil, transcriptFailure("TRANSCRIPT_SCHEMA_UNSUPPORTED", "tool result lacks call identity")
			}
			out.Kind = ports.TranscriptToolResult
			out.CallID = block.ToolUseID
			out.Status = "succeeded"
			if block.IsError {
				out.Status = "failed"
			}
		case "image", "document", "audio", "resource", "thinking", "redacted_thinking":
			out.Kind = ports.TranscriptAttachment
			out.Reference = fmt.Sprintf("%s sha256:%x", block.Type, sha256.Sum256(rawBlock))
		default:
			return nil, transcriptFailure("TRANSCRIPT_SCHEMA_UNSUPPORTED", "unknown native content block")
		}
		if strings.ContainsRune(out.Text, 0) {
			return nil, transcriptFailure("TRANSCRIPT_RECORD_INVALID", "native text contains NUL")
		}
		result = append(result, out)
	}
	return result, nil
}

func canonicalTranscriptHash(raw []byte) (string, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if e := d.Decode(&value); e != nil {
		return "", e
	}
	b, e := json.Marshal(value)
	if e != nil {
		return "", e
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
