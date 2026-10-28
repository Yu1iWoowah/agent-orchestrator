package domain

import (
	"fmt"
	"path"
	"strings"
)

// OrchestratorHandoffConfig locates living files inside a project's AO-owned root.
// Evidence is deliberately not configurable into this mutable directory.
type OrchestratorHandoffConfig struct {
	SharedDir    string `json:"sharedDir,omitempty"`
	DocumentPath string `json:"documentPath,omitempty"`
}

// Validate checks that living files cannot overlap evidence or leave the project root.
func (c OrchestratorHandoffConfig) Validate() error {
	for _, p := range []string{c.SharedDir, c.DocumentPath} {
		if p != "" && (path.Clean(p) != p || p == "." || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") || p == ".." || strings.HasPrefix(p, "../")) {
			return fmt.Errorf("orchestratorHandoff: paths must be confined relative paths")
		}
	}
	first := strings.ToLower(strings.TrimRight(strings.Split(c.SharedDir, "/")[0], ". "))
	if first == "handoffs" || first == "adoptions" {
		return fmt.Errorf("orchestratorHandoff.sharedDir overlaps immutable evidence")
	}
	return nil
}

// ProjectDocumentLease persists controller ownership and a hashed writer token.
type ProjectDocumentLease struct {
	SessionID SessionID              `json:"sessionId"`
	Owner     SessionControllerOwner `json:"owner"`
	Epoch     int64                  `json:"epoch"`
	Frozen    bool                   `json:"frozen"`
	TokenHash string                 `json:"tokenHash"`
}

// ProjectDocumentEntry records one confined file and optional adoption provenance.
type ProjectDocumentEntry struct {
	Path           string `json:"path"`
	SourceIdentity string `json:"sourceIdentity,omitempty"`
	Backup         bool   `json:"backup,omitempty"`
	Hash           string `json:"hash"`
	Size           int64  `json:"size"`
}

// ProjectDocumentAdoption pins the one-time source snapshot and its completion receipt.
type ProjectDocumentAdoption struct {
	SourceRootIdentity string                 `json:"sourceRootIdentity"`
	SourceSessionID    SessionID              `json:"sourceSessionId"`
	Files              []ProjectDocumentEntry `json:"files"`
	ManifestHash       string                 `json:"manifestHash"`
	Complete           bool                   `json:"complete"`
}

// ProjectDocumentState contains internal control facts, never display status.
type ProjectDocumentState struct {
	Evidence  map[string]HandoffEvidenceReceipt `json:"handoffEvidence,omitempty"`
	Revision  int64                             `json:"-"`
	SharedDir string                            `json:"sharedDir"`
	Lease     *ProjectDocumentLease             `json:"lease,omitempty"`
	Adoption  *ProjectDocumentAdoption          `json:"adoption,omitempty"`
}

// HandoffEvidenceReceipt survives source session deletion and is never updated
// by progress or lease transitions. The manifest does not hash itself.
type HandoffEvidenceReceipt struct {
	Directory    string `json:"directory"`
	ManifestHash string `json:"manifestHash"`
	InputHash    string `json:"inputHash"`
}
