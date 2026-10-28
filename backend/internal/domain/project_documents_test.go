package domain

import "testing"

func TestHandoffConfigKeepsLivingFilesSeparateFromEvidence(t *testing.T) {
	for _, root := range []string{"../outside", "/absolute", "handoffs/current", "adoptions", "HANDOFFS", "handoffs.", "adoptions /child", "x/../shared", "rules\\escape"} {
		if err := (OrchestratorHandoffConfig{SharedDir: root}).Validate(); err == nil {
			t.Errorf("accepted %q", root)
		}
	}
	for _, root := range []string{"", "shared", "living/docs"} {
		if err := (OrchestratorHandoffConfig{SharedDir: root, DocumentPath: "handoff/current.md"}).Validate(); err != nil {
			t.Errorf("%q: %v", root, err)
		}
	}
}
