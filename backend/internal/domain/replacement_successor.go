package domain

// PendingNativeResumeMatches admits observation, never a provider launch or a
// successful resume receipt. Capability rotation is the pre-launch boundary;
// only the reserved generation on the same native identity may be inspected.
func (op OrchestratorReplacement) PendingNativeResumeMatches(rec SessionRecord) bool {
	p := op.ResumeEvidence
	if op.SourceRetiredAt == nil || p == nil || !p.StopConfirmed || p.StopReceipt == nil || p.ResumeIntentAt == nil || p.ResumeGeneration == "" || p.ResumeGeneration == p.BeforeOwner.ControllerGeneration || p.ResumedAt != nil || op.SuccessorController == nil || p.BeforeOwner != op.SuccessorController.Owner || p.BeforeCapabilityVerifier != op.SuccessorController.CapabilityVerifier || rec.ID != op.SuccessorSessionID || rec.ProjectID != op.ProjectID || rec.Kind != KindOrchestrator || rec.IsTerminated || rec.Harness != HarnessClaudeCode || NormalizeSessionMode(rec.Mode) != SessionModeChat || rec.ProvisionState != SessionProvisionReady || rec.ClientRequestID != op.SpawnRequestID || rec.ClientRequestHash != op.SpawnRequestHash || rec.Metadata.ProviderConversationID != p.BeforeNativeID || rec.Metadata.BrowserCapabilityVerifier == "" || rec.Metadata.BrowserCapabilityVerifier == p.BeforeCapabilityVerifier {
		return false
	}
	reserved := p.BeforeOwner
	reserved.ControllerGeneration = p.ResumeGeneration
	return rec.ControllerOwner() == p.BeforeOwner || rec.ControllerOwner() == reserved
}

// ExpectedSuccessorController selects retained ownership facts, never proof that
// the controller is currently live. Callers must separately observe that owner.
func (op OrchestratorReplacement) ExpectedSuccessorController() (ReplacementSuccessorController, bool) {
	var zero ReplacementSuccessorController
	initial := op.SuccessorController
	if op.SourceRetiredAt == nil || op.SuccessorSessionID == "" || initial == nil || initial.CommittedAt.IsZero() || initial.CapabilityVerifier == "" || initial.Owner.IsTerminated || initial.Owner.Harness != HarnessClaudeCode || initial.Owner.Mode != SessionModeChat || initial.Owner.ControllerGeneration == "" || initial.Owner.ProviderConversationID == "" {
		return zero, false
	}
	p := op.ResumeEvidence
	if p == nil || p.ResumedAt == nil {
		switch op.Phase {
		case ReplacementStarting, ReplacementControllerReady, ReplacementHandoffSettled, ReplacementVerifyingResume:
			return *initial, true
		default:
			return zero, false
		}
	}
	switch op.Phase {
	case ReplacementVerifyingResume, ReplacementActivatingSharedWriter, ReplacementNotifying:
	default:
		return zero, false
	}
	stop := p.StopReceipt
	if p.StartedAt.IsZero() || !p.StopConfirmed || stop == nil || stop.ConfirmedAt.IsZero() || stop.Method != "chat-hibernate" || stop.Owner != initial.Owner || stop.NativeID != initial.Owner.ProviderConversationID || stop.CapabilityVerifier != initial.CapabilityVerifier || p.BeforeOwner != initial.Owner || p.BeforeNativeID != initial.Owner.ProviderConversationID || p.BeforeCapabilityVerifier != initial.CapabilityVerifier || p.ResumeIntentAt == nil || p.ResumeIntentAt.IsZero() || p.ResumedAt.IsZero() || !p.HistoryVerified || p.AfterNativeID != p.BeforeNativeID || p.AfterOwner.ProviderConversationID != p.BeforeNativeID || p.AfterOwner.ControllerGeneration == "" || p.AfterOwner.ControllerGeneration == p.BeforeOwner.ControllerGeneration || p.AfterOwner.ControllerGeneration != p.ResumeGeneration || p.AfterOwner.Harness != HarnessClaudeCode || p.AfterOwner.Mode != SessionModeChat || p.AfterOwner.IsTerminated || p.AfterCapabilityVerifier == "" || p.AfterCapabilityVerifier == p.BeforeCapabilityVerifier {
		return zero, false
	}
	return ReplacementSuccessorController{Owner: p.AfterOwner, CapabilityVerifier: p.AfterCapabilityVerifier, CommittedAt: *p.ResumedAt}, true
}
