package domain

// NativeThreadProvenance separates a branch's root from positively identified
// native child work. Missing or conflicting ownership is never inferred.
type NativeThreadProvenance string

// Native thread classifications require positive ownership evidence.
const (
	NativeThreadUnknown NativeThreadProvenance = "unknown"
	NativeThreadMain    NativeThreadProvenance = "main"
	NativeThreadChild   NativeThreadProvenance = "child"
)

// NativeThreadTurnKey scopes opaque provider turns to one branch.
type NativeThreadTurnKey struct{ BranchID, ProviderTurnID string }

// NativeThreadEvidenceIndex keeps unknown/conflicting owners as empty strings.
type NativeThreadEvidenceIndex map[NativeThreadTurnKey]string

// IndexNativeThreadEvidence makes ownership conflicts sticky across duplicates.
func IndexNativeThreadEvidence(evidence []ConversationNativeThreadEvidence) NativeThreadEvidenceIndex {
	index := make(NativeThreadEvidenceIndex)
	for _, item := range evidence {
		if item.BranchID == "" || item.ProviderTurnID == "" {
			continue
		}
		key := NativeThreadTurnKey{item.BranchID, item.ProviderTurnID}
		previous, found := index[key]
		if !found {
			index[key] = item.ProviderConversationID
		} else if previous != item.ProviderConversationID {
			index[key] = ""
		}
	}
	return index
}

// Classify compares an observed owner with the branch root; callers decide scope.
func (index NativeThreadEvidenceIndex) Classify(branchID, turnID, root string) NativeThreadProvenance {
	owner := index[NativeThreadTurnKey{branchID, turnID}]
	if branchID == "" || turnID == "" || root == "" || owner == "" {
		return NativeThreadUnknown
	}
	if owner == root {
		return NativeThreadMain
	}
	return NativeThreadChild
}
