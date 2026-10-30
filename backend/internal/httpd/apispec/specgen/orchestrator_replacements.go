package specgen

import (
	"net/http"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

func orchestratorReplacementOperations() []operation {
	responses := func(status int) []respUnit {
		return []respUnit{
			{status, controllers.OrchestratorReplacementResponse{}},
			{http.StatusBadRequest, envelope.APIError{}},
			{http.StatusForbidden, envelope.APIError{}},
			{http.StatusNotFound, envelope.APIError{}},
			{http.StatusConflict, envelope.APIError{}},
			{http.StatusInternalServerError, envelope.APIError{}},
			{http.StatusNotImplemented, envelope.APIError{}},
		}
	}
	ops := make([]operation, 0, 6)
	ops = append(ops, []operation{
		{method: http.MethodGet, path: "/api/v1/orchestrator/replacements/{operationId}/diagnose", id: "diagnoseOrchestratorReplacement", tag: "projects", summary: "Read operator CAS metadata without raw state", pathParams: []any{controllers.HandoffEvidenceOperationParam{}}, resps: append([]respUnit{{http.StatusOK, controllers.OrchestratorReplacementDiagnosticResponse{}}}, responses(http.StatusOK)[1:]...)},
		{method: http.MethodPost, path: "/api/v1/orchestrator/replacements/{operationId}/repair", id: "repairOrchestratorReplacement", tag: "projects", summary: "Authorize data-only repair from a sealed snapshot with explicit CAS", pathParams: []any{controllers.HandoffEvidenceOperationParam{}}, reqBody: controllers.OrchestratorReplacementRepairRequest{}, resps: responses(http.StatusAccepted)},
		{method: http.MethodPost, path: "/api/v1/projects/{id}/orchestrator/replacements", id: "replaceOrchestrator", tag: "projects", summary: "Accept a durable Claude Code orchestrator handoff", pathParams: []any{controllers.ProjectIDParam{}}, reqBody: controllers.OrchestratorReplaceRequest{}, resps: responses(http.StatusAccepted)},
		{method: http.MethodGet, path: "/api/v1/orchestrator/replacements/{operationId}", id: "getOrchestratorReplacement", tag: "projects", summary: "Read safe durable replacement progress and evidence", pathParams: []any{controllers.HandoffEvidenceOperationParam{}}, resps: responses(http.StatusOK)},
		{method: http.MethodGet, path: "/api/v1/orchestrator/replacements/{operationId}/intake", id: "getOrchestratorReplacementIntake", tag: "projects", summary: "Read canonical queued intake and pending reports without claiming delivery", pathParams: []any{controllers.HandoffEvidenceOperationParam{}}, resps: append([]respUnit{{http.StatusOK, controllers.OrchestratorReplacementIntakeResponse{}}}, responses(http.StatusOK)[1:]...)},
	}...)
	for _, action := range []struct {
		path, id, summary string
		body              any
	}{
		{"retry", "retryOrchestratorReplacement", "Reconcile the same operation and retained successor", controllers.OrchestratorReplacementActionRequest{}},
		{"cancel", "cancelOrchestratorReplacement", "Cancel a safe pre-stop operation without interrupting agents", controllers.OrchestratorReplacementActionRequest{}},
		{"prepared", "submitOrchestratorReplacementPreparation", "Submit the owning source's managed handoff receipt", controllers.OrchestratorReplacementPreparedRequest{}},
	} {
		ops = append(ops, operation{method: http.MethodPost, path: "/api/v1/orchestrator/replacements/{operationId}/" + action.path, id: action.id, tag: "projects", summary: action.summary, pathParams: []any{controllers.HandoffEvidenceOperationParam{}}, reqBody: action.body, resps: responses(http.StatusAccepted)})
	}
	return ops
}
