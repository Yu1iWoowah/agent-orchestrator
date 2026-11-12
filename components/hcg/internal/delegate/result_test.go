package delegate

import (
	"encoding/json"
	"fmt"
	"reflect"
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

// Each reachable verifier refusal keeps its first failing check privately and
// the public reply stays the same blocked projection as before.
func TestVerifyReplyNamesFirstFailurePrivately(t *testing.T) {
	source := "first line\nsecond line\n"
	baseScope := func() preparedScope {
		return preparedScope{Sources: []sourceSnapshot{{SourceRange: SourceRange{ID: "s0000", Path: "prompt.txt", Revision: "r1", SHA256: textHash(source), Start: 1, End: 3}, text: source}}}
	}
	baseAudit := func() PayloadAudit {
		return PayloadAudit{Coverage: "complete", Sources: []SourceRange{baseScope().Sources[0].SourceRange}, Omissions: []Omission{}}
	}
	baseProfile := Profile{BillingProvider: "openai", Model: "fixture", ID: "fixture", Revision: "r1"}
	mutate := func(change func(map[string]any)) string {
		var v map[string]any
		if err := json.Unmarshal([]byte(validProposal(source)), &v); err != nil {
			t.Fatal(err)
		}
		change(v)
		b, _ := json.Marshal(v)
		return string(b)
	}
	evidence := func(v map[string]any) map[string]any { return v["evidence"].([]any)[0].(map[string]any) }
	finding := func(v map[string]any) map[string]any { return v["findings"].([]any)[0].(map[string]any) }
	notFound := `{"schema":"hcg.delegate-read/1","status":"not_found","findings":[],"evidence":[]}`
	type tc struct {
		raw     string
		limit   int
		scope   func(*preparedScope)
		audit   func(*PayloadAudit)
		profile func(*Profile)
		want    replyFailure
		reason  string
	}
	many := func(key string, n int, item func(int) any) string {
		return mutate(func(v map[string]any) {
			items := []any{}
			for i := range n {
				items = append(items, item(i))
			}
			v[key] = items
		})
	}
	offset := func(n int64) *int64 { return &n }
	cases := map[string]tc{
		"output_limit_invalid":     {raw: validProposal(source), limit: -1, want: replyFailure{Stage: "precheck", Code: "output_limit_invalid", Limit: intp(-1)}},
		"raw_completion_too_large": {raw: strings.Repeat(" ", 65537), want: replyFailure{Stage: "precheck", Code: "raw_completion_too_large", Length: intp(65537), Limit: intp(65536)}},
		"raw_sensitive_pattern":    {raw: "HCG_CANARY_DO_NOT_SEND", want: replyFailure{Stage: "precheck", Code: "raw_sensitive_pattern"}},
		"raw_invalid_utf8":         {raw: "\xff", want: replyFailure{Stage: "precheck", Code: "raw_invalid_utf8"}},
		"raw_control_character":    {raw: "\x01", want: replyFailure{Stage: "precheck", Code: "raw_control_character"}},
		"payload_coverage":         {raw: validProposal(source), audit: func(a *PayloadAudit) { a.Coverage = "partial" }, want: replyFailure{Stage: "precheck", Code: "payload_coverage_incomplete", Coverage: "partial"}},
		"json_syntax_invalid":      {raw: `{"schema" 1}`, want: replyFailure{Stage: "decode", Code: "json_syntax_invalid", Offset: offset(11)}},
		"json_type_invalid":        {raw: mutate(func(v map[string]any) { evidence(v)["line_start"] = "1" }), want: replyFailure{Stage: "decode", Code: "json_type_invalid", Field: "evidence.line_start", Index: intp(0)}},
		"json_unknown_field":       {raw: mutate(func(v map[string]any) { v["HCG_SECRET_FIELD_NAME"] = 1 }), want: replyFailure{Stage: "decode", Code: "json_unknown_field"}},
		"json_decode_failed":       {raw: `{"schema":`, want: replyFailure{Stage: "decode", Code: "json_decode_failed"}},
		"json_trailing_value":      {raw: validProposal(source) + " {}", want: replyFailure{Stage: "envelope", Code: "json_trailing_value"}},
		"schema_mismatch":          {raw: mutate(func(v map[string]any) { v["schema"] = "other" }), want: replyFailure{Stage: "envelope", Code: "schema_mismatch", Field: "schema"}},
		"status_invalid":           {raw: mutate(func(v map[string]any) { v["status"] = "maybe" }), want: replyFailure{Stage: "envelope", Code: "status_invalid", Field: "status"}},
		"findings_null":            {raw: mutate(func(v map[string]any) { v["findings"] = nil }), want: replyFailure{Stage: "envelope", Code: "findings_null_or_missing", Field: "findings"}},
		"evidence_missing":         {raw: mutate(func(v map[string]any) { delete(v, "evidence") }), want: replyFailure{Stage: "envelope", Code: "evidence_null_or_missing", Field: "evidence"}},
		"proposal_sensitive":       {raw: strings.Replace(validProposal(source), "fixture-answer", jsonEscape("H")+"CG_CANARY_DO_NOT_SEND", 1), want: replyFailure{Stage: "envelope", Code: "proposal_sensitive_pattern"}},
		"proposal_control":         {raw: strings.Replace(validProposal(source), "fixture-answer", jsonEscape("\x01"), 1), want: replyFailure{Stage: "envelope", Code: "proposal_control_character"}},
		"not_found_findings":       {raw: mutate(func(v map[string]any) { v["status"] = "not_found" }), want: replyFailure{Stage: "not_found", Code: "not_found_findings_nonempty", Field: "findings", Count: intp(1)}},
		"not_found_evidence":       {raw: mutate(func(v map[string]any) { v["status"], v["findings"] = "not_found", []any{} }), want: replyFailure{Stage: "not_found", Code: "not_found_evidence_nonempty", Field: "evidence", Count: intp(1)}},
		"not_found_public":         {raw: notFound, profile: func(p *Profile) { p.Model = "HCG_CANARY_DO_NOT_SEND" }, want: replyFailure{Stage: "not_found", Code: "public_reply_sensitive"}},
		"not_found_size":           {raw: notFound, limit: 10, reason: "response_size_limit", want: replyFailure{Stage: "not_found", Code: "response_size_limit", Limit: intp(10)}},
		"findings_empty":           {raw: mutate(func(v map[string]any) { v["findings"] = []any{} }), want: replyFailure{Stage: "counts", Code: "findings_empty", Field: "findings", Count: intp(0)}},
		"findings_count_limit":     {raw: many("findings", 33, func(i int) any { return finding(map[string]any{"findings": []any{map[string]any{}}}) }), want: replyFailure{Stage: "counts", Code: "findings_count_limit", Field: "findings", Count: intp(33), Limit: intp(32)}},
		"evidence_empty":           {raw: mutate(func(v map[string]any) { v["evidence"] = []any{} }), want: replyFailure{Stage: "counts", Code: "evidence_empty", Field: "evidence", Count: intp(0)}},
		"evidence_count_limit":     {raw: many("evidence", 65, func(int) any { return map[string]any{} }), want: replyFailure{Stage: "counts", Code: "evidence_count_limit", Field: "evidence", Count: intp(65), Limit: intp(64)}},
		"evidence_source_unknown":  {raw: mutate(func(v map[string]any) { evidence(v)["source_id"] = "s9999" }), want: replyFailure{Stage: "evidence", Code: "evidence_source_unknown", Field: "evidence.source_id", Index: intp(0)}},
		"evidence_id_invalid":      {raw: mutate(func(v map[string]any) { evidence(v)["id"] = "-bad" }), want: replyFailure{Stage: "evidence", Code: "evidence_id_invalid", Field: "evidence.id", Index: intp(0), SourceID: "s0000"}},
		"evidence_id_duplicate": {raw: mutate(func(v map[string]any) {
			v["evidence"] = append(v["evidence"].([]any), evidence(v))
		}), want: replyFailure{Stage: "evidence", Code: "evidence_id_duplicate", Field: "evidence.id", Index: intp(1), SourceID: "s0000"}},
		"evidence_revision_mismatch": {raw: mutate(func(v map[string]any) { evidence(v)["revision"] = "r2" }), want: replyFailure{Stage: "evidence", Code: "evidence_revision_mismatch", Field: "evidence.revision", Index: intp(0), SourceID: "s0000"}},
		"evidence_sha256_mismatch":   {raw: mutate(func(v map[string]any) { evidence(v)["content_sha256"] = strings.Repeat("b", 64) }), want: replyFailure{Stage: "evidence", Code: "evidence_source_sha256_mismatch", Field: "evidence.content_sha256", Index: intp(0), SourceID: "s0000"}},
		"line_start_invalid":         {raw: mutate(func(v map[string]any) { evidence(v)["line_start"] = 0 }), want: replyFailure{Stage: "evidence", Code: "line_start_invalid", Field: "evidence.line_start", Index: intp(0), SourceID: "s0000"}},
		"line_end_before_start":      {raw: mutate(func(v map[string]any) { evidence(v)["line_start"] = 2 }), want: replyFailure{Stage: "evidence", Code: "line_end_before_start", Field: "evidence.line_end", Index: intp(0), SourceID: "s0000"}},
		"line_end_out_of_range":      {raw: mutate(func(v map[string]any) { evidence(v)["line_end"] = 4 }), want: replyFailure{Stage: "evidence", Code: "line_end_out_of_range", Field: "evidence.line_end", Index: intp(0), SourceID: "s0000"}},
		"quote_empty":                {raw: mutate(func(v map[string]any) { evidence(v)["quote"] = "" }), want: replyFailure{Stage: "evidence", Code: "quote_empty", Field: "evidence.quote", Index: intp(0), SourceID: "s0000"}},
		"quote_sha256_mismatch":      {raw: mutate(func(v map[string]any) { evidence(v)["quote_sha256"] = strings.Repeat("c", 64) }), want: replyFailure{Stage: "evidence", Code: "quote_sha256_mismatch", Field: "evidence.quote_sha256", Index: intp(0), SourceID: "s0000"}},
		"source_snapshot_sha256":     {raw: validProposal(source), scope: func(s *preparedScope) { s.Sources[0].originalText = "changed\n" }, want: replyFailure{Stage: "source_view", Code: "source_snapshot_sha256_mismatch", Index: intp(0), SourceID: "s0000"}},
		"source_view_mismatch":       {raw: validProposal(source), scope: func(s *preparedScope) { s.Sources[0].originalText, s.Sources[0].text = source, "other view" }, want: replyFailure{Stage: "source_view", Code: "source_view_mismatch", Index: intp(0), SourceID: "s0000"}},
		"source_line_map_mismatch":   {raw: validProposal(source), scope: func(s *preparedScope) { s.Sources[0].End = 4 }, want: replyFailure{Stage: "source_view", Code: "source_line_map_mismatch", Index: intp(0), SourceID: "s0000"}},
		"quote_text_mismatch": {raw: mutate(func(v map[string]any) {
			evidence(v)["quote"], evidence(v)["quote_sha256"] = "fabricated", textHash("fabricated")
		}), want: replyFailure{Stage: "source_view", Code: "quote_text_mismatch", Field: "evidence.quote", Index: intp(0), SourceID: "s0000"}},
		"finding_id_invalid": {raw: mutate(func(v map[string]any) { finding(v)["id"] = "" }), want: replyFailure{Stage: "findings", Code: "finding_id_invalid", Field: "findings.id", Index: intp(0)}},
		"finding_id_duplicate": {raw: mutate(func(v map[string]any) {
			v["findings"] = append(v["findings"].([]any), finding(v))
		}), want: replyFailure{Stage: "findings", Code: "finding_id_duplicate", Field: "findings.id", Index: intp(1)}},
		"finding_kind_invalid":  {raw: mutate(func(v map[string]any) { finding(v)["kind"] = "opinion" }), want: replyFailure{Stage: "findings", Code: "finding_kind_invalid", Field: "findings.kind", Index: intp(0)}},
		"finding_text_empty":    {raw: mutate(func(v map[string]any) { finding(v)["text"] = "  " }), want: replyFailure{Stage: "findings", Code: "finding_text_empty", Field: "findings.text", Index: intp(0)}},
		"finding_text_limit":    {raw: mutate(func(v map[string]any) { finding(v)["text"] = strings.Repeat("x", 4097) }), want: replyFailure{Stage: "findings", Code: "finding_text_limit", Field: "findings.text", Index: intp(0), Length: intp(4097), Limit: intp(4096)}},
		"finding_refs_empty":    {raw: mutate(func(v map[string]any) { finding(v)["evidence_ids"] = []any{} }), want: replyFailure{Stage: "findings", Code: "finding_refs_empty", Field: "findings.evidence_ids", Index: intp(0)}},
		"finding_ref_unknown":   {raw: mutate(func(v map[string]any) { finding(v)["evidence_ids"] = []any{"e1", "e9"} }), want: replyFailure{Stage: "findings", Code: "finding_ref_unknown", Field: "findings.evidence_ids", Index: intp(0), RefIndex: intp(1)}},
		"finding_ref_duplicate": {raw: mutate(func(v map[string]any) { finding(v)["evidence_ids"] = []any{"e1", "e1"} }), want: replyFailure{Stage: "findings", Code: "finding_ref_duplicate", Field: "findings.evidence_ids", Index: intp(0), RefIndex: intp(1)}},
		"final_public":          {raw: validProposal(source), profile: func(p *Profile) { p.Model = "HCG_CANARY_DO_NOT_SEND" }, want: replyFailure{Stage: "final", Code: "public_reply_sensitive"}},
		"final_size":            {raw: validProposal(source), limit: 256, reason: "response_size_limit", want: replyFailure{Stage: "final", Code: "response_size_limit", Limit: intp(256)}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			scope, audit, profile := baseScope(), baseAudit(), baseProfile
			if c.scope != nil {
				c.scope(&scope)
			}
			if c.audit != nil {
				c.audit(&audit)
			}
			if c.profile != nil {
				c.profile(&profile)
			}
			out, failure, err := verifyReply(time.Unix(0, 0), c.raw, testExecution, profile, Receipt{ProfileDigest: strings.Repeat("a", 64)}, audit, scope, c.limit)
			if err == nil || failure == nil || out.Status != "blocked" || out.Answer != "" || len(out.Findings) != 0 || len(out.Evidence) != 0 {
				t.Fatalf("refusal changed: %+v %+v %v", out, failure, err)
			}
			want := c.want
			if want.Code == "response_size_limit" {
				want.Length = failure.Length
				if failure.Length == nil || *failure.Length <= *want.Limit {
					t.Fatalf("size failure without encoded length: %+v", failure)
				}
			}
			if want.Code == "json_type_invalid" {
				want.Offset = failure.Offset
				if failure.Offset == nil {
					t.Fatal("typed decode offset lost")
				}
			}
			if !reflect.DeepEqual(*failure, want) {
				got, _ := json.Marshal(failure)
				exp, _ := json.Marshal(want)
				t.Fatalf("first failure=%s want=%s", got, exp)
			}
			reason := c.reason
			if reason == "" {
				reason = "unverified"
			}
			encoded, _ := json.Marshal(out)
			if out.Reason != reason || (want.Code != reason && strings.Contains(string(encoded), want.Code)) || strings.Contains(string(encoded), "HCG_SECRET_FIELD_NAME") {
				t.Fatalf("private diagnostic crossed public reply: %s", encoded)
			}
		})
	}
	// Exactly-at-limit public replies still pass; diagnostics add no public bytes.
	for _, raw := range []string{validProposal(source), notFound} {
		out, failure, err := verifyReply(time.Unix(0, 0), raw, testExecution, baseProfile, Receipt{}, baseAudit(), baseScope(), 0)
		if err != nil || failure != nil {
			t.Fatalf("valid reply refused: %+v %v", failure, err)
		}
		encoded, _ := json.Marshal(out)
		if again, failure, err := verifyReply(time.Unix(0, 0), raw, testExecution, baseProfile, Receipt{}, baseAudit(), baseScope(), len(encoded)); err != nil || failure != nil || !reflect.DeepEqual(again, out) {
			t.Fatalf("exact limit changed: %+v %v", failure, err)
		}
		if _, failure, err := verifyReply(time.Unix(0, 0), raw, testExecution, baseProfile, Receipt{}, baseAudit(), baseScope(), len(encoded)-1); err == nil || failure.Code != "response_size_limit" {
			t.Fatalf("one byte over limit accepted: %+v %v", failure, err)
		}
	}
}

// jsonEscape writes each byte as a JSON unicode escape without using the raw byte.
func jsonEscape(s string) string {
	out := ""
	for _, b := range []byte(s) {
		out += fmt.Sprintf("%cu%04x", 92, b)
	}
	return out
}
