package transcriptexport

import (
	"encoding/json"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// SteerCoverage binds an AO delivery receipt to one native input record.
type SteerCoverage struct {
	ActivityID     string   `json:"activityId"`
	DispatchTurnID string   `json:"dispatchTurnId"`
	NativeID       string   `json:"nativeId"`
	NativeRecordID string   `json:"nativeRecordId"`
	MemberTurnIDs  []string `json:"memberTurnIds"`
}

// batchInputs validates upstream relations without creating a parallel queue.
func batchInputs(s domain.TranscriptSourceSnapshot) (map[string]domain.TranscriptTurnInput, map[string]string, error) {
	turns := map[string]domain.ConversationTurn{}
	for _, t := range s.Turns {
		turns[t.ID] = t
	}
	inputs := map[string]domain.TranscriptTurnInput{}
	members := map[string]string{}
	for _, in := range s.Inputs {
		t, ok := turns[in.TurnID]
		if !ok || in.TurnID == "" {
			return nil, nil, incomplete("TRANSCRIPT_PROVENANCE_UNKNOWN", "input proof has no owned AO turn")
		}
		if _, duplicate := inputs[in.TurnID]; duplicate {
			return nil, nil, incomplete("TRANSCRIPT_IDENTITY_CONFLICT", "input proof repeats a turn")
		}
		inputs[in.TurnID] = in
		if t.State == domain.TurnStateQueued && in.Reserved {
			return nil, nil, incomplete("TRANSCRIPT_TAIL_UNSETTLED", "input delivery reservation is unsettled")
		}
		if in.BatchedInput && in.PromotedToTurnID == "" {
			return nil, nil, incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "batch member has no dispatch receipt")
		}
		if in.PromotedToTurnID == "" {
			continue
		}
		lead, ok := turns[in.PromotedToTurnID]
		if !ok || lead.ID == t.ID || lead.BranchID != t.BranchID || lead.ConversationID != t.ConversationID || t.State != domain.TurnStateCompleted {
			return nil, nil, incomplete("TRANSCRIPT_PROVENANCE_UNKNOWN", "batch member has no owned settled dispatch")
		}
		members[t.ID] = lead.ID
	}
	for _, lead := range members {
		if members[lead] != "" {
			return nil, nil, incomplete("TRANSCRIPT_PROVENANCE_UNKNOWN", "nested batch dispatch is not verified")
		}
	}
	return inputs, members, nil
}

type steerInput struct {
	Event             string            `json:"event"`
	Text              string            `json:"text"`
	SourceTurnID      string            `json:"sourceTurnId"`
	Captured          bool              `json:"providerInputCaptured"`
	ProviderInputText string            `json:"providerInputText"`
	StoredOnTurn      bool              `json:"providerInputStoredOnTurn"`
	Members           []string          `json:"providerInputSourceTurnIds"`
	Content           []json.RawMessage `json:"content"`
}

func steerDetails(s domain.TranscriptSourceSnapshot) (map[string]steerInput, error) {
	details := map[string]steerInput{}
	for _, a := range s.Activities {
		if a.Kind != domain.ActivityKindSystem || a.Status != domain.ActivityStatusCompleted || len(a.Detail) == 0 {
			continue
		}
		var in steerInput
		if json.Unmarshal(a.Detail, &in) != nil {
			return nil, incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "system delivery detail cannot be verified")
		}
		if in.Event != "steer" {
			continue
		}
		if a.ID == "" {
			return nil, incomplete("TRANSCRIPT_PROVENANCE_UNKNOWN", "steer receipt has no identity")
		}
		if in.Captured {
			if in.ProviderInputText == "" {
				return nil, incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "captured steer has no native input")
			}
		} else {
			if len(in.Content) > 0 {
				return nil, incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "legacy non-text steer requires identity evidence")
			}
			in.ProviderInputText = in.Text
		}
		if in.ProviderInputText == "" {
			return nil, incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "steer input is unknown")
		}
		if len(in.Members) == 0 && in.SourceTurnID != "" {
			in.Members = []string{in.SourceTurnID}
		}
		details[a.ID] = in
	}
	return details, nil
}

func verifySteerInputs(s domain.TranscriptSourceSnapshot, segments []ports.NativeTranscriptSegment, links []CoverageLink, members map[string]string, details map[string]steerInput) ([]SteerCoverage, map[string]string, error) {
	leadLinks := map[string]CoverageLink{}
	for _, link := range links {
		leadLinks[link.AOTurnID] = link
	}
	byNative := map[string]ports.NativeTranscriptSegment{}
	for _, seg := range segments {
		byNative[seg.NativeID] = seg
	}
	var receipts []SteerCoverage
	coveredMembers := map[string]string{}
	used := map[string]bool{}
	for _, activity := range s.Activities {
		in, ok := details[activity.ID]
		if !ok {
			continue
		}
		link, ok := leadLinks[activity.TurnID]
		if !ok {
			return nil, nil, incomplete("TRANSCRIPT_PROVENANCE_UNKNOWN", "steer receipt has no verified main dispatch")
		}
		seg := byNative[link.NativeID]
		matches := []string{}
		selected := map[string]bool{}
		for _, id := range seg.MainRecordIDs {
			selected[id] = true
		}
		for _, record := range seg.Records {
			if record.Excluded != "" || record.Provenance != domain.NativeThreadMain || !selected[record.ID] || !domain.NativeCheckpointTextMatches(in.ProviderInputText, recordText(record)) {
				continue
			}
			queued := record.Type == "attachment" && record.AttachmentType == "queued_command"
			storedUser := in.StoredOnTurn && record.Type == "user" && (record.PromptID == link.NativePromptID || record.ID == link.NativePromptID)
			if queued || storedUser {
				matches = append(matches, record.ID)
			}
		}
		if len(matches) != 1 {
			return nil, nil, incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "steer native input is missing or ambiguous")
		}
		key := link.NativeID + "\x00" + matches[0]
		if used[key] {
			return nil, nil, incomplete("TRANSCRIPT_COVERAGE_INCOMPLETE", "two receipts claim one native steer input")
		}
		used[key] = true
		for _, member := range in.Members {
			if members[member] != activity.TurnID || coveredMembers[member] != "" {
				return nil, nil, incomplete("TRANSCRIPT_PROVENANCE_UNKNOWN", "steer member receipt conflicts with dispatch")
			}
			coveredMembers[member] = activity.ID
		}
		receipts = append(receipts, SteerCoverage{ActivityID: activity.ID, DispatchTurnID: activity.TurnID, NativeID: link.NativeID, NativeRecordID: matches[0], MemberTurnIDs: in.Members})
	}
	return receipts, coveredMembers, nil
}
