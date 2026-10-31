package transcriptexport

import "github.com/aoagents/agent-orchestrator/backend/internal/domain"

type nativeTurnOwner struct {
	nativeID   string
	provenance domain.NativeThreadProvenance
}

// VerifyNativeTurnOwnership checks settled AO history from durable DB facts.
// It does not read native files or control providers; native coverage still
// requires Build after capture. Batched members use their dispatch's ownership.
func VerifyNativeTurnOwnership(s domain.TranscriptSourceSnapshot) error {
	epochs, err := inventory(s)
	if err != nil {
		return err
	}
	_, members, err := batchInputs(s)
	if err != nil {
		return err
	}
	_, err = verifyTurnOwnership(s, epochs, members)
	return err
}

func verifyTurnOwnership(s domain.TranscriptSourceSnapshot, epochs, members map[string]string) (map[string]nativeTurnOwner, error) {
	branches := map[string]string{}
	for _, b := range s.Branches {
		branches[b.ID] = b.ProviderConversationID
	}
	index := domain.IndexNativeThreadEvidence(s.NativeThreads)
	owners := map[string]nativeTurnOwner{}
	for _, turn := range s.Turns {
		if members[turn.ID] != "" || !turn.State.Terminal() || turn.ProviderTurnID == "" {
			continue
		}
		root := branches[turn.BranchID]
		provenance := index.Classify(turn.BranchID, turn.ProviderTurnID, root)
		owner := index[domain.NativeThreadTurnKey{BranchID: turn.BranchID, ProviderTurnID: turn.ProviderTurnID}]
		if provenance == domain.NativeThreadChild {
			if _, ownedEpoch := epochs[owner]; ownedEpoch {
				root, provenance = owner, domain.NativeThreadMain
			} else if !confirmedChild(s, root, owner) {
				provenance = domain.NativeThreadUnknown
			}
		}
		if provenance == domain.NativeThreadUnknown {
			return nil, incomplete("TRANSCRIPT_PROVENANCE_UNKNOWN", "AO turn lacks unambiguous native-thread ownership")
		}
		owners[turn.ID] = nativeTurnOwner{nativeID: root, provenance: provenance}
	}
	return owners, nil
}
