package transcriptexport

import (
	"context"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type fixtureReader map[string]ports.NativeTranscriptSegment

func (f fixtureReader) ReadNativeTranscript(_ context.Context, r ports.NativeTranscriptRequest) (ports.NativeTranscriptSegment, error) {
	return f[r.Source.NativeSessionID], nil
}
func sourceFixture() (domain.TranscriptSourceSnapshot, []EpochCutoff, fixtureReader) {
	s := domain.TranscriptSourceSnapshot{Session: domain.SessionRecord{ID: "source", ProjectID: "project", Kind: domain.KindOrchestrator, Harness: domain.HarnessClaudeCode}, Fingerprint: "snapshot", CutoffSequence: 10}
	var cuts []EpochCutoff
	reader := fixtureReader{}
	for _, id := range []string{"epoch1", "epoch2"} {
		s.NativeSessions = append(s.NativeSessions, domain.TranscriptNativeSession{Harness: domain.HarnessClaudeCode, NativeID: id, ConfigDir: "/owned"})
		cuts = append(cuts, EpochCutoff{NativeID: id, Bytes: 100, LeafID: "a", Schema: ports.ClaudeTranscriptSchemaV1})
		reader[id] = ports.NativeTranscriptSegment{NativeID: id, ByteCutoff: 100, LeafID: "a", SHA256: strings.Repeat("a", 64), Schema: ports.ClaudeTranscriptSchemaV1, ReaderVersion: "fixture/1", MainRecordIDs: []string{"u", "a"}, Counts: ports.TranscriptCounts{Records: 2, MainRecords: 2, UserRecords: 1, AssistantRecords: 1}, Records: []ports.NativeTranscriptRecord{{ID: "u", PromptID: "prompt", NativeID: id, Type: "user", Provenance: domain.NativeThreadMain, Blocks: []ports.NativeTranscriptBlock{{Kind: ports.TranscriptText, Text: "same question"}}}, {ID: "a", ParentID: "u", NativeID: id, Type: "assistant", Provenance: domain.NativeThreadMain, Blocks: []ports.NativeTranscriptBlock{{Kind: ports.TranscriptText, Text: "same answer"}}}}}
	}
	return s, cuts, reader
}
func TestTranscriptSourceCoverageMatrix(t *testing.T) {
	s, cuts, r := sourceFixture()
	out, err := Build(context.Background(), s, cuts, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Manifest.Segments) != 2 || out.Manifest.Coverage != "complete_for_scope" || strings.Count(string(out.Transcript), "same answer") != 2 {
		t.Fatal("epochs or repeated identities lost")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*domain.TranscriptSourceSnapshot, *[]EpochCutoff)
	}{
		{"missing epoch", func(_ *domain.TranscriptSourceSnapshot, c *[]EpochCutoff) { *c = (*c)[:1] }},
		{"non Claude history", func(s *domain.TranscriptSourceSnapshot, _ *[]EpochCutoff) {
			s.NativeSessions[0].Harness = domain.HarnessCodex
		}},
		{"unknown root", func(s *domain.TranscriptSourceSnapshot, _ *[]EpochCutoff) {
			s.Terminal = []domain.TranscriptTerminalTurn{{Harness: domain.HarnessClaudeCode}}
		}},
		{"unsupported source", func(s *domain.TranscriptSourceSnapshot, _ *[]EpochCutoff) { s.Session.Harness = domain.HarnessCodex }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c, r := sourceFixture()
			tc.mutate(&s, &c)
			if _, e := Build(context.Background(), s, c, r); e == nil {
				t.Fatal("incomplete source accepted")
			}
		})
	}
}
func TestTranscriptChildCannotDischargeMainCoverage(t *testing.T) {
	s, c, r := sourceFixture()
	s.Branches = []domain.ConversationBranch{{ID: "branch", ProviderConversationID: "epoch2"}}
	s.Turns = []domain.ConversationTurn{{ID: "turn", BranchID: "branch", ProviderTurnID: "missing", State: domain.TurnStateCompleted}}
	s.NativeThreads = []domain.ConversationNativeThreadEvidence{{BranchID: "branch", ProviderTurnID: "missing", ProviderConversationID: "epoch2"}}
	if _, err := Build(context.Background(), s, c, r); err == nil {
		t.Fatal("missing main turn accepted")
	}
	s.NativeThreads[0].ProviderConversationID = "child"
	s.Bindings = []domain.UsageBindingRecord{{ID: 1, Harness: domain.HarnessClaudeCode, NativeRootID: "epoch2"}}
	s.Sources = []domain.UsageSourceRecord{{BindingID: 1, Kind: domain.UsageSourceClaudeSubagent, NativeSessionID: "epoch2", SubagentID: "child"}}
	out, err := Build(context.Background(), s, c, r)
	if err != nil {
		t.Fatal(err)
	}
	if out.Manifest.ExcludedChildTurns != 1 {
		t.Fatal("child exclusion not counted")
	}
	s.NativeThreads = nil
	if _, err := Build(context.Background(), s, c, r); err == nil {
		t.Fatal("unknown provenance treated as child")
	}
}

func TestTranscriptCoverageCorrelatesLegacyACPOnlyWhenUnique(t *testing.T) {
	s, c, r := sourceFixture()
	s.Branches = []domain.ConversationBranch{{ID: "branch", ProviderConversationID: "epoch2"}}
	s.Turns = []domain.ConversationTurn{{ID: "ao-turn", BranchID: "branch", ProviderTurnID: "acp-local-turn", State: domain.TurnStateCompleted}}
	s.NativeThreads = []domain.ConversationNativeThreadEvidence{{BranchID: "branch", ProviderTurnID: "acp-local-turn", ProviderConversationID: "epoch2"}}
	s.Messages = []domain.ConversationMessage{{ID: "ao-u", TurnID: "ao-turn", Role: domain.MessageRoleUser, Text: "same question"}, {ID: "ao-a", TurnID: "ao-turn", Role: domain.MessageRoleAssistant, Text: "same answer"}}
	out, e := Build(context.Background(), s, c, r)
	if e != nil {
		t.Fatal(e)
	}
	if len(out.Manifest.CoveredTurns) != 1 {
		t.Fatal("legacy ACP correlation missing")
	}
	seg := r["epoch2"]
	seg.Records = append(seg.Records, ports.NativeTranscriptRecord{ID: "u2", PromptID: "prompt2", Type: "user", NativeID: "epoch2", Provenance: domain.NativeThreadMain, Blocks: []ports.NativeTranscriptBlock{{Kind: ports.TranscriptText, Text: "same question"}}}, ports.NativeTranscriptRecord{ID: "a2", Type: "assistant", NativeID: "epoch2", Provenance: domain.NativeThreadMain, Blocks: []ports.NativeTranscriptBlock{{Kind: ports.TranscriptText, Text: "same answer"}}})
	seg.MainRecordIDs = append(seg.MainRecordIDs, "u2", "a2")
	r["epoch2"] = seg
	if _, e = Build(context.Background(), s, c, r); e == nil {
		t.Fatal("ambiguous repeated pair used as identity")
	}
}

func TestTranscriptOldOwnedEpochIsNotMistakenForChild(t *testing.T) {
	s, c, r := sourceFixture()
	s.Branches = []domain.ConversationBranch{{ID: "branch", ProviderConversationID: "epoch2"}}
	s.Turns = []domain.ConversationTurn{{ID: "old-turn", BranchID: "branch", ProviderTurnID: "prompt", State: domain.TurnStateCompleted}}
	s.NativeThreads = []domain.ConversationNativeThreadEvidence{{BranchID: "branch", ProviderTurnID: "prompt", ProviderConversationID: "epoch1"}}
	out, e := Build(context.Background(), s, c, r)
	if e != nil {
		t.Fatal(e)
	}
	if out.Manifest.ExcludedChildTurns != 0 || len(out.Manifest.CoveredTurns) != 1 || out.Manifest.CoverageLinks[0].NativeID != "epoch1" {
		t.Fatal("old main epoch was silently excluded")
	}
	s.NativeThreads[0].ProviderConversationID = "unclassified-thread"
	if _, e = Build(context.Background(), s, c, r); e == nil {
		t.Fatal("unknown thread assumed to be a child")
	}
}

func TestTranscriptRequiresTransitionEpochsAndSettledInterface(t *testing.T) {
	s, c, r := sourceFixture()
	s.Transitions = []domain.SessionInterfaceTransition{{NativeConversationID: "missing-old-root", Phase: domain.SessionInterfaceTransitionCompleted}}
	if _, e := Build(context.Background(), s, c, r); e == nil {
		t.Fatal("transition's old epoch omitted")
	}
	s.Transitions[0].NativeConversationID = "epoch1"
	s.Transitions[0].Phase = domain.SessionInterfaceTransitionRequested
	if _, e := Build(context.Background(), s, c, r); e == nil {
		t.Fatal("active interface transition accepted")
	}
}

func TestTranscriptObservedButUnflushedInputRefusesComplete(t *testing.T) {
	s, c, r := sourceFixture()
	s.Session.Metadata.NativeCheckpointEvidence = domain.AppendNativeCheckpoint("", "epoch1", domain.NativeCheckpointObservation{Generation: "generation", PromptID: "not-flushed", Text: "missing reply"})
	if _, e := Build(context.Background(), s, c, r); e == nil {
		t.Fatal("unflushed Stop observation ignored")
	}
	s.Session.Metadata.NativeCheckpointEvidence = domain.AppendNativeCheckpoint("", "epoch1", domain.NativeCheckpointObservation{Generation: "generation", PromptID: "prompt", Text: "same answer"})
	if _, e := Build(context.Background(), s, c, r); e != nil {
		t.Fatal(e)
	}
	s.Session.Metadata.ConversationCheckpointUnsettled = true
	if _, e := Build(context.Background(), s, c, r); e == nil {
		t.Fatal("uncorrelated native observation ignored")
	}
}

func TestTranscriptQueuedSubmissionRequiresAncestryBoundNonce(t *testing.T) {
	s, c, r := sourceFixture()
	s.Session.Metadata.NativeCheckpointEvidence = domain.AppendNativeCheckpoint("", "epoch1", domain.NativeCheckpointObservation{Generation: "g", Submission: true, SubmissionID: "nonce", Text: "same question"})
	if _, e := Build(context.Background(), s, c, r); e == nil {
		t.Fatal("submission accepted before hook attachment flush")
	}
	seg := r["epoch1"]
	seg.Records = append(seg.Records, ports.NativeTranscriptRecord{ID: "hook", ParentID: "u", NativeID: "epoch1", Type: "attachment", AttachmentType: "hook_additional_context", HookEvent: "UserPromptSubmit", Provenance: domain.NativeThreadMain, Blocks: []ports.NativeTranscriptBlock{{Kind: ports.TranscriptText, Text: domain.NativeSubmissionContext("nonce")}}})
	seg.Records[1].ParentID = "hook"
	seg.MainRecordIDs = []string{"u", "hook", "a"}
	r["epoch1"] = seg
	if _, e := Build(context.Background(), s, c, r); e != nil {
		t.Fatal(e)
	}
	seg.Records[2].ParentID = "absent"
	r["epoch1"] = seg
	if _, e := Build(context.Background(), s, c, r); e == nil {
		t.Fatal("orphan nonce accepted")
	}
}
func TestTranscriptTimeRangeUsesInstants(t *testing.T) {
	s, c, r := sourceFixture()
	first := r["epoch1"]
	first.Records[0].Timestamp = "2026-10-28T12:00:00+08:00"
	first.Records[1].Timestamp = "2026-10-28T12:01:00+08:00"
	r["epoch1"] = first
	second := r["epoch2"]
	second.Records[0].Timestamp = "2026-10-28T03:00:00Z"
	second.Records[1].Timestamp = "2026-10-28T05:00:00Z"
	r["epoch2"] = second
	out, e := Build(context.Background(), s, c, r)
	if e != nil {
		t.Fatal(e)
	}
	if out.Manifest.TimeStart != "2026-10-28T03:00:00Z" || out.Manifest.TimeEnd != "2026-10-28T05:00:00Z" {
		t.Fatalf("time range uses string order: %s %s", out.Manifest.TimeStart, out.Manifest.TimeEnd)
	}
}
