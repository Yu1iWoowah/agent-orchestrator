package controllers

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	chatsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/chat"
	"github.com/go-chi/chi/v5"
)

type planConversationService interface {
	Plan(context.Context, domain.SessionID) (chatsvc.PlanSnapshot, error)
	PlanHistory(context.Context, domain.SessionID, int64, int64) (chatsvc.PlanHistory, error)
}

func (c *ConversationsController) plan(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.Svc.(planConversationService)
	if !ok {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/sessions/{sessionId}/plan")
		return
	}
	snapshot, err := svc.Plan(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")))
	if err != nil {
		writeConversationError(w, r, err)
		return
	}
	response := SessionPlanResponse{State: "absent", ConversationID: snapshot.ConversationID, ActiveBranchID: snapshot.ActiveBranchID}
	if snapshot.Version != nil {
		version := conversationPlanVersionResponse(*snapshot.Version)
		response.Version = &version
		response.State = "present"
		if len(version.Plan.Steps) == 0 {
			response.State = "cleared"
		}
	}
	envelope.WriteJSON(w, http.StatusOK, response)
}

func (c *ConversationsController) planHistory(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.Svc.(planConversationService)
	if !ok {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/sessions/{sessionId}/plan/history")
		return
	}
	before, err := optionalPositiveInt64(r.URL.Query().Get("beforeSequence"))
	if err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "PLAN_CURSOR_INVALID", err.Error(), nil)
		return
	}
	limit := int64(20)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || limit < 1 || limit > 100 {
			envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "PLAN_LIMIT_INVALID", "limit must be between 1 and 100", nil)
			return
		}
	}
	history, err := svc.PlanHistory(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), before, limit)
	if err != nil {
		writeConversationError(w, r, err)
		return
	}
	versions := make([]ConversationPlanVersionResponse, 0, len(history.Versions))
	for _, version := range history.Versions {
		versions = append(versions, conversationPlanVersionResponse(version))
	}
	envelope.WriteJSON(w, http.StatusOK, ConversationPlanHistoryResponse{
		ConversationID: history.ConversationID, ActiveBranchID: history.ActiveBranchID,
		Versions: versions, HasMoreBefore: history.HasMoreBefore, NextBeforeSequence: history.NextBeforeSequence,
	})
}

func conversationPlanVersionResponse(version domain.ConversationPlanVersion) ConversationPlanVersionResponse {
	steps := make([]ConversationPlanStepResponse, 0, len(version.Plan.Steps))
	for _, step := range version.Plan.Steps {
		steps = append(steps, ConversationPlanStepResponse{Text: step.Text, Status: string(step.Status)})
	}
	return ConversationPlanVersionResponse{
		BranchID: version.BranchID, TurnID: version.TurnID, SourceActivityID: version.SourceActivityID,
		Sequence: version.Sequence, Revision: version.Revision, UpdatedAt: version.UpdatedAt.UTC().Format(time.RFC3339Nano),
		Plan: ConversationPlanResponse{Explanation: version.Plan.Explanation, SpansTurns: version.Plan.SpansTurns, Steps: steps},
	}
}
