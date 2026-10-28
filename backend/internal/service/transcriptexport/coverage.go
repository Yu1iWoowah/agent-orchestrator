package transcriptexport

import (
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// CoverageLink records how one AO turn maps to an owned native prompt.
type CoverageLink struct {
	DispatchTurnID string `json:"dispatchTurnId,omitempty"`
	AOTurnID       string `json:"aoTurnId"`
	NativeID       string `json:"nativeId"`
	NativePromptID string `json:"nativePromptId"`
	Method         string `json:"method"`
}
type nativePrompt struct{ ID, RecordID, User, Assistant string }

func promptInventory(seg ports.NativeTranscriptSegment) []nativePrompt {
	records := map[string]ports.NativeTranscriptRecord{}
	for _, r := range seg.Records {
		if r.Excluded == "" {
			records[r.ID] = r
		}
	}
	var out []nativePrompt
	for _, id := range seg.MainRecordIDs {
		r := records[id]
		text := recordText(r)
		if r.Type == "user" && !r.Meta && !r.Companion && r.State != "interrupted" && text != "" && r.PromptID != "" {
			out = append(out, nativePrompt{ID: r.PromptID, RecordID: r.ID, User: text})
		} else if r.Type == "assistant" && len(out) > 0 && text != "" {
			out[len(out)-1].Assistant = text
		}
	}
	return out
}
func recordText(r ports.NativeTranscriptRecord) string {
	var texts []string
	for _, b := range r.Blocks {
		if b.Kind == ports.TranscriptText {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}
func correlateTurn(turn domain.ConversationTurn, messages []domain.ConversationMessage, identities map[string]bool, prompts []nativePrompt) (CoverageLink, error) {
	link := CoverageLink{AOTurnID: turn.ID, NativePromptID: turn.ProviderTurnID, Method: "native-turn-identity"}
	if turn.ProviderInputText == "" && identities[turn.ProviderTurnID] {
		return link, nil
	}
	var user, assistant string
	for _, m := range messages {
		if m.TurnID != turn.ID {
			continue
		}
		if turn.ProviderInputText == "" && m.DeliveryContentJSON != "" {
			return link, incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "legacy ACP non-text delivery requires identity correlation")
		}
		switch m.Role {
		case domain.MessageRoleUser:
			if user != "" && turn.ProviderInputText == "" {
				return link, incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "multiple AO user deliveries require native identity")
			}
			user = m.Text
		case domain.MessageRoleAssistant:
			assistant = m.Text
		}
	}
	if turn.ProviderInputText != "" {
		user = turn.ProviderInputText
	}
	if user == "" {
		return link, incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "owned AO main turn is absent from its native epoch")
	}
	found := ""
	for _, p := range prompts {
		if identities[turn.ProviderTurnID] && p.ID != turn.ProviderTurnID && p.RecordID != turn.ProviderTurnID {
			continue
		}
		if !domain.NativeCheckpointTextMatches(user, p.User) || assistant != "" && !domain.NativeCheckpointTextMatches(assistant, p.Assistant) {
			continue
		}
		if found != "" {
			return link, incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "legacy ACP prompt/answer pair is ambiguous")
		}
		found = p.ID
	}
	if found == "" {
		return link, incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "legacy ACP prompt/answer pair is missing")
	}
	link.NativePromptID = found
	link.Method = "unique-pair/owned-native-thread"
	return link, nil
}

// SourceExclusion distinguishes out-of-scope child files from read records.
type SourceExclusion struct {
	NativeID         string `json:"nativeId"`
	SubagentID       string `json:"subagentId"`
	Path             string `json:"path"`
	Reason           string `json:"reason"`
	RecordCountKnown bool   `json:"recordCountKnown"`
}

func confirmedChild(s domain.TranscriptSourceSnapshot, root, child string) bool {
	for _, source := range s.Sources {
		if source.Kind != domain.UsageSourceClaudeSubagent || source.SubagentID != child || source.NativeSessionID != root {
			continue
		}
		for _, binding := range s.Bindings {
			if binding.ID == source.BindingID && binding.Harness == domain.HarnessClaudeCode && binding.NativeRootID == root {
				return true
			}
		}
	}
	return false
}
