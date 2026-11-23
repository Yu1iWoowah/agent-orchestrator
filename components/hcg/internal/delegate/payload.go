package delegate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// systemPrompt is AO's own fixed instruction; the request carries the schema.
const systemPrompt = "You are a read-only delegate for AO. Answer only from the numbered sources in the request and reply with the single JSON object it describes."

// PayloadAudit records only hashes and verified ranges, never source bodies.
type PayloadAudit struct {
	ScopeDigest     string        `json:"scopeDigest"`
	InstructionHash string        `json:"instructionHash"`
	FocusHash       string        `json:"focusHash"`
	QuestionHash    string        `json:"questionHash"`
	SystemHash      string        `json:"systemHash"`
	PayloadHash     string        `json:"payloadHash"`
	ManifestDigest  string        `json:"manifestDigest"`
	Coverage        string        `json:"coverage"`
	Sources         []SourceRange `json:"sources"`
	Omissions       []Omission    `json:"omissions"`
}

func numbered(text string) string {
	lines := strings.Split(text, "\n")
	width := max(4, len(fmt.Sprint(len(lines))))
	for i, line := range lines {
		lines[i] = fmt.Sprintf("%*d│ %s", width, i+1, line)
	}
	return strings.Join(lines, "\n")
}

// userPrompt appends every numbered source to the fixed question prompt.
func userPrompt(question string, scope preparedScope) string {
	var b strings.Builder
	b.WriteString(question)
	b.WriteString("\n\n=== SOURCES ===\n")
	for _, source := range scope.Sources {
		b.WriteString("\n--- BEGIN SOURCE " + source.ID + " ---\n" + numbered(source.text) + "\n--- END SOURCE " + source.ID + " ---\n")
	}
	return b.String()
}

func buildPayload(profile Profile, question string, scope preparedScope) ([]byte, PayloadAudit, error) {
	prompt := userPrompt(question, scope)
	audit := PayloadAudit{QuestionHash: textHash(question), SystemHash: textHash(systemPrompt), ManifestDigest: scope.ManifestDigest, Coverage: "none", Sources: []SourceRange{}, Omissions: []Omission{}}
	if !safeText(prompt) {
		return nil, audit, scopeFailure("blocked", "", "payload_sensitive")
	}
	body := map[string]any{"model": profile.Model, "messages": []map[string]string{{"role": "system", "content": systemPrompt}, {"role": "user", "content": prompt}}, "max_completion_tokens": profile.OutputTokens}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, audit, err
	}
	// Use a conservative byte envelope for context admission; budget.go
	// reserves the reviewed full model input ceiling, never this byte estimate.
	if int64(len(payload)) > profile.ContextTokens-profile.OutputTokens {
		return nil, audit, scopeFailure("scope_too_large", "", "payload_context_limit")
	}
	audit.PayloadHash = textHash(string(payload))
	audit.Coverage = "complete"
	for _, source := range scope.Sources {
		audit.Sources = append(audit.Sources, source.SourceRange)
	}
	scopeBytes, _ := json.Marshal(audit.Sources)
	audit.ScopeDigest = textHash(string(scopeBytes))
	return payload, audit, nil
}
