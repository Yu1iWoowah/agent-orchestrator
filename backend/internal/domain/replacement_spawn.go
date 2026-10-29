package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ReplacementSpawnRequestHash binds the one successor launch intent to its
// pinned configuration and published handoff. Mutable saga progress is excluded
// so a recovered caller uses the same request identity after the seed is bound.
func ReplacementSpawnRequestHash(op OrchestratorReplacement) (string, error) {
	p, t := op.Preparation, op.Transcript
	if op.ID == "" || op.ProjectID == "" || op.SourceSessionID == "" || op.SourceRetiredAt == nil || op.TargetAccountID == "" || op.SharedDir == "" || op.DocumentPath == "" || p == nil || p.SubmittedHash == "" || t == nil || t.AttemptID == "" || t.TranscriptHash == "" || t.Receipt == nil || t.Receipt.Directory == "" || t.Receipt.ManifestHash == "" || t.Receipt.InputHash == "" {
		return "", ErrReplacementStateInvalid
	}
	b, err := json.Marshal(struct {
		Version            int
		Operation          string
		Project            ProjectID
		Source             SessionID
		SourceOwner        SessionControllerOwner
		ProjectFingerprint string
		Harness            AgentHarness
		Mode               SessionMode
		Config             AgentConfig
		Launch             AgentLaunchOptions
		Account            string
		SharedDirectory    string
		DocumentPath       string
		HandoffHash        string
		Attempt            string
		TranscriptHash     string
		Receipt            HandoffEvidenceReceipt
	}{Version: 1, Operation: op.ID, Project: op.ProjectID, Source: op.SourceSessionID, SourceOwner: op.SourceOwner, ProjectFingerprint: op.ProjectFingerprint, Harness: HarnessClaudeCode, Mode: SessionModeChat, Config: op.TargetAgentConfig, Launch: op.TargetAgentConfig.Launch, Account: op.TargetAccountID, SharedDirectory: op.SharedDir, DocumentPath: op.DocumentPath, HandoffHash: p.SubmittedHash, Attempt: t.AttemptID, TranscriptHash: t.TranscriptHash, Receipt: *t.Receipt})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
