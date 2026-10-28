package domain

import "testing"

func TestNativeThreadEvidenceIndexIsBranchScopedAndConflictIsSticky(t *testing.T) {
	evidence := []ConversationNativeThreadEvidence{
		{BranchID: "a", ProviderTurnID: "same", ProviderConversationID: "main-a"},
		{BranchID: "b", ProviderTurnID: "same", ProviderConversationID: "child-b"},
		{BranchID: "c", ProviderTurnID: "same", ProviderConversationID: "main-c"},
		{BranchID: "c", ProviderTurnID: "same", ProviderConversationID: "child-c"},
		{BranchID: "c", ProviderTurnID: "same", ProviderConversationID: "main-c"},
	}
	index := IndexNativeThreadEvidence(evidence)
	for _, tc := range []struct {
		branch, root string
		want         NativeThreadProvenance
	}{
		{"a", "main-a", NativeThreadMain}, {"b", "main-b", NativeThreadChild},
		{"c", "main-c", NativeThreadUnknown}, {"missing", "main-a", NativeThreadUnknown},
	} {
		if got := index.Classify(tc.branch, "same", tc.root); got != tc.want {
			t.Errorf("branch %s: got %s want %s", tc.branch, got, tc.want)
		}
	}
	if got := index.Classify("a", "same", ""); got != NativeThreadUnknown {
		t.Fatal("missing root became known")
	}
}
