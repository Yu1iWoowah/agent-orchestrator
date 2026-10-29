package domain

import (
	"fmt"

	"github.com/google/uuid"
)

// MaxReplacementProbeReplyBytes bounds a control acknowledgment, not an export.
const MaxReplacementProbeReplyBytes = 64 << 10

// MaxReplacementProbeReplyMessages bounds one control turn's reply candidates.
const MaxReplacementProbeReplyMessages = 64

// ReplacementProbeAcknowledgment is the exact reply bound to a new nonce and
// the immutable bootstrap digest. It is not itself native-continuity evidence.
func ReplacementProbeAcknowledgment(op OrchestratorReplacement) (string, bool) {
	if op.ID == "" || op.Bootstrap == nil || op.Bootstrap.PromptHash == "" || op.ResumeEvidence == nil || op.ResumeEvidence.Probe == nil {
		return "", false
	}
	nonce := op.ResumeEvidence.Probe.Nonce
	id, err := uuid.Parse(nonce)
	if err != nil || id == uuid.Nil || id.String() != nonce {
		return "", false
	}
	return fmt.Sprintf("AO_HANDOFF_RESUME_ACK %s %s %s", op.ID, nonce, op.Bootstrap.PromptHash), true
}
