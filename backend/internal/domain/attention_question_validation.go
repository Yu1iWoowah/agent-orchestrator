package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

var attentionTokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

func attentionInvalid(field, message string) error {
	return &AttentionValidationError{Field: field, Message: message}
}

func attentionToken(field, value string, limit int) error {
	if len(value) < 1 || len(value) > limit || !attentionTokenPattern.MatchString(value) {
		return attentionInvalid(field, fmt.Sprintf("must contain 1–%d ASCII letters, digits, underscores or hyphens, starting with a letter or digit", limit))
	}
	return nil
}

// ValidateAttentionClientKey checks the session-scoped idempotency key.
func ValidateAttentionClientKey(key string) error { return attentionToken("clientKey", key, 64) }

func attentionTextBound(field, value string, limit int) error {
	if strings.TrimSpace(value) == "" || utf8.RuneCountInString(value) > limit {
		return attentionInvalid(field, fmt.Sprintf("must contain 1–%d Unicode code points", limit))
	}
	return nil
}

// Preserve missing boolean defaults, but refuse unknown members, duplicates and
// explicit nulls rather than silently changing the meaning of a decision.
func decodeAttentionJSON(raw []byte, limit int, field string, target any) error {
	if len(raw) > limit {
		return attentionInvalid(field, fmt.Sprintf("JSON exceeds %d bytes", limit))
	}
	if !utf8.Valid(raw) {
		return attentionInvalid(field, "JSON must be UTF-8")
	}
	tokens := json.NewDecoder(bytes.NewReader(raw))
	tokens.UseNumber()
	if err := scanAttentionJSON(tokens, field, "", 0); err != nil {
		return err
	}
	if _, err := tokens.Token(); err != io.EOF {
		return attentionInvalid(field, "expected one JSON value")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return attentionInvalid(field, err.Error())
	}
	return nil
}

// DecodeAttentionRequest preserves strict JSON semantics for the small command
// envelopes around question payloads; payload validation remains authoritative.
func DecodeAttentionRequest(raw []byte, limit int, target any) error {
	return decodeAttentionJSON(raw, limit, "request", target)
}

func scanAttentionJSON(decoder *json.Decoder, path, member string, depth int) error {
	if depth > 32 {
		return attentionInvalid(path, "JSON is too deeply nested")
	}
	token, err := decoder.Token()
	if err != nil {
		return attentionInvalid(path, err.Error())
	}
	if token == nil {
		return attentionInvalid(path, "null is not a field value; omit optional fields")
	}
	if member == "multiSelect" || member == "recommended" {
		if _, ok := token.(bool); !ok {
			return attentionInvalid(path, "must be a boolean")
		}
	}
	switch token {
	case json.Delim('{'):
		keys := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return attentionInvalid(path, err.Error())
			}
			name, ok := key.(string)
			if !ok || keys[name] {
				return attentionInvalid(path, "duplicate or invalid JSON member")
			}
			keys[name] = true
			if err := scanAttentionJSON(decoder, path+"."+name, name, depth+1); err != nil {
				return err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return attentionInvalid(path, err.Error())
		}
	case json.Delim('['):
		for i := 0; decoder.More(); i++ {
			if err := scanAttentionJSON(decoder, fmt.Sprintf("%s[%d]", path, i), "", depth+1); err != nil {
				return err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return attentionInvalid(path, err.Error())
		}
	}
	return nil
}

// ParseAttentionQuestionPayload strictly validates the versioned create or revise payload.
func ParseAttentionQuestionPayload(raw []byte, kind AttentionQuestionKind) (AttentionQuestionPayload, error) {
	var payload AttentionQuestionPayload
	if err := decodeAttentionJSON(raw, MaxAttentionCreateBytes, "payload", &payload); err != nil {
		return payload, err
	}
	if payload.SchemaVersion != 1 {
		return payload, attentionInvalid("schemaVersion", "must be 1")
	}
	for _, value := range []struct {
		field, text string
		limit       int
	}{
		{"title", payload.Title, 120}, {"explanation", payload.Explanation, 8000},
	} {
		if err := attentionTextBound(value.field, value.text, value.limit); err != nil {
			return payload, err
		}
	}
	if len(payload.Questions) < 1 || len(payload.Questions) > 4 {
		return payload, attentionInvalid("questions", "must contain 1–4 questions")
	}
	keys := map[string]bool{}
	for i, question := range payload.Questions {
		path := fmt.Sprintf("questions[%d]", i)
		if err := attentionToken(path+".key", question.Key, 64); err != nil {
			return payload, err
		}
		if keys[question.Key] {
			return payload, attentionInvalid(path+".key", "duplicate question key")
		}
		keys[question.Key] = true
		for _, value := range []struct {
			field, text string
			limit       int
		}{
			{"header", question.Header, 24}, {"question", question.Question, 2000},
		} {
			if err := attentionTextBound(path+"."+value.field, value.text, value.limit); err != nil {
				return payload, err
			}
		}
		if question.Explanation != nil {
			if err := attentionTextBound(path+".explanation", *question.Explanation, 4000); err != nil {
				return payload, err
			}
		}
		if len(question.Options) < 2 || len(question.Options) > 4 {
			return payload, attentionInvalid(path+".options", "must contain 2–4 options")
		}
		ids, labels := map[string]bool{}, map[string]bool{}
		for j, option := range question.Options {
			optionPath := fmt.Sprintf("%s.options[%d]", path, j)
			if err := attentionToken(optionPath+".id", option.ID, 64); err != nil {
				return payload, err
			}
			if ids[option.ID] {
				return payload, attentionInvalid(optionPath+".id", "duplicate option id")
			}
			ids[option.ID] = true
			label := strings.TrimSpace(option.Label)
			if labels[label] || strings.EqualFold(label, "other") || label == "其他" {
				return payload, attentionInvalid(optionPath+".label", "duplicate or reserved option label")
			}
			labels[label] = true
			if err := attentionTextBound(optionPath+".label", option.Label, 80); err != nil {
				return payload, err
			}
			if err := attentionTextBound(optionPath+".description", option.Description, 1000); err != nil {
				return payload, err
			}
			if option.Recommended && j != 0 {
				return payload, attentionInvalid(optionPath+".recommended", "only the first option may be recommended")
			}
			if option.Preview != nil {
				if err := validateAttentionPreview(option.Preview); err != nil {
					return payload, attentionInvalid(optionPath+".preview", err.Error())
				}
			}
		}
	}
	switch kind {
	case AttentionQuestionKindQuestion:
		if payload.Authorization != nil {
			return payload, attentionInvalid("authorization", "not allowed for a question")
		}
	case AttentionQuestionKindAuthorization:
		if payload.Authorization == nil {
			return payload, attentionInvalid("authorization", "required for authorization")
		}
		a := payload.Authorization
		for _, value := range []struct {
			field, text string
			limit       int
		}{
			{"action", a.Action, 1000}, {"scope", a.Scope, 2000}, {"limits", a.Limits, 2000},
		} {
			if err := attentionTextBound("authorization."+value.field, value.text, value.limit); err != nil {
				return payload, err
			}
		}
		if len(a.Evidence) < 1 || len(a.Evidence) > 4 {
			return payload, attentionInvalid("authorization.evidence", "must contain 1–4 text items")
		}
		for i, value := range a.Evidence {
			if err := attentionTextBound(fmt.Sprintf("authorization.evidence[%d]", i), value, 1000); err != nil {
				return payload, err
			}
		}
	default:
		return payload, attentionInvalid("kind", "must be question or authorization")
	}
	return payload, nil
}

func validateAttentionPreview(preview *AttentionPreview) error {
	if err := attentionTextBound("content", preview.Content, 8192); err != nil {
		return err
	}
	if preview.Format == "text" {
		return nil
	}
	if preview.Format != "markdown" {
		return attentionInvalid("format", "must be text or markdown")
	}
	source := []byte(preview.Content)
	context := parser.NewContext()
	document := goldmark.New(goldmark.WithExtensions(extension.Table)).Parser().Parse(text.NewReader(source), parser.WithContext(context))
	for _, reference := range context.References() {
		if err := validateAttentionPreviewURL(reference.Destination()); err != nil {
			return err
		}
	}
	return ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch node.Kind() {
		case ast.KindHTMLBlock, ast.KindRawHTML, ast.KindImage:
			return ast.WalkStop, attentionInvalid("content", "HTML and images are not allowed")
		}
		var destination []byte
		switch link := node.(type) {
		case *ast.Link:
			destination = link.Destination
		case *ast.AutoLink:
			destination = link.URL(source)
		}
		if destination != nil {
			if err := validateAttentionPreviewURL(destination); err != nil {
				return ast.WalkStop, err
			}
		}
		return ast.WalkContinue, nil
	})
}

func validateAttentionPreviewURL(destination []byte) error {
	decoded := strings.Map(func(r rune) rune {
		if r <= 32 || r == 127 {
			return -1
		}
		return r
	}, html.UnescapeString(string(destination)))
	value, err := url.Parse(decoded)
	if err != nil || (value.Scheme != "" && !strings.EqualFold(value.Scheme, "https") && !strings.EqualFold(value.Scheme, "http")) {
		return attentionInvalid("content", "unsafe URL scheme")
	}
	return nil
}

// ParseAttentionAnswer checks the answer envelope before source-dependent validation.
func ParseAttentionAnswer(raw []byte) (AttentionAnswerSubmission, error) {
	var input AttentionAnswerSubmission
	if err := decodeAttentionJSON(raw, MaxAttentionAnswerBytes, "answer", &input); err != nil {
		return input, err
	}
	if err := attentionToken("questionId", input.QuestionID, 128); err != nil {
		return input, err
	}
	if err := attentionToken("clientAnswerId", input.ClientAnswerID, 128); err != nil {
		return input, err
	}
	if input.ExpectedVersion < 1 || input.ExpectedVersion > MaxAttentionQuestionVersion {
		return input, attentionInvalid("expectedVersion", "must be a positive JavaScript-safe integer")
	}
	if len(input.Answers) < 1 || len(input.Answers) > 4 {
		return input, attentionInvalid("answers", "must contain 1–4 answers")
	}
	for i := range input.Answers {
		if input.Answers[i].SelectedOptionIDs == nil {
			input.Answers[i].SelectedOptionIDs = []string{}
		}
	}
	return input, nil
}

// ValidateAttentionAnswers checks that every prompt has exactly one valid answer record.
func ValidateAttentionAnswers(payload AttentionQuestionPayload, answers []AttentionQuestionAnswer) error {
	_, err := CanonicalAttentionAnswers(payload, answers)
	return err
}

// CanonicalAttentionAnswers orders validated answers by question and option declaration order.
func CanonicalAttentionAnswers(payload AttentionQuestionPayload, answers []AttentionQuestionAnswer) ([]AttentionQuestionAnswer, error) {
	if len(answers) != len(payload.Questions) {
		return nil, attentionInvalid("answers", "must answer every question exactly once")
	}
	byKey := make(map[string]AttentionQuestionAnswer, len(answers))
	for _, answer := range answers {
		if _, exists := byKey[answer.QuestionKey]; exists {
			return nil, attentionInvalid("answers", "duplicate question key")
		}
		byKey[answer.QuestionKey] = answer
	}
	result := make([]AttentionQuestionAnswer, 0, len(answers))
	for _, question := range payload.Questions {
		answer, exists := byKey[question.Key]
		if !exists {
			return nil, attentionInvalid("answers", "missing or unknown question key")
		}
		selected := map[string]bool{}
		for _, id := range answer.SelectedOptionIDs {
			if selected[id] {
				return nil, attentionInvalid("answers."+question.Key, "duplicate option id")
			}
			selected[id] = true
		}
		ordered := make([]string, 0, len(selected))
		for _, option := range question.Options {
			if selected[option.ID] {
				ordered = append(ordered, option.ID)
			}
		}
		if len(ordered) != len(selected) {
			return nil, attentionInvalid("answers."+question.Key, "unknown option id")
		}
		if answer.OtherText != nil {
			if err := attentionTextBound("answers."+question.Key+".otherText", *answer.OtherText, 4000); err != nil {
				return nil, err
			}
		}
		count := len(ordered)
		if answer.OtherText != nil {
			count++
		}
		if count == 0 || (!question.MultiSelect && count != 1) {
			return nil, attentionInvalid("answers."+question.Key, "single choice requires one option XOR Other; multiple choice requires at least one answer")
		}
		answer.SelectedOptionIDs = ordered
		result = append(result, answer)
	}
	return result, nil
}
