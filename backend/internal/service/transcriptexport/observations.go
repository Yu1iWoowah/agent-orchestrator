package transcriptexport

import (
	"encoding/json"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Native observations can precede disk flush. File stability alone cannot
// discharge them; a Stop needs its prompt and a submission its nonce attachment.
func verifyObservations(source domain.TranscriptSourceSnapshot, segments []ports.NativeTranscriptSegment) error {
	if source.Session.Metadata.ConversationCheckpointUnsettled {
		return incomplete("TRANSCRIPT_TAIL_UNSETTLED", "native observation has no correlated prompt")
	}
	encoded := source.Session.Metadata.NativeCheckpointEvidence
	if encoded == "" {
		return nil
	}
	var evidence domain.NativeCheckpointEvidence
	if json.Unmarshal([]byte(encoded), &evidence) != nil || evidence.Invalid || evidence.NativeID == "" || len(evidence.Events) == 0 {
		return incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "native checkpoint evidence is invalid")
	}
	var segment *ports.NativeTranscriptSegment
	for i := range segments {
		if segments[i].NativeID == evidence.NativeID {
			segment = &segments[i]
			break
		}
	}
	if segment == nil {
		return incomplete("TRANSCRIPT_EPOCHS_INCOMPLETE", "observed native epoch is missing")
	}
	prompts := promptInventory(*segment)
	records := map[string]ports.NativeTranscriptRecord{}
	for _, record := range segment.Records {
		if record.Excluded == "" && record.Provenance == domain.NativeThreadMain {
			records[record.ID] = record
		}
	}
	for _, observation := range evidence.Events {
		if observation.Generation == "" {
			return incomplete("TRANSCRIPT_PROVENANCE_UNKNOWN", "native observation has no generation")
		}
		var matches []nativePrompt
		if !observation.Submission {
			for _, prompt := range prompts {
				if prompt.ID == observation.PromptID && (observation.Text == "" || domain.NativeCheckpointTextMatches(observation.Text, prompt.Assistant)) {
					matches = append(matches, prompt)
				}
			}
		} else if observation.SubmissionID != "" {
			nonce := domain.NativeSubmissionContext(observation.SubmissionID)
			for _, record := range records {
				if record.Type != "attachment" || record.AttachmentType != "hook_additional_context" || record.HookEvent != "UserPromptSubmit" {
					continue
				}
				found := false
				for _, block := range record.Blocks {
					if block.Kind == ports.TranscriptText && block.Text == nonce {
						found = true
					}
				}
				if !found {
					continue
				}
				parent, ok := records[record.ParentID]
				if !ok {
					continue
				}
				submitted := recordText(parent)
				if parent.Type != "attachment" || parent.AttachmentType != "queued_command" {
					submitted = ""
				}
				seen := map[string]bool{}
				node := parent
				for node.ID != "" && !seen[node.ID] {
					seen[node.ID] = true
					if node.Type == "user" && !node.Meta && !node.Companion && recordText(node) != "" {
						for _, prompt := range prompts {
							if prompt.ID == node.PromptID {
								if submitted == "" {
									submitted = prompt.User
								}
								if observation.Coordination || domain.NativeCheckpointTextMatches(observation.Text, submitted) {
									matches = append(matches, prompt)
								}
							}
						}
						break
					}
					node = records[node.ParentID]
				}
			}
		}
		if len(matches) != 1 {
			return incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "native observation is missing, ambiguous, or not flushed")
		}
	}
	return nil
}
