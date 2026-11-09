package delegate

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Hash of the literal CHAT_PROMPT at PALCommit, with code generation disabled
// and an empty LOCALE. A changed template is blocked, never silently adopted.
const palFinalExchangeHash = "cc611db1ac9ac0e235f36bba893c3c99e9fb5aa647baa915509ac9312b2928dc"

const palChatSystemHash = "6821f22fb3362295aef9c090fc93b715471c45bcb000c047663929ef4eae4bb5"
const userPrefix = "=== USER REQUEST ===\n"
const userSuffix = "\n=== END CONTEXT ====\n=== END REQUEST ===\n\nPlease provide a thoughtful, comprehensive response:"

// PayloadAudit records only hashes and verified ranges, never source bodies.
type PayloadAudit struct {
	QuestionHash   string        `json:"questionHash"`
	SystemHash     string        `json:"systemHash"`
	TemplateHash   string        `json:"templateHash"`
	PayloadHash    string        `json:"payloadHash"`
	ManifestDigest string        `json:"manifestDigest"`
	Coverage       string        `json:"coverage"`
	Sources        []SourceRange `json:"sources"`
	Omissions      []Omission    `json:"omissions"`
}

func numbered(text string) string {
	lines := strings.Split(text, "\n")
	width := max(4, len(fmt.Sprint(len(lines))))
	for i, line := range lines {
		lines[i] = fmt.Sprintf("%*d│ %s", width, i+1, line)
	}
	return strings.Join(lines, "\n")
}
func expectedPALPrompt(question string, scope preparedScope) string {
	blocks := make([]string, 0, len(scope.Sources))
	for _, source := range scope.Sources {
		path := "/view/" + source.ID + ".txt"
		blocks = append(blocks, "\n--- BEGIN FILE: "+path+" (Last modified: 1970-01-01 00:00:00 UTC) ---\n"+numbered(source.text)+"\n--- END FILE: "+path+" ---\n")
	}
	return userPrefix + question + "\n\n=== CONTEXT FILES ===\n" + strings.Join(blocks, "\n\n") + userSuffix
}

func gatePayload(profile Profile, question, prompt, system string, scope preparedScope) ([]byte, PayloadAudit, error) {
	audit := PayloadAudit{QuestionHash: textHash(question), SystemHash: textHash(system), TemplateHash: textHash(userPrefix + "{question}\n\n=== CONTEXT FILES ===\n{numbered_sources}" + userSuffix + palFinalExchangeHash), ManifestDigest: scope.ManifestDigest, Coverage: "none", Sources: []SourceRange{}, Omissions: []Omission{}}
	for _, source := range scope.Sources {
		audit.Omissions = append(audit.Omissions, Omission{SourceID: source.ID, Reason: "payload_coverage_unverified"})
	}
	if textHash(system) != palChatSystemHash {
		return nil, audit, scopeFailure("blocked", "", "system_template_changed")
	}
	if !safeText(system) || !safeText(prompt) {
		return nil, audit, scopeFailure("blocked", "", "payload_sensitive")
	}
	base := expectedPALPrompt(question, scope)
	if !strings.HasPrefix(prompt, base) {
		return nil, audit, scopeFailure("blocked", "", "payload_boundary_or_coverage_mismatch")
	}
	if textHash(prompt[len(base):]) != palFinalExchangeHash {
		return nil, audit, scopeFailure("blocked", "", "followup_template_changed")
	}

	payload, err := json.Marshal(map[string]any{"model": profile.Model, "messages": []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": prompt}}, "max_completion_tokens": profile.OutputTokens})
	if err != nil {
		return nil, audit, err
	}
	// Use a conservative byte envelope for PAL context admission; budget.go
	// reserves the reviewed full model input ceiling, never this byte estimate.
	if int64(len(payload)) > profile.ContextTokens-profile.OutputTokens {
		return nil, audit, scopeFailure("scope_too_large", "", "payload_context_limit")
	}
	audit.PayloadHash = textHash(string(payload))
	audit.Coverage = "complete"
	audit.Omissions = []Omission{}
	for _, source := range scope.Sources {
		audit.Sources = append(audit.Sources, source.SourceRange)
	}
	return payload, audit, nil
}
