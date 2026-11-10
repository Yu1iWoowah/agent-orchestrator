package rca

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

type windowReader struct {
	source     io.ReaderAt
	start, end int64
	violation  bool
}

func (r *windowReader) ReadAt(p []byte, off int64) (int, error) {
	if off < r.start || off+int64(len(p)) > r.end {
		r.violation = true
		return 0, io.ErrUnexpectedEOF
	}
	return r.source.ReadAt(p, off)
}

func TestCodexWindowDoesNotProbeOutsideBudget(t *testing.T) {
	input := header() + strings.Repeat("x", 1000) + "\n"
	r := &windowReader{source: strings.NewReader(input), start: int64(len(input)) - 200, end: int64(len(input))}
	_ = readCodex(t.Context(), r, int64(len(input)), syntheticNativeID, readLimits{Window: 200, Line: 256, Depth: 32})
	if r.violation {
		t.Fatal("read outside authorized byte window")
	}
}

const syntheticNativeID = "11111111-1111-4111-8111-111111111111"

func header() string {
	return `{"type":"session_meta","payload":{"id":"` + syntheticNativeID + `","cli_version":"0.167.0","source":"cli","base_instructions":{"text":"規則"},"dynamic_tools":[{"name":"private_tool","input_schema":{"type":"object"}}]}}` + "\n"
}

func readSynthetic(t *testing.T, input string, limits readLimits) recording {
	t.Helper()
	return readCodex(context.Background(), strings.NewReader(input), int64(len(input)), syntheticNativeID, limits)
}

func TestCodexRecordedContentAndEOF(t *testing.T) {
	input := header() + `{"type":"turn_context","payload":{"turn_id":"private-turn-key"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"abcd"}]}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"A_SYNTHETIC_CREDENTIAL_CANARY"}]}}` + "\n" +
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"private-call","output":"B_CONTENT_CANARY"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"reason"}],"encrypted_content":"C_ENCRYPTED_CANARY"}}` + "\n"
	r := readSynthetic(t, input, defaultReadLimits)
	if r.Status != "partial" || measured(r.Buckets[0].Bytes) != 10 || measured(r.Buckets[2].Bytes) != 29 || measured(r.Buckets[3].Bytes) != 16 || measured(r.Buckets[4].Bytes) != 6 || len(r.Turns) != 1 {
		t.Fatalf("unexpected numeric projection: %+v", r)
	}
	for _, b := range r.Buckets {
		if b.Bytes != nil && measured(b.EstimatedTokens) != (measured(b.Bytes)+3)/4 {
			t.Fatalf("wrong estimate: %+v", b)
		}
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{"A_SYNTHETIC", "B_CONTENT", "private-", "private_tool", syntheticNativeID, "C_ENCRYPTED_CANARY"} {
		if strings.Contains(string(encoded), canary) {
			t.Fatal("projection contains external content")
		}
	}
	// Captured EOF excludes later appends and re-reading replaces, never adds usage.
	appended := input + `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"later"}]}}` + "\n"
	again := readCodex(context.Background(), strings.NewReader(appended), int64(len(input)), syntheticNativeID, defaultReadLimits)
	if !reflect.DeepEqual(again.Buckets, r.Buckets) || again.EndOffset != int64(len(input)) {
		t.Fatal("read beyond captured EOF")
	}
}

func TestCodexRecordDepthAndIncompleteTail(t *testing.T) {
	base := header()
	limits := readLimits{Window: 1 << 20, Line: int64(len(base)) + 1, Depth: 32}
	oversized := strings.Repeat("x", len(base)+1) + "\n"
	deep := `{"x":` + strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34) + "}\n"
	valid := `{"type":"response_item","payload":{"type":"function_call_output","output":"kept"}}` + "\n"
	r := readSynthetic(t, base+oversized+deep+"not-json\n"+valid+`{"unfinished":`, limits)
	for _, reason := range []string{"record_limit", "parse_limit", "invalid_record", "incomplete_tail"} {
		if !hasReason(r, reason) {
			t.Errorf("missing %s", reason)
		}
	}
	if measured(r.Buckets[3].Bytes) != 4 || r.Status != "partial" {
		t.Fatalf("lost later valid record: %+v", r)
	}
}

func TestCodexWindowNeverBackfillsHeader(t *testing.T) {
	input := header() + strings.Repeat(" ", 1000) + "\n" + `{"type":"response_item","payload":{"type":"function_call_output","output":"not admitted"}}` + "\n"
	r := readSynthetic(t, input, readLimits{Window: 200, Line: 256, Depth: 32})
	if r.Status != "unavailable" || !hasReason(r, "window_head_gap") || !hasReason(r, "source_version_unknown") || r.Buckets[3].Bytes != nil {
		t.Fatalf("tail window bypassed header proof: %+v", r)
	}
}

func TestCodexToolCatalogUpdatesAndQuality(t *testing.T) {
	update := `{"type":"dynamic_tools_updated","payload":{"thread_id":"` + syntheticNativeID + `","tools":[{"name":"private-schema","input_schema":{"type":"object"}}]}}` + "\n"
	additional := `{"type":"response_item","payload":{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"private-additional"}]}}` + "\n"
	r := readSynthetic(t, header()+update+additional+`{"type":"compacted","payload":{"message":"short"}}`+"\n"+`{"type":"future_type","payload":{"private":"not classified"}}`+"\n", defaultReadLimits)
	if measured(r.Buckets[1].Bytes) <= measured(readSynthetic(t, header(), defaultReadLimits).Buckets[1].Bytes) || !hasReason(r, "compaction_gap") || !hasReason(r, "unknown_schema") {
		t.Fatal("catalog or quality not observed")
	}
	if r.Buckets[5].Bytes != nil {
		t.Fatal("invented unknown content")
	}
}

func TestCodexRejectsIdentityVersionAndSubagent(t *testing.T) {
	for _, tc := range []struct{ old, replacement, reason string }{
		{"0.167.0", "0.168.0", "unsupported_source_version"},
		{syntheticNativeID, "22222222-2222-4222-8222-222222222222", "source_identity_mismatch"},
		{`"source":"cli"`, `"source":{"subagent":{"parent_thread_id":"private"}}`, "subagent_source"},
	} {
		r := readSynthetic(t, strings.Replace(header(), tc.old, tc.replacement, 1), defaultReadLimits)
		if r.Status != "unavailable" || !hasReason(r, tc.reason) {
			t.Errorf("wrong admission: %+v", r)
		}
		for _, b := range r.Buckets {
			if b.Bytes != nil {
				t.Fatal("rejected source aggregated")
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := readCodex(ctx, strings.NewReader(header()), int64(len(header())), syntheticNativeID, defaultReadLimits)
	if r.Status != "unavailable" || !hasReason(r, "canceled") {
		t.Fatal("canceled read admitted")
	}
}

func measured(n *int64) int64 {
	if n == nil {
		return -1
	}
	return *n
}

func TestCodexMissingAndKnownEmptyAreDifferent(t *testing.T) {
	r := readSynthetic(t, header(), defaultReadLimits)
	if r.Buckets[3].Bytes != nil || r.Buckets[3].EstimatedTokens != nil {
		t.Fatal("missing result became zero")
	}
	r = readSynthetic(t, header()+`{"type":"response_item","payload":{"type":"function_call_output","output":""}}`+"\n", defaultReadLimits)
	if measured(r.Buckets[3].Bytes) != 0 || measured(r.Buckets[3].EstimatedTokens) != 0 {
		t.Fatal("explicit empty result is unknown")
	}
}

func TestCodexExactSchemaFragmentAndUnknownContentQuality(t *testing.T) {
	definition := `[{ "name":"<>&工具", "input_schema":{ "type":"object" }}]`
	source := `{"type":"session_meta","payload":{"id":"` + syntheticNativeID + `","cli_version":"0.167.0","source":"vscode","dynamic_tools":` + definition + `},"future_field":"B_SYNTHETIC_FIELD"}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_image","image_url":"A_SYNTHETIC_URL"}]}}` + "\n"
	r := readSynthetic(t, source, defaultReadLimits)
	if measured(r.Buckets[1].Bytes) != int64(len(definition)) {
		t.Fatal("schema was reencoded instead of measured")
	}
	if !hasReason(r, "unknown_fields") || !hasReason(r, "unsupported_content") || r.Buckets[2].Bytes != nil {
		t.Fatal("unknown content became measured zero")
	}
}

func TestCodexRepeatedCurrentTurnContextKeepsOneOrdinal(t *testing.T) {
	contextLine := `{"type":"turn_context","payload":{"turn_id":"A_SYNTHETIC_TURN_KEY"}}` + "\n"
	message := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"abcd"}]}}` + "\n"
	r := readSynthetic(t, header()+contextLine+message+contextLine+message, defaultReadLimits)
	if len(r.Turns) != 1 || measured(r.Turns[0].Buckets[2].Bytes) != 8 {
		t.Fatal("repeated current turn context split ordinal")
	}
}

func TestCodexDoesNotRepairInvalidUTF8(t *testing.T) {
	input := header() + `{"type":"response_item","payload":{"type":"function_call_output","output":"` + string([]byte{0xff}) + `"}}` + "\n"
	r := readSynthetic(t, input, defaultReadLimits)
	if !hasReason(r, "invalid_record") || r.Buckets[3].Bytes != nil {
		t.Fatal("invalid UTF-8 was repaired into measured content")
	}
}

func TestCodexForkTruncationAndUnknownRetainedContext(t *testing.T) {
	input := strings.Replace(header(), `"source":"cli"`, `"source":"cli","forked_from_id":"A_SYNTHETIC_PARENT"`, 1) +
		`{"type":"response_item","payload":{"type":"function_call_output","output":"kept","truncated":true}}` + "\n" +
		`{"type":"retained_context","payload":{"unknown":"B_SYNTHETIC_CONTEXT"}}` + "\n"
	r := readSynthetic(t, input, defaultReadLimits)
	for _, reason := range []string{"fork_context_unknown", "source_truncated", "retained_context_unknown"} {
		if !hasReason(r, reason) {
			t.Fatalf("missing %s", reason)
		}
	}
	if r.Status != "partial" || measured(r.Buckets[3].Bytes) != 4 || r.Buckets[5].Bytes != nil {
		t.Fatal("invented retained context or lost observed result")
	}
}

func TestCodexUnknownPayloadFieldsStayUnclassified(t *testing.T) {
	for _, key := range []string{"future_payload", "name call_id"} {
		input := header() + `{"type":"response_item","payload":{"type":"function_call_output","output":"kept","` + key + `":"A_SYNTHETIC_FIELD B_CONTENT_FIELD"}}` + "\n"
		r := readSynthetic(t, input, defaultReadLimits)
		if !hasReason(r, "unknown_fields") || r.Status != "partial" || measured(r.Buckets[3].Bytes) != 4 || r.Buckets[5].Bytes != nil {
			t.Fatal("unknown payload was silently accepted or guessed")
		}
	}
}
