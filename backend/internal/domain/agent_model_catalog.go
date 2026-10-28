package domain

import "time"

// AgentModelCatalogNeedsRevalidation uses the machine's calendar date rather
// than an elapsed duration. Successful evidence remains fresh through the
// current local day; missing or future success times are untrusted.
func AgentModelCatalogNeedsRevalidation(lastSuccess, now time.Time) bool {
	if lastSuccess.IsZero() || lastSuccess.After(now) {
		return true
	}
	y, m, d := now.Date()
	ly, lm, ld := lastSuccess.In(now.Location()).Date()
	return y != ly || m != lm || d != ld
}
