package controllers

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	reviewcore "github.com/aoagents/agent-orchestrator/backend/internal/review"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/agentsettings"
)

type reviewerSettingsService interface {
	ReviewerAgentSettings(context.Context, string) (domain.ReviewerAgentSettingsState, error)
	ChangeReviewerAgentSettings(context.Context, string, reviewcore.ReviewerSettingsChange) (domain.ReviewerAgentSettingsOperation, error)
}

func (c *ReviewsController) reviewerSettings(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.Svc.(reviewerSettingsService)
	if !ok {
		apispec.NotImplemented(w, r, "GET", "/api/v1/reviews/{reviewId}/agent-settings")
		return
	}
	ctx := ports.WithAgentSettingsCaller(r.Context(), settingsCaller(r))
	state, err := svc.ReviewerAgentSettings(ctx, chi.URLParam(r, "reviewId"))
	if err != nil {
		envelope.WriteError(w, r, agentsettings.SettingsError(err))
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ReviewerAgentSettingsResponse{ReviewID: state.Review.ID, Harness: state.Review.Harness, Revision: state.Revision, Settings: state.Settings, ActivePass: state.HasRunningPass, ApplyMode: "next-pass", EffectiveState: "unknown", Reason: "reviewer_effective_unavailable"})
}

func (c *ReviewsController) changeReviewerSettings(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.Svc.(reviewerSettingsService)
	if !ok {
		apispec.NotImplemented(w, r, "PATCH", "/api/v1/reviews/{reviewId}/agent-settings")
		return
	}
	var in ChangeReviewerAgentSettingsPayload
	if err := decodeAgentSettingsBody(w, r, &in); err != nil || in.ExpectedRevision == nil || *in.ExpectedRevision < 0 || in.Settings == (domain.AgentSettingsPatch{}) {
		envelope.WriteError(w, r, agentsettings.SettingsError(ports.ErrAgentSettingsInvalid))
		return
	}
	ctx := ports.WithAgentSettingsCaller(r.Context(), settingsCaller(r))
	op, err := svc.ChangeReviewerAgentSettings(ctx, chi.URLParam(r, "reviewId"), reviewcore.ReviewerSettingsChange{BaseRevision: *in.ExpectedRevision, Patch: in.Settings, IdempotencyKey: r.Header.Get("Idempotency-Key"), RequestID: middleware.GetReqID(r.Context())})
	if err != nil {
		envelope.WriteError(w, r, agentsettings.SettingsError(err))
		return
	}
	envelope.WriteJSON(w, http.StatusOK, ReviewerSettingsOperationResponse{ID: op.ID, ReviewID: op.Owner.ReviewID, BaseRevision: op.BaseRevision, Revision: op.Revision, Phase: op.Phase, ApplyMode: op.ApplyMode, Source: op.Source, Target: op.Target, Actor: op.Actor, PolicyRevision: op.PolicyRevision, RequestID: op.RequestID, CreatedAt: op.CreatedAt})
}
