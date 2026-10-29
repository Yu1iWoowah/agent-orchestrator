package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func testAttentionPayload() AttentionQuestionPayload {
	return AttentionQuestionPayload{
		SchemaVersion: 1, Title: "Release decision",
		Explanation: "Choose the release scope because it determines the next change.",
		Questions: []AttentionPrompt{{
			Key: "Q1", Header: "Scope", Question: "Which scope should we use?",
			Options: []AttentionQuestionOption{
				{ID: "first", Label: "Small", Description: "Ship the focused change."},
				{ID: "second", Label: "Large", Description: "Include the related change."},
			},
		}},
	}
}

func attentionJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func attentionText(s string) *string { return &s }

func TestAttentionPayloadDefaultsAndGrouping(t *testing.T) {
	payload := testAttentionPayload()
	raw := attentionJSON(t, payload)
	explicit := bytes.Replace(raw, []byte(`"header":`), []byte(`"multiSelect":false,"header":`), 1)
	explicit = bytes.Replace(explicit, []byte(`"label":`), []byte(`"recommended":false,"label":`), 1)
	withDefaults, err := ParseAttentionQuestionPayload(explicit, AttentionQuestionKindQuestion)
	if err != nil {
		t.Fatal(err)
	}
	omitted := raw
	got, err := ParseAttentionQuestionPayload(omitted, AttentionQuestionKindQuestion)
	if err != nil {
		t.Fatal(err)
	}
	if string(attentionJSON(t, got)) != string(attentionJSON(t, withDefaults)) {
		t.Fatalf("omitted false defaults changed payload: %s", attentionJSON(t, got))
	}
	for _, count := range []int{1, 4} {
		value := testAttentionPayload()
		for len(value.Questions) < count {
			q := testAttentionPayload().Questions[0]
			q.Key = fmt.Sprintf("Q%d", len(value.Questions)+1)
			value.Questions = append(value.Questions, q)
		}
		value.Questions[0].Options[0].Recommended = true
		value.Questions[0].MultiSelect = true
		if _, err := ParseAttentionQuestionPayload(attentionJSON(t, value), AttentionQuestionKindQuestion); err != nil {
			t.Fatalf("%d questions: %v", count, err)
		}
	}
}

func TestAttentionPayloadRejectsInvalidStructure(t *testing.T) {
	tests := []struct {
		name, field string
		change      func(*AttentionQuestionPayload)
	}{
		{"zero questions", "questions", func(p *AttentionQuestionPayload) { p.Questions = nil }},
		{"five questions", "questions", func(p *AttentionQuestionPayload) {
			for i := 1; i < 5; i++ {
				q := testAttentionPayload().Questions[0]
				q.Key = fmt.Sprintf("Q%d", i+1)
				p.Questions = append(p.Questions, q)
			}
		}},
		{"one option", "options", func(p *AttentionQuestionPayload) { p.Questions[0].Options = p.Questions[0].Options[:1] }},
		{"five options", "options", func(p *AttentionQuestionPayload) {
			for i := 2; i < 5; i++ {
				p.Questions[0].Options = append(p.Questions[0].Options, AttentionQuestionOption{ID: fmt.Sprint(i), Label: fmt.Sprint(i), Description: "A distinct option."})
			}
		}},
		{"duplicate question key", "key", func(p *AttentionQuestionPayload) { p.Questions = append(p.Questions, p.Questions[0]) }},
		{"duplicate option id", "id", func(p *AttentionQuestionPayload) { p.Questions[0].Options[1].ID = "first" }},
		{"duplicate label", "label", func(p *AttentionQuestionPayload) { p.Questions[0].Options[1].Label = "Small" }},
		{"reserved other label", "label", func(p *AttentionQuestionPayload) { p.Questions[0].Options[0].Label = "Other" }},
		{"reserved Chinese label", "label", func(p *AttentionQuestionPayload) { p.Questions[0].Options[0].Label = "其他" }},
		{"reserved other id", "id", func(p *AttentionQuestionPayload) { p.Questions[0].Options[0].ID = "__other__" }},
		{"non-ASCII key", "key", func(p *AttentionQuestionPayload) { p.Questions[0].Key = "問題" }},
		{"key with whitespace", "key", func(p *AttentionQuestionPayload) { p.Questions[0].Key = " Q1" }},
		{"long key", "key", func(p *AttentionQuestionPayload) { p.Questions[0].Key = strings.Repeat("a", 65) }},
		{"long id", "id", func(p *AttentionQuestionPayload) { p.Questions[0].Options[0].ID = strings.Repeat("a", 65) }},
		{"nonfirst recommendation", "recommended", func(p *AttentionQuestionPayload) { p.Questions[0].Options[1].Recommended = true }},
		{"two recommendations", "recommended", func(p *AttentionQuestionPayload) {
			p.Questions[0].Options[0].Recommended = true
			p.Questions[0].Options[1].Recommended = true
		}},
		{"unknown version", "schemaVersion", func(p *AttentionQuestionPayload) { p.SchemaVersion = 2 }},
		{"missing title", "title", func(p *AttentionQuestionPayload) { p.Title = " \t" }},
		{"missing explanation", "explanation", func(p *AttentionQuestionPayload) { p.Explanation = "" }},
		{"empty optional explanation", "explanation", func(p *AttentionQuestionPayload) { p.Questions[0].Explanation = attentionText("") }},
		{"missing description", "description", func(p *AttentionQuestionPayload) { p.Questions[0].Options[0].Description = "" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := testAttentionPayload()
			tc.change(&payload)
			_, err := ParseAttentionQuestionPayload(attentionJSON(t, payload), AttentionQuestionKindQuestion)
			var validation *AttentionValidationError
			if !errors.As(err, &validation) || (validation.Field != tc.field && !strings.HasSuffix(validation.Field, "."+tc.field)) {
				t.Fatalf("want %s validation error, got %v", tc.field, err)
			}
		})
	}
	allowed := testAttentionPayload()
	allowed.Questions[0].Key = strings.Repeat("A", 64)
	allowed.Questions[0].Options[0].ID = strings.Repeat("B", 64)
	allowed.Questions[0].Options = append(allowed.Questions[0].Options,
		AttentionQuestionOption{ID: "third", Label: "Third", Description: "Third."},
		AttentionQuestionOption{ID: "fourth", Label: "Fourth", Description: "Fourth."})
	if _, err := ParseAttentionQuestionPayload(attentionJSON(t, allowed), AttentionQuestionKindQuestion); err != nil {
		t.Fatal(err)
	}
}

func TestAttentionPayloadUnicodeBounds(t *testing.T) {
	tests := []struct {
		field string
		max   int
		set   func(*AttentionQuestionPayload, string)
	}{
		{"title", 120, func(p *AttentionQuestionPayload, s string) { p.Title = s }},
		{"explanation", 8000, func(p *AttentionQuestionPayload, s string) { p.Explanation = s }},
		{"header", 24, func(p *AttentionQuestionPayload, s string) { p.Questions[0].Header = s }},
		{"question", 2000, func(p *AttentionQuestionPayload, s string) { p.Questions[0].Question = s }},
		{"explanation", 4000, func(p *AttentionQuestionPayload, s string) { p.Questions[0].Explanation = &s }},
		{"label", 80, func(p *AttentionQuestionPayload, s string) { p.Questions[0].Options[0].Label = s }},
		{"description", 1000, func(p *AttentionQuestionPayload, s string) { p.Questions[0].Options[0].Description = s }},
		{"preview", 8192, func(p *AttentionQuestionPayload, s string) {
			p.Questions[0].Options[0].Preview = &AttentionPreview{Format: "text", Content: s}
		}},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s-%d", tc.field, tc.max), func(t *testing.T) {
			for _, n := range []int{1, tc.max, tc.max + 1} {
				p := testAttentionPayload()
				tc.set(&p, strings.Repeat("🚀", n))
				_, err := ParseAttentionQuestionPayload(attentionJSON(t, p), AttentionQuestionKindQuestion)
				if n <= tc.max && err != nil {
					t.Fatalf("%d Unicode code points: %v", n, err)
				}
				if n > tc.max && (err == nil || !strings.Contains(err.Error(), tc.field)) {
					t.Fatalf("%d code points: want %s error, got %v", n, tc.field, err)
				}
			}
		})
	}
}

func TestAttentionPayloadStrictJSONAndBytes(t *testing.T) {
	raw := attentionJSON(t, testAttentionPayload())
	raw = bytes.Replace(raw, []byte(`"header":`), []byte(`"multiSelect":false,"header":`), 1)
	raw = bytes.Replace(raw, []byte(`"label":`), []byte(`"recommended":false,"label":`), 1)
	padded := append(bytes.Clone(raw), bytes.Repeat([]byte(" "), 128*1024-len(raw))...)
	if _, err := ParseAttentionQuestionPayload(padded, AttentionQuestionKindQuestion); err != nil {
		t.Fatalf("128 KiB valid JSON: %v", err)
	}
	tests := map[string][]byte{
		"over 128 KiB":         append(padded, ' '),
		"unknown field":        bytes.Replace(raw, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":1,"originSessionId":"forged"`), 1),
		"unknown nested field": bytes.Replace(raw, []byte(`"multiSelect":false`), []byte(`"multiSelect":false,"foreign":true`), 1),
		"duplicate member":     bytes.Replace(raw, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":1,"schemaVersion":1`), 1),
		"null boolean":         bytes.Replace(raw, []byte(`"multiSelect":false`), []byte(`"multiSelect":null`), 1),
		"wrong boolean":        bytes.Replace(raw, []byte(`"recommended":false`), []byte(`"recommended":"false"`), 1),
		"trailing object":      append(bytes.Clone(raw), []byte("{}")...),
		"invalid UTF-8":        append(bytes.Clone(raw), 0xff),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if bytes.Equal(input, raw) {
				t.Fatal("invalid fixture mutation did not change JSON")
			}
			if _, err := ParseAttentionQuestionPayload(input, AttentionQuestionKindQuestion); err == nil {
				t.Fatal("invalid JSON accepted")
			}
		})
	}
}

func TestAttentionPreviewSafety(t *testing.T) {
	tests := []struct {
		format, content string
		valid           bool
	}{
		{"text", "<script>literal text</script>", true},
		{"markdown", "Contact user@example.com", true},
		{"markdown", "[unused]: javascript:alert(1)", false},
		{"markdown", "### Scope\n- Small\n\n[Reference](https://example.com)", true},
		{"markdown", "\x60\x60\x60html\n<script>alert(1)</script>\n\x60\x60\x60", true},
		{"markdown", "> Quote\n\n| A | B |\n|---|---|\n| x | y |", true},
		{"markdown", "<div>raw HTML</div>", false},
		{"markdown", "<iframe src=\"https://example.com\"></iframe>", false},
		{"markdown", "![remote](https://example.com/a.png)", false},
		{"markdown", "![local](file:///tmp/private.png)", false},
		{"markdown", "[run](javascript:alert%281%29)", false},
		{"markdown", "[run](data:text/html,hello)", false},
		{"markdown", "[read](file:///etc/passwd)", false},
		{"markdown", "<Widget />", false},
		{"html", "<p>not supported</p>", false},
		{"text", " ", false},
	}
	for i, tc := range tests {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			p := testAttentionPayload()
			p.Questions[0].Options[0].Preview = &AttentionPreview{Format: tc.format, Content: tc.content}
			_, err := ParseAttentionQuestionPayload(attentionJSON(t, p), AttentionQuestionKindQuestion)
			if (err == nil) != tc.valid {
				t.Fatalf("preview %q: valid=%v, error=%v", tc.content, tc.valid, err)
			}
		})
	}
}

func TestAttentionAuthorizationContext(t *testing.T) {
	p := testAttentionPayload()
	p.Authorization = &AttentionAuthorization{Action: "Apply change", Scope: "This worktree", Limits: "No publish", Evidence: []string{"Reviewed diff"}}
	if _, err := ParseAttentionQuestionPayload(attentionJSON(t, p), AttentionQuestionKindAuthorization); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseAttentionQuestionPayload(attentionJSON(t, p), AttentionQuestionKindQuestion); err == nil {
		t.Fatal("question accepted authorization context")
	}
	p.Authorization = nil
	if _, err := ParseAttentionQuestionPayload(attentionJSON(t, p), AttentionQuestionKindAuthorization); err == nil {
		t.Fatal("authorization accepted missing context")
	}
}

func TestAttentionAuthorizationBoundsAndClientKey(t *testing.T) {
	valid := func() AttentionQuestionPayload {
		p := testAttentionPayload()
		p.Authorization = &AttentionAuthorization{Action: "Apply change", Scope: "This worktree", Limits: "No publish", Evidence: []string{"Reviewed diff"}}
		return p
	}
	for _, tc := range []struct {
		name string
		max  int
		set  func(*AttentionAuthorization, string)
	}{
		{"action", 1000, func(a *AttentionAuthorization, s string) { a.Action = s }},
		{"scope", 2000, func(a *AttentionAuthorization, s string) { a.Scope = s }},
		{"limits", 2000, func(a *AttentionAuthorization, s string) { a.Limits = s }},
		{"evidence text", 1000, func(a *AttentionAuthorization, s string) { a.Evidence[0] = s }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, size := range []int{0, 1, tc.max, tc.max + 1} {
				p := valid()
				tc.set(p.Authorization, strings.Repeat("界", size))
				_, err := ParseAttentionQuestionPayload(attentionJSON(t, p), AttentionQuestionKindAuthorization)
				if (err == nil) != (size >= 1 && size <= tc.max) {
					t.Fatalf("size %d: %v", size, err)
				}
			}
		})
	}
	for _, size := range []int{0, 1, 4, 5} {
		p := valid()
		p.Authorization.Evidence = make([]string, size)
		for i := range p.Authorization.Evidence {
			p.Authorization.Evidence[i] = "Source evidence"
		}
		_, err := ParseAttentionQuestionPayload(attentionJSON(t, p), AttentionQuestionKindAuthorization)
		if (err == nil) != (size >= 1 && size <= 4) {
			t.Fatalf("%d evidence entries: %v", size, err)
		}
	}
	for _, key := range []string{"", " has-space", "__other__", "非ASCII", strings.Repeat("k", 65)} {
		if err := ValidateAttentionClientKey(key); err == nil {
			t.Fatalf("invalid client key %q", key)
		}
	}
	if err := ValidateAttentionClientKey(strings.Repeat("K", 64)); err != nil {
		t.Fatal(err)
	}
}

func TestAttentionCanonicalAnswerOrder(t *testing.T) {
	p := testAttentionPayload()
	p.Questions[0].MultiSelect = true
	a := []AttentionQuestionAnswer{{QuestionKey: "Q1", SelectedOptionIDs: []string{"second", "first"}}}
	got, err := CanonicalAttentionAnswers(p, a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got[0].SelectedOptionIDs, ",") != "first,second" {
		t.Fatalf("canonical order: %v", got)
	}
	if strings.Join(a[0].SelectedOptionIDs, ",") != "second,first" {
		t.Fatal("mutated caller's answer")
	}
}

func TestAttentionGroupedAnswers(t *testing.T) {
	p := testAttentionPayload()
	second := testAttentionPayload().Questions[0]
	second.Key, second.MultiSelect = "Q2", true
	p.Questions = append(p.Questions, second)
	valid := []AttentionQuestionAnswer{
		{QuestionKey: "Q1", SelectedOptionIDs: []string{"first"}},
		{QuestionKey: "Q2", SelectedOptionIDs: []string{"first", "second"}, OtherText: attentionText("Also check the narrow layout.")},
	}
	if err := ValidateAttentionAnswers(p, valid); err != nil {
		t.Fatal(err)
	}
	tests := map[string][]AttentionQuestionAnswer{
		"missing question":         valid[:1],
		"duplicate question":       {valid[0], valid[0]},
		"unknown question":         {valid[0], {QuestionKey: "Q3", SelectedOptionIDs: []string{"first"}}},
		"unknown option":           {{QuestionKey: "Q1", SelectedOptionIDs: []string{"unknown"}}, valid[1]},
		"duplicate option":         {valid[0], {QuestionKey: "Q2", SelectedOptionIDs: []string{"first", "first"}}},
		"single multiple options":  {{QuestionKey: "Q1", SelectedOptionIDs: []string{"first", "second"}}, valid[1]},
		"single option plus Other": {{QuestionKey: "Q1", SelectedOptionIDs: []string{"first"}, OtherText: attentionText("Other")}, valid[1]},
		"missing answer":           {{QuestionKey: "Q1"}, valid[1]},
		"empty Other":              {{QuestionKey: "Q1", OtherText: attentionText(" \n")}, valid[1]},
		"long Other":               {{QuestionKey: "Q1", OtherText: attentionText(strings.Repeat("界", 4001))}, valid[1]},
		"empty multi":              {valid[0], {QuestionKey: "Q2"}},
	}
	for name, answers := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateAttentionAnswers(p, answers); err == nil {
				t.Fatal("invalid grouped answer accepted")
			}
		})
	}
	valid[0] = AttentionQuestionAnswer{QuestionKey: "Q1", OtherText: attentionText(strings.Repeat("🚀", 4000))}
	valid[1] = AttentionQuestionAnswer{QuestionKey: "Q2", OtherText: attentionText("Only other")}
	if err := ValidateAttentionAnswers(p, valid); err != nil {
		t.Fatal(err)
	}
}

func TestAttentionAnswerIdentityAndBytes(t *testing.T) {
	input := AttentionAnswerSubmission{QuestionID: "question-id", ExpectedVersion: 1, ClientAnswerID: "client-answer-id", Answers: []AttentionQuestionAnswer{{QuestionKey: "Q1", SelectedOptionIDs: []string{"first"}}}}
	raw := attentionJSON(t, input)
	padded := append(bytes.Clone(raw), bytes.Repeat([]byte(" "), 32*1024-len(raw))...)
	if _, err := ParseAttentionAnswer(padded); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{
		"over 32 KiB":       append(padded, ' '),
		"unknown field":     bytes.Replace(raw, []byte(`"expectedVersion":1`), []byte(`"expectedVersion":1,"actor":"agent-says-user"`), 1),
		"zero version":      bytes.Replace(raw, []byte(`"expectedVersion":1`), []byte(`"expectedVersion":0`), 1),
		"unsafe version":    bytes.Replace(raw, []byte(`"expectedVersion":1`), []byte(`"expectedVersion":9007199254740992`), 1),
		"empty client id":   bytes.Replace(raw, []byte("client-answer-id"), nil, 1),
		"invalid client id": bytes.Replace(raw, []byte("client-answer-id"), []byte("bad id"), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseAttentionAnswer(raw); err == nil {
				t.Fatal("invalid answer identity accepted")
			}
		})
	}
	input.ExpectedVersion = 9007199254740991
	input.ClientAnswerID = strings.Repeat("a", 128)
	if _, err := ParseAttentionAnswer(attentionJSON(t, input)); err != nil {
		t.Fatal(err)
	}
}
