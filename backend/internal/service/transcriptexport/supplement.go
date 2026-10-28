package transcriptexport

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// AOSupplement records captured AO input without asserting native delivery.
type AOSupplement struct {
	MessageID      string                  `json:"messageId"`
	TurnID         string                  `json:"turnId,omitempty"`
	TurnState      domain.TurnState        `json:"turnState,omitempty"`
	NativeDelivery string                  `json:"nativeDelivery"`
	Attachments    []AOAttachmentReference `json:"attachments"`
	TextReferences []string                `json:"textReferences,omitempty"`
}

// AOAttachmentReference retains provenance without copying embedded payloads.
type AOAttachmentReference struct {
	Type                 string                      `json:"type"`
	Reference            string                      `json:"reference"`
	MIMEType             string                      `json:"mimeType,omitempty"`
	Name                 string                      `json:"name,omitempty"`
	URI                  string                      `json:"uri,omitempty"`
	EmbeddedURIReference string                      `json:"embeddedUriReference,omitempty"`
	Excerpt              *ports.ChatExcerptReference `json:"excerpt,omitempty"`
	FileReference        *ports.ChatFileReference    `json:"fileReference,omitempty"`
	CapturedLines        int                         `json:"capturedLines,omitempty"`
}

func aoSupplement(message domain.ConversationMessage, state domain.TurnState) (AOSupplement, string, error) {
	entry := AOSupplement{MessageID: message.ID, TurnID: message.TurnID, TurnState: state, NativeDelivery: "unconfirmed"}
	var body strings.Builder
	fmt.Fprintf(&body, "## AO-only supplement / %s\n\nSequence: %d; delivery: %s; origin: %s; AO state: %s; native delivery: unconfirmed\n\n%s", inlineMetadata(message.ID), message.Sequence, inlineMetadata(message.ClientMessageID), inlineMetadata(string(message.Origin)), inlineMetadata(string(state)), literal(message.Text))
	if message.DeliveryContentJSON == "" {
		return entry, body.String(), nil
	}
	var blocks []json.RawMessage
	if !utf8.ValidString(message.DeliveryContentJSON) || json.Unmarshal([]byte(message.DeliveryContentJSON), &blocks) != nil {
		return entry, "", incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "AO-only structured content is invalid")
	}
	for _, raw := range blocks {
		var content ports.ChatContent
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&content) != nil {
			return entry, "", incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "AO-only content metadata cannot be verified")
		}
		reference := fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
		item := AOAttachmentReference{Type: content.Type, Reference: reference, MIMEType: content.MIMEType, Name: content.Name, URI: content.URI}
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(item.URI)), "data:") {
			item.EmbeddedURIReference = fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(item.URI)))
			item.URI = ""
		}
		switch content.Type {
		case "text":
			entry.TextReferences = append(entry.TextReferences, reference)
			body.WriteString("AO-only structured text / " + inlineMetadata(reference) + "\n\n" + literal(content.Text))
			continue
		case "image", "resource", "resource_link":
		case "excerpt":
			if content.Excerpt == nil {
				return entry, "", incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "AO-only excerpt has no source reference")
			}
			item.Excerpt = &content.Excerpt.Reference
		case "file_reference":
			if content.FileReference == nil {
				return entry, "", incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "AO-only file has no source reference")
			}
			file := *content.FileReference
			item.CapturedLines = len(file.Lines)
			file.Lines = nil
			item.FileReference = &file
		default:
			return entry, "", incomplete("TRANSCRIPT_SCHEMA_UNSUPPORTED", "unknown AO-only structured content type")
		}
		entry.Attachments = append(entry.Attachments, item)
		metadata, err := json.MarshalIndent(item, "", "  ")
		if err != nil {
			return entry, "", err
		}
		body.WriteString("AO-only attachment reference\n\n" + literal(string(metadata)))
	}
	fmt.Fprintf(&body, "AO-only attachment count: %d; structured text blocks: %d. Embedded payloads are not expanded.\n\n", len(entry.Attachments), len(entry.TextReferences))
	return entry, body.String(), nil
}
