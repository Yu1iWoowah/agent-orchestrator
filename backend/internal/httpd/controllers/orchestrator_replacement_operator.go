package controllers

import (
	"context"
	"encoding/hex"
	"net/http"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

type replacementOperatorService interface {
	Diagnose(context.Context, string) (domain.ReplacementDiagnostic, error)
	RepairSnapshot(context.Context, string) (domain.ReplacementRepairSnapshot, error)
	Repair(context.Context, domain.ReplacementRepairRequest) (domain.OrchestratorReplacement, error)
}

func (c *OrchestratorReplacementController) operator(w http.ResponseWriter, r *http.Request) (replacementOperatorService, bool) {
	svc, ok := c.Svc.(replacementOperatorService)
	if !ok {
		envelope.WriteError(w, r, apierr.NotImplemented("ORCHESTRATOR_REPLACEMENT_REPAIR_UNAVAILABLE", "Verified replacement repair is unavailable"))
	}
	return svc, ok
}

func (c *OrchestratorReplacementController) diagnose(w http.ResponseWriter, r *http.Request) {
	id, ok := replacementOperationID(w, r)
	if !ok {
		return
	}
	svc, ok := c.operator(w, r)
	if !ok {
		return
	}
	d, err := svc.Diagnose(r.Context(), id)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	view := OrchestratorReplacementDiagnostic{OperationID: d.OperationID, ProjectID: d.ProjectID, SourceSessionID: d.SourceSessionID, Revision: d.Revision, Active: d.Active, RawDigest: d.RawDigest, Corrupt: d.Corrupt, SnapshotRevision: d.SnapshotRevision, SnapshotDigest: d.SnapshotDigest}
	envelope.WriteJSON(w, http.StatusOK, OrchestratorReplacementDiagnosticResponse{Diagnostic: view})
}

func (c *OrchestratorReplacementController) repair(w http.ResponseWriter, r *http.Request) {
	id, ok := replacementOperationID(w, r)
	if !ok {
		return
	}
	svc, ok := c.operator(w, r)
	if !ok {
		return
	}
	var in OrchestratorReplacementRepairRequest
	if err := decodeReplacementRequest(r, &in); err != nil || in.ExpectedRevision == nil || in.SnapshotRevision == nil || *in.ExpectedRevision < 0 || *in.SnapshotRevision < 0 || !replacementDigest(in.ExpectedDigest) || !replacementDigest(in.SnapshotDigest) {
		replacementBadRequest(w, r)
		return
	}
	request := domain.ReplacementRepairRequest{OperationID: id, ExpectedRevision: *in.ExpectedRevision, ExpectedDigest: in.ExpectedDigest, Snapshot: domain.ReplacementRepairSnapshot{OperationID: id, Revision: *in.SnapshotRevision, Digest: in.SnapshotDigest}}
	snapshot, err := svc.RepairSnapshot(r.Context(), id)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	if snapshot.OperationID != id || snapshot.Revision != *in.SnapshotRevision || snapshot.Digest != in.SnapshotDigest {
		// A successful repair seals a newer snapshot. Repeating its exact CAS
		// after a lost response reads the verified receipt, never repairs again.
		if current, readErr := c.Svc.Get(r.Context(), id); readErr == nil && current.HasRepairReceipt(request) {
			c.write(w, r, current, nil, http.StatusAccepted)
			return
		}
		envelope.WriteError(w, r, apierr.Conflict("ORCHESTRATOR_REPLACEMENT_REPAIR_CONFLICT", "Trusted snapshot changed; diagnose again before authorizing repair", nil))
		return
	}
	request.Snapshot = snapshot
	op, err := svc.Repair(r.Context(), request)
	c.write(w, r, op, err, http.StatusAccepted)
}

func replacementDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
