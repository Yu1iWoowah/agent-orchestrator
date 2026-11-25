package delegate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadReplyDecodedStringsCannotEscapeSafetyScan(t *testing.T) {
	source := "public line\n"
	scope := preparedScope{Sources: []sourceSnapshot{{SourceRange: SourceRange{ID: "s0000", Path: "prompt.txt", Revision: scopeRevision, SHA256: textHash(source), Start: 1, End: 2}, text: source}}}
	audit := PayloadAudit{Coverage: "complete", Sources: []SourceRange{scope.Sources[0].SourceRange}}
	profile := budgetProfile("openai")
	for _, tc := range []struct{ name, old, replacement string }{
		{"escaped_canary", `"fixture-answer"`, `"\u0048CG_CANARY_DO_NOT_SEND"`},
		{"escaped_assignment_pattern", `"fixture-answer"`, `"\u0070assword=SYNTHETIC_DENY_PATTERN"`},
		{"escaped_control", `"fixture-answer"`, `"public\u0001line"`},
		{"escaped_finding_id", `"id":"f1"`, `"id":"\u0048CG_CANARY_DO_NOT_SEND"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(validProposal(), tc.old, tc.replacement, 1)
			if !safeText(raw) {
				t.Fatal("fixture was blocked before JSON decoding")
			}
			out, _, err := verifyReply(time.Unix(0, 0), raw, testExecution, profile, Receipt{}, audit, scope, 0)
			if err == nil || out.Status != "blocked" || out.Answer != "" || len(out.Findings) != 0 || len(out.Evidence) != 0 {
				t.Fatal("decoded unsafe output was returned or partial text retained")
			}
		})
	}
	execution := testExecution
	execution.SessionID = "HCG_CANARY_DO_NOT_SEND"
	out, _, err := verifyReply(time.Unix(0, 0), validProposal(), execution, profile, Receipt{}, audit, scope, 0)
	if err == nil || out.Status != "blocked" || out.SessionID != "" {
		t.Fatal("final reply metadata bypassed safety scan")
	}
}

func TestNormalizedLinesReconstructOriginalQuotes(t *testing.T) {
	for _, family := range []string{"openai"} {
		for _, newline := range []string{"\r\n", "\r"} {
			name := "CR"
			if newline == "\r\n" {
				name = "CRLF"
			}
			t.Run(family+"/"+name, func(t *testing.T) {
				raw := "alpha" + newline + "beta" + newline
				source := filepath.Join(t.TempDir(), "prompt.txt")
				if err := os.WriteFile(source, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
				proposal, _ := json.Marshal(map[string]any{"schema": resultSchema, "status": "ok", "findings": []Finding{{ID: "f1", Kind: "fact", Text: "public finding", EvidenceIDs: []string{"e1"}}}, "evidence": []proposedEvidence{{ID: "e1", SourceID: "s0000", LineStart: 1, LineEnd: 2}}})
				sink := &receiptSink{}
				calls := 0
				profile := budgetProfile(family)
				consumer := Consumer{Recorder: sink, Transport: transportFunc(func(context.Context, WireRequest) (WireResponse, error) {
					calls++
					return providerFixtureReply(profile.Model, string(proposal)), nil
				})}
				out, err := consumer.DelegateRead(t.Context(), testExecution, profile, approvedRequest(t, &consumer, "QUESTION_MUST_SURVIVE", source))
				if err != nil || out.Reply.Status != "ok" || calls != 1 || len(out.Reply.Evidence) != 1 {
					t.Fatalf("valid line-range proposal rejected: status=%s calls=%d err=%v", out.Reply.Status, calls, err)
				}
				evidence := out.Reply.Evidence[0]
				original := "alpha" + newline + "beta"
				if evidence.Quote != original || evidence.QuoteSHA256 != textHash(original) || evidence.ContentSHA256 != textHash(raw) || evidence.LineStart != 1 || evidence.LineEnd != 2 {
					t.Fatal("original bytes/hash/line mapping were not retained")
				}
			})
		}
	}
}
