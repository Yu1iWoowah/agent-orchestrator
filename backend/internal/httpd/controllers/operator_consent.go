package controllers

import (
	"net/http"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// OperatorConsentReadiness is read-only. Prepare and consume are domain ports;
// there is deliberately no HTTP confirmation or mint endpoint.
func OperatorConsentReadiness(w http.ResponseWriter, _ *http.Request) {
	envelope.WriteJSON(w, http.StatusOK, OperatorConsentReadinessResponse(domain.GetAuthorityReadiness(nil, domain.OperatorActionScope{})))
}
