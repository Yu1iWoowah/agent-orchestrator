package delegate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
)

// Profile is a deployment snapshot, never supplied by the model/tool request.
// Limits describe the reviewed model; OutputTokens is this attempt's hard cap.
type Profile struct {
	Prices          Prices `json:"prices"`
	ID              string `json:"id"`
	Revision        string `json:"revision"`
	BillingProvider string `json:"billingProvider"`
	Model           string `json:"model"`
	ContextTokens   int64  `json:"contextTokens"`
	MaxOutputTokens int64  `json:"maxOutputTokens"`
	OutputTokens    int64  `json:"outputTokens"`
}

var profileToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func (p Profile) endpoint() string {
	switch p.BillingProvider {
	case "openai":
		return "https://api.openai.com/v1/chat/completions"
	case "anthropic":
		return "https://api.anthropic.com/v1/chat/completions"
	default:
		return ""
	}
}

func (p Profile) valid() bool {
	return safeText(p.ID) && safeText(p.Revision) && safeText(p.Model) && p.endpoint() != "" && profileToken.MatchString(p.ID) && profileToken.MatchString(p.Revision) && profileToken.MatchString(p.Model) && p.Model != "auto" && p.Model != "latest" && p.Model != "default" && p.ContextTokens > 0 && p.MaxOutputTokens > 0 && p.OutputTokens > 0 && p.OutputTokens <= p.MaxOutputTokens && p.OutputTokens < p.ContextTokens
}

func (p Profile) digest() string {
	b, _ := json.Marshal(p)
	d := sha256.Sum256(b)
	return hex.EncodeToString(d[:])
}

// Only PAL's fixed assembly metadata crosses into the child, not credentials
// or future host registration/budget state.
func (p Profile) childConfig() map[string]any {
	return map[string]any{"billingProvider": p.BillingProvider, "model": p.Model, "contextTokens": p.ContextTokens, "outputTokens": p.OutputTokens}
}
