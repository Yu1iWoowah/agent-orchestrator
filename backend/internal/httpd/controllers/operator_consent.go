package controllers

import (
	"net/http"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// OperatorConsentAuthority reads native qualification without minting.
type OperatorConsentAuthority interface {
	GetAuthorityReadiness([]domain.OperatorPurpose, domain.OperatorActionScope) domain.OperatorAuthorityReadiness
}

// OperatorConsentReadinessQuery describes read-only purpose and scope qualifiers.
type OperatorConsentReadinessQuery struct {
	Purpose    []string `query:"purpose,omitempty" description:"Required closed-registry purposes; repeat for multiple purposes."`
	ProjectRef string   `query:"projectRef,omitempty"`
	OwnerRef   string   `query:"ownerRef,omitempty"`
	BranchRef  string   `query:"branchRef,omitempty"`
	NativeRef  string   `query:"nativeRef,omitempty"`
}

// OperatorConsentReadinessHandler accepts only read-only qualification inputs.
func OperatorConsentReadinessHandler(authority OperatorConsentAuthority) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if authority == nil {
			OperatorConsentReadiness(w, r)
			return
		}
		query := r.URL.Query()
		var purposes []domain.OperatorPurpose
		for _, value := range query["purpose"] {
			purposes = append(purposes, domain.OperatorPurpose(value))
		}
		scope := domain.OperatorActionScope{ProjectRef: query.Get("projectRef"), OwnerRef: query.Get("ownerRef"), BranchRef: query.Get("branchRef"), NativeRef: query.Get("nativeRef")}
		envelope.WriteJSON(w, http.StatusOK, OperatorConsentReadinessResponse(authority.GetAuthorityReadiness(purposes, scope)))
	}
}

// OperatorConsentReadiness is read-only. Prepare and consume are domain ports;
// there is deliberately no HTTP confirmation or mint endpoint.
func OperatorConsentReadiness(w http.ResponseWriter, _ *http.Request) {
	envelope.WriteJSON(w, http.StatusOK, OperatorConsentReadinessResponse(domain.GetAuthorityReadiness(nil, domain.OperatorActionScope{})))
}
