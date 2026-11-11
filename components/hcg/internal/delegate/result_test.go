package delegate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validProposal(source string) string {
	b, _ := json.Marshal(map[string]any{"schema": resultSchema, "status": "ok", "findings": []Finding{{ID: "f1", Kind: "fact", Text: "fixture-answer", EvidenceIDs: []string{"e1"}}}, "evidence": []proposedEvidence{{ID: "e1", SourceID: "s0000", Revision: "r1", ContentSHA256: textHash(source), LineStart: 1, LineEnd: 1, Quote: strings.Split(source, "\n")[0], QuoteSHA256: textHash(strings.Split(source, "\n")[0])}}})
	return string(b)
}
func TestReadReplyEvidenceAndScope(t *testing.T) {
	source := "first line\nsecond line\n"
	scope := preparedScope{Sources: []sourceSnapshot{{SourceRange: SourceRange{ID: "s0000", Path: "prompt.txt", Revision: "r1", SHA256: textHash(source), Start: 1, End: 3}, text: source}}}
	audit := PayloadAudit{Coverage: "complete", Sources: []SourceRange{scope.Sources[0].SourceRange}, Omissions: []Omission{}}
	p := Profile{BillingProvider: "openai", Model: "fixture", ID: "fixture", Revision: "r1"}
	receipt := Receipt{RequestID: "request", ProfileDigest: strings.Repeat("a", 64)}
	raw := validProposal(source)
	verify := func(raw string, limit int) (ReadReply, error) {
		out, _, err := verifyReply(time.Unix(0, 0), raw, testExecution, p, receipt, audit, scope, limit)
		return out, err
	}
	out, err := verify(raw, 0)
	if err != nil || out.Status != "ok" || out.Answer != "[fact] fixture-answer" || out.Evidence[0].Path != "prompt.txt" || out.Evidence[0].Verification != "machine_checked" || out.ProjectID != testExecution.ProjectID || out.Coverage.Status != "complete" || out.Coverage.RequestedScopeDigest != out.Coverage.InjectedScopeDigest {
		t.Fatalf("reply=%+v %v", out, err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"fake_source":    func(v map[string]any) { v["evidence"].([]any)[0].(map[string]any)["source_id"] = "s9999" },
		"wrong_revision": func(v map[string]any) { v["evidence"].([]any)[0].(map[string]any)["revision"] = "r2" },
		"wrong_hash": func(v map[string]any) {
			v["evidence"].([]any)[0].(map[string]any)["content_sha256"] = strings.Repeat("b", 64)
		},
		"wrong_line": func(v map[string]any) { v["evidence"].([]any)[0].(map[string]any)["line_start"] = 2 },
		"wrong_quote": func(v map[string]any) {
			e := v["evidence"].([]any)[0].(map[string]any)
			e["quote"] = "fabricated"
			e["quote_sha256"] = textHash("fabricated")
		},
		"no_evidence":           func(v map[string]any) { v["findings"].([]any)[0].(map[string]any)["evidence_ids"] = []string{} },
		"fake_evidence_ref":     func(v map[string]any) { v["findings"].([]any)[0].(map[string]any)["evidence_ids"] = []string{"fake"} },
		"self_claimed_coverage": func(v map[string]any) { v["coverage_status"] = "complete" },
		"sensitive_answer":      func(v map[string]any) { v["findings"].([]any)[0].(map[string]any)["text"] = "HCG_CANARY_DO_NOT_SEND" },
	} {
		t.Run(name, func(t *testing.T) {
			var v map[string]any
			if err := json.Unmarshal([]byte(raw), &v); err != nil {
				t.Fatal(err)
			}
			mutate(v)
			b, _ := json.Marshal(v)
			out, err := verify(string(b), 0)
			if err == nil || out.Status != "blocked" || out.Answer != "" || len(out.Evidence) != 0 {
				t.Fatalf("unsafe proposal accepted: %+v %v", out, err)
			}
		})
	}
	var withoutHash map[string]any
	if err := json.Unmarshal([]byte(raw), &withoutHash); err != nil {
		t.Fatal(err)
	}
	delete(withoutHash["evidence"].([]any)[0].(map[string]any), "quote_sha256")
	b, _ := json.Marshal(withoutHash)
	computed, err := verify(string(b), 0)
	if err != nil || computed.Evidence[0].QuoteSHA256 != textHash("first line") {
		t.Fatalf("AO quote hash missing: %+v %v", computed, err)
	}
	scope.Sources[0].originalText = "first line\r\nsecond line\r\n"
	if reply, err := verify(raw, 0); err == nil || reply.Status != "blocked" {
		t.Fatal("normalized text silently replaced original quote bytes")
	}
	scope.Sources[0].originalText = ""
	out, err = verify(raw, 256)
	if err == nil || out.Status != "blocked" || out.Reason != "response_size_limit" || len(out.Omissions) == 0 {
		t.Fatalf("oversized result accepted: %+v %v", out, err)
	}
	notFound := `{"schema":"hcg.delegate-read/1","status":"not_found","findings":[],"evidence":[]}`
	out, err = verify(notFound, 0)
	if err != nil || out.Status != "not_found" || out.Reason != "not_found_in_injected_scope" {
		t.Fatalf("not_found=%+v %v", out, err)
	}
	audit.Coverage = "partial"
	out, err = verify(notFound, 0)
	if err == nil || out.Status != "blocked" {
		t.Fatalf("incomplete scope accepted not_found: %+v %v", out, err)
	}
}
