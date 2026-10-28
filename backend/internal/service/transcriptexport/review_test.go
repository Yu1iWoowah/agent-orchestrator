package transcriptexport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	gmhtml "github.com/yuin/goldmark/renderer/html"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestTranscriptTerminalTurnsWithoutProviderIdentityAreAOOnly(t *testing.T) {
	for _, state := range []domain.TurnState{domain.TurnStateCancelled, domain.TurnStateFailed, domain.TurnStateInterrupted, domain.TurnStateRecovered, domain.TurnStateCompleted} {
		t.Run(string(state), func(t *testing.T) {
			s, cuts, reader := sourceFixture()
			s.Turns = []domain.ConversationTurn{{ID: "undispatched", State: state}}
			s.Messages = []domain.ConversationMessage{{ID: "source-message", TurnID: "undispatched", Role: domain.MessageRoleUser, Text: "preserve AO-only request"}}
			out, err := Build(context.Background(), s, cuts, reader)
			if err != nil {
				t.Fatal(err)
			}
			if out.Manifest.AOOnlyMessages != 1 || len(out.Manifest.CoveredTurns) != 0 || !strings.Contains(string(out.Transcript), "preserve AO-only request") || !strings.Contains(string(out.Transcript), "native delivery: unconfirmed") {
				t.Fatal("undelivered terminal message lost or counted as native coverage")
			}
		})
	}
}

func TestTranscriptAOOnlyInvalidAttachmentRefusesComplete(t *testing.T) {
	for _, payload := range []string{`{broken`, `[{"type":"future-binary","data":"unknown"}]`, `[{"type":"image","mimeType":123}]`} {
		s, cuts, reader := sourceFixture()
		s.Turns = []domain.ConversationTurn{{ID: "queued", State: domain.TurnStateQueued}}
		s.Messages = []domain.ConversationMessage{{ID: "m", TurnID: "queued", DeliveryContentJSON: payload}}
		if _, err := Build(context.Background(), s, cuts, reader); err == nil {
			t.Fatal("unverifiable AO attachment accepted", payload)
		}
	}
}

func TestTranscriptAOOnlyEmbeddedURIUsesContentReference(t *testing.T) {
	s, cuts, reader := sourceFixture()
	s.Turns = []domain.ConversationTurn{{ID: "queued", State: domain.TurnStateQueued}}
	s.Messages = []domain.ConversationMessage{{ID: "m", TurnID: "queued", DeliveryContentJSON: `[{"type":"resource","mimeType":"application/pdf","uri":"data:application/pdf;base64,DO_NOT_INLINE_BINARY"}]`}}
	out, err := Build(context.Background(), s, cuts, reader)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out.Transcript), "DO_NOT_INLINE_BINARY") {
		t.Fatal("embedded URI payload expanded instead of content reference")
	}
	if !strings.Contains(string(out.Transcript), "embeddedUriReference") || !strings.Contains(string(out.Transcript), "sha256:") {
		t.Fatal("embedded URI reference lost")
	}
}

func assertNoInjectedMetadataMarkup(t *testing.T, markdown []byte) {
	t.Helper()
	var rendered bytes.Buffer
	if err := goldmark.New(goldmark.WithRendererOptions(gmhtml.WithUnsafe())).Convert(markdown, &rendered); err != nil {
		t.Fatal(err)
	}
	html := rendered.String()
	if strings.Contains(html, "<img ") || strings.Contains(html, "<a href=") || strings.Contains(html, "<h1>INJECTED") {
		t.Fatalf("metadata created active Markdown elements: %s", html)
	}
	if !strings.Contains(html, "INJECTED") {
		t.Fatal("metadata was discarded instead of safely represented")
	}
}

func TestTranscriptMetadataCannotCreateMarkdownElements(t *testing.T) {
	attack := "\n# INJECTED\n![trap](https://example.invalid/image)\n<a href=\"https://example.invalid\">trap</a>`"
	s, cuts, reader := sourceFixture()
	s.Session.ID = domain.SessionID("source" + attack)
	s.Session.ProjectID = domain.ProjectID("project" + attack)
	for id, seg := range reader {
		seg.Records[0].ID = "u" + attack
		seg.Records[0].PromptID = "prompt" + attack
		seg.Records[1].ParentID = seg.Records[0].ID
		seg.MainRecordIDs[0] = seg.Records[0].ID
		reader[id] = seg
	}
	out, err := Build(context.Background(), s, cuts, reader)
	if err != nil {
		t.Fatal(err)
	}
	assertNoInjectedMetadataMarkup(t, out.Transcript)
	if out.Manifest.Segments[0].Records[0].ID != "u"+attack {
		t.Fatal("raw identity changed in ledger")
	}
	for _, block := range []ports.NativeTranscriptBlock{
		{Kind: ports.TranscriptToolCall, ToolName: "Read" + attack, CallID: "call" + attack, Text: "{}", Status: "requested"},
		{Kind: ports.TranscriptToolResult, CallID: "call" + attack, Status: "succeeded"},
		{Kind: ports.TranscriptAttachment, Reference: "reference" + attack},
	} {
		t.Run(string(block.Kind), func(t *testing.T) { assertNoInjectedMetadataMarkup(t, []byte(renderBlock(block))) })
	}
}

func TestTranscriptRecoveredProviderTurnStillRequiresCoverage(t *testing.T) {
	s, cuts, reader := sourceFixture()
	s.Branches = []domain.ConversationBranch{{ID: "b", ProviderConversationID: "epoch1"}}
	s.Turns = []domain.ConversationTurn{{ID: "recovered", BranchID: "b", ProviderTurnID: "prompt", State: domain.TurnStateRecovered}}
	s.NativeThreads = []domain.ConversationNativeThreadEvidence{{BranchID: "b", ProviderTurnID: "prompt", ProviderConversationID: "epoch1"}}
	out, err := Build(context.Background(), s, cuts, reader)
	if err != nil || len(out.Manifest.CoveredTurns) != 1 {
		t.Fatalf("recovered native turn not covered: %v", err)
	}
	s.Turns[0].ProviderTurnID = "missing"
	s.NativeThreads[0].ProviderTurnID = "missing"
	if _, err := Build(context.Background(), s, cuts, reader); err == nil {
		t.Fatal("missing recovered provider turn accepted")
	}
}

func TestTranscriptAOOnlyAttachmentReferencesArePreserved(t *testing.T) {
	s, cuts, reader := sourceFixture()
	s.Turns = []domain.ConversationTurn{{ID: "queued", State: domain.TurnStateQueued}}
	payload := `[{"type":"image","mimeType":"image/png","data":"BINARY_PAYLOAD_MUST_NOT_LEAK","name":"fixture.png"},{"type":"resource","uri":"file:///fixture/document.pdf","mimeType":"application/pdf"}]`
	s.Messages = []domain.ConversationMessage{{ID: "queued-message", TurnID: "queued", Text: "pending with attachments", DeliveryContentJSON: payload}}
	out, err := Build(context.Background(), s, cuts, reader)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out.Transcript)
	for _, want := range []string{"image", "image/png", "fixture.png", "file:///fixture/document.pdf", "sha256:"} {
		if !strings.Contains(text, want) {
			t.Errorf("AO-only attachment reference missing %q", want)
		}
	}
	if strings.Contains(text, "BINARY_PAYLOAD_MUST_NOT_LEAK") {
		t.Fatal("binary payload expanded into transcript")
	}
	encoded, err := json.Marshal(out.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	var ledger struct {
		Count       int `json:"aoOnlyAttachments"`
		Supplements []struct {
			MessageID   string                             `json:"messageId"`
			Attachments []struct{ Type, Reference string } `json:"attachments"`
		} `json:"aoSupplements"`
	}
	if err = json.Unmarshal(encoded, &ledger); err != nil {
		t.Fatal(err)
	}
	if ledger.Count != 2 || len(ledger.Supplements) != 1 || ledger.Supplements[0].MessageID != "queued-message" || len(ledger.Supplements[0].Attachments) != 2 {
		t.Fatalf("attachment ledger incomplete: %s", encoded)
	}
	var raw []json.RawMessage
	if err = json.Unmarshal([]byte(payload), &raw); err != nil {
		t.Fatal(err)
	}
	if ledger.Supplements[0].Attachments[0].Type != "image" || ledger.Supplements[0].Attachments[0].Reference != fmt.Sprintf("sha256:%x", sha256.Sum256(raw[0])) {
		t.Fatal("attachment type or content reference changed")
	}
}

func TestTranscriptCapturedInputUsesOwnedNativeIdentityToDisambiguate(t *testing.T) {
	turn := domain.ConversationTurn{ID: "turn", ProviderTurnID: "p2", ProviderInputText: "repeated"}
	prompts := []nativePrompt{{ID: "p1", User: "repeated"}, {ID: "p2", User: "repeated"}}
	link, err := correlateTurn(turn, nil, map[string]bool{"p1": true, "p2": true}, prompts)
	if err != nil || link.NativePromptID != "p2" {
		t.Fatalf("owned identity did not disambiguate captured input: %+v %v", link, err)
	}
	prompts[1].User = "different captured input"
	if _, err = correlateTurn(turn, nil, map[string]bool{"p1": true, "p2": true}, prompts); err == nil {
		t.Fatal("known identity silently fell back to a different matching prompt")
	}
}

func TestTranscriptReaderCannotSubstituteAnotherRequiredEpoch(t *testing.T) {
	s, cuts, reader := sourceFixture()
	reader["epoch1"] = reader["epoch2"]
	if _, err := Build(context.Background(), s, cuts, reader); err == nil {
		t.Fatal("reader substituted one epoch twice and still claimed completeness")
	}
}

func TestTranscriptQueuedSubmissionTraversesMetadataLineage(t *testing.T) {
	s, cuts, reader := sourceFixture()
	s.Session.Metadata.NativeCheckpointEvidence = domain.AppendNativeCheckpoint("", "epoch1", domain.NativeCheckpointObservation{Generation: "gen", SubmissionID: "nonce", Submission: true, Text: "queued text"})
	seg := reader["epoch1"]
	seg.Records = append(seg.Records,
		ports.NativeTranscriptRecord{ID: "q", ParentID: "u", NativeID: "epoch1", Type: "attachment", AttachmentType: "queued_command", Provenance: domain.NativeThreadMain, Blocks: []ports.NativeTranscriptBlock{{Kind: ports.TranscriptText, Text: "queued text"}}},
		ports.NativeTranscriptRecord{ID: "hook", ParentID: "q", NativeID: "epoch1", Type: "progress", Provenance: domain.NativeThreadMain, Excluded: "metadata", Meta: true},
		ports.NativeTranscriptRecord{ID: "nonce", ParentID: "hook", NativeID: "epoch1", Type: "attachment", AttachmentType: "hook_additional_context", HookEvent: "UserPromptSubmit", Provenance: domain.NativeThreadMain, Blocks: []ports.NativeTranscriptBlock{{Kind: ports.TranscriptText, Text: domain.NativeSubmissionContext("nonce")}}},
	)
	seg.MainRecordIDs = []string{"u", "q", "nonce", "a"}
	reader["epoch1"] = seg
	if _, err := Build(context.Background(), s, cuts, reader); err != nil {
		t.Fatal(err)
	}
	seg.Records[3].Provenance = domain.NativeThreadChild
	reader["epoch1"] = seg
	if _, err := Build(context.Background(), s, cuts, reader); err == nil {
		t.Fatal("child metadata discharged a main submission proof")
	}
}
