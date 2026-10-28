package transcriptexport

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestTranscriptBatchMembersUseCapturedInputAndReceipts(t *testing.T) {
	s, c, r := sourceFixture()
	s.Branches = []domain.ConversationBranch{{ID: "b", ProviderConversationID: "epoch1"}}
	s.Turns = []domain.ConversationTurn{{ID: "lead", BranchID: "b", ProviderTurnID: "opaque", ProviderInputText: "exact combined input", State: domain.TurnStateCompleted}, {ID: "member", BranchID: "b", State: domain.TurnStateCompleted}}
	s.Inputs = []domain.TranscriptTurnInput{{TurnID: "lead", ProviderInputText: "exact combined input"}, {TurnID: "member", PromotedToTurnID: "lead", BatchedInput: true}}
	s.NativeThreads = []domain.ConversationNativeThreadEvidence{{BranchID: "b", ProviderTurnID: "opaque", ProviderConversationID: "epoch1"}}
	s.Messages = []domain.ConversationMessage{{ID: "m1", TurnID: "lead", Role: domain.MessageRoleUser, Text: "part one"}, {ID: "m2", TurnID: "member", Role: domain.MessageRoleUser, Text: "part two"}}
	seg := r["epoch1"]
	seg.Records[0].Blocks[0].Text = "exact combined input"
	r["epoch1"] = seg
	out, e := Build(context.Background(), s, c, r)
	if e != nil {
		t.Fatal(e)
	}
	if len(out.Manifest.CoveredTurns) != 2 || len(out.Manifest.CoverageLinks) != 2 || strings.Count(string(out.Transcript), "exact combined input") != 1 {
		t.Fatal("batch messages lost or native body duplicated")
	}
	s.Inputs[1].PromotedToTurnID = "missing"
	if _, e = Build(context.Background(), s, c, r); e == nil {
		t.Fatal("unowned dispatch accepted")
	}
}
func TestTranscriptSteerReceiptMustMatchOwnNativeQueuedInput(t *testing.T) {
	s, c, r := sourceFixture()
	s.Branches = []domain.ConversationBranch{{ID: "b", ProviderConversationID: "epoch1"}}
	s.Turns = []domain.ConversationTurn{{ID: "lead", BranchID: "b", ProviderTurnID: "prompt", State: domain.TurnStateCompleted}, {ID: "member", BranchID: "b", State: domain.TurnStateCompleted}}
	s.Inputs = []domain.TranscriptTurnInput{{TurnID: "member", PromotedToTurnID: "lead", BatchedInput: true}}
	s.NativeThreads = []domain.ConversationNativeThreadEvidence{{BranchID: "b", ProviderTurnID: "prompt", ProviderConversationID: "epoch1"}}
	detail, _ := json.Marshal(map[string]any{"event": "steer", "providerInputCaptured": true, "providerInputText": "exact steer input", "providerInputSourceTurnIds": []string{"member"}})
	s.Activities = []domain.ConversationActivity{{ID: "steer", TurnID: "lead", Kind: domain.ActivityKindSystem, Status: domain.ActivityStatusCompleted, Detail: detail}}
	if _, e := Build(context.Background(), s, c, r); e == nil {
		t.Fatal("unflushed steer accepted")
	}
	seg := r["epoch1"]
	seg.Records = append(seg.Records, ports.NativeTranscriptRecord{ID: "q", ParentID: "u", NativeID: "epoch1", Type: "attachment", AttachmentType: "queued_command", Provenance: domain.NativeThreadMain, Blocks: []ports.NativeTranscriptBlock{{Kind: ports.TranscriptText, Text: "exact steer input"}}})
	seg.MainRecordIDs = []string{"u", "q", "a"}
	seg.Records[1].ParentID = "q"
	r["epoch1"] = seg
	out, e := Build(context.Background(), s, c, r)
	if e != nil {
		t.Fatal(e)
	}
	if len(out.Manifest.Steers) != 1 || len(out.Manifest.CoveredTurns) != 2 {
		t.Fatal("steer receipt not covered")
	}
	seg.Records[2].Provenance = domain.NativeThreadChild
	seg.Records[2].Excluded = "child"
	seg.MainRecordIDs = []string{"u", "a"}
	r["epoch1"] = seg
	if _, e = Build(context.Background(), s, c, r); e == nil {
		t.Fatal("child input discharged steer coverage")
	}
}

func TestTranscriptStoredSteerDoesNotProveUnlistedMember(t *testing.T) {
	s, c, r := sourceFixture()
	s.Branches = []domain.ConversationBranch{{ID: "b", ProviderConversationID: "epoch1"}}
	s.Turns = []domain.ConversationTurn{{ID: "lead", BranchID: "b", ProviderTurnID: "prompt", ProviderInputText: "steer", State: domain.TurnStateCompleted}, {ID: "member", BranchID: "b", State: domain.TurnStateCompleted}}
	s.Inputs = []domain.TranscriptTurnInput{{TurnID: "lead", ProviderInputText: "steer"}, {TurnID: "member", PromotedToTurnID: "lead", BatchedInput: true}}
	s.NativeThreads = []domain.ConversationNativeThreadEvidence{{BranchID: "b", ProviderTurnID: "prompt", ProviderConversationID: "epoch1"}}
	detail, _ := json.Marshal(map[string]any{"event": "steer", "providerInputCaptured": true, "providerInputStoredOnTurn": true, "providerInputText": "steer"})
	s.Activities = []domain.ConversationActivity{{ID: "steer", TurnID: "lead", Kind: domain.ActivityKindSystem, Status: domain.ActivityStatusCompleted, Detail: detail}}
	seg := r["epoch1"]
	seg.Records = append(seg.Records, ports.NativeTranscriptRecord{ID: "q", NativeID: "epoch1", Type: "attachment", AttachmentType: "queued_command", Provenance: domain.NativeThreadMain, Blocks: []ports.NativeTranscriptBlock{{Kind: ports.TranscriptText, Text: "steer"}}})
	seg.MainRecordIDs = []string{"u", "q", "a"}
	r["epoch1"] = seg
	if _, e := Build(context.Background(), s, c, r); e == nil {
		t.Fatal("stored steer field falsely proved an unlisted member")
	}
	detail, _ = json.Marshal(map[string]any{"event": "steer", "providerInputCaptured": true, "providerInputStoredOnTurn": true, "providerInputText": "steer", "providerInputSourceTurnIds": []string{"member"}})
	s.Activities[0].Detail = detail
	out, e := Build(context.Background(), s, c, r)
	if e != nil {
		t.Fatal(e)
	}
	if len(out.Manifest.CoveredTurns) != 2 || len(out.Manifest.Steers) != 1 || strings.Count(string(out.Transcript), "\nsteer\n") != 1 {
		t.Fatal("stored steer lost its member receipt or duplicated the guidance")
	}
}
