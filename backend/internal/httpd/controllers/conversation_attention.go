package controllers

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

type attentionConversationService interface {
	Attention(context.Context, domain.SessionID, domain.ConversationAttentionQuery) (domain.ConversationAttentionPage, error)
	MarkAttentionRead(context.Context, domain.SessionID, string, string, int64) error
}

func (c *ConversationsController) attention(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.Svc.(attentionConversationService)
	if !ok {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/sessions/{sessionId}/attention")
		return
	}
	query := domain.ConversationAttentionQuery{State: r.URL.Query().Get("state"), Limit: 20}
	if query.State == "" {
		query.State = "open"
	}
	var err error
	query.BeforeSequence, err = optionalPositiveInt64(r.URL.Query().Get("beforeSequence"))
	if err == nil && r.URL.Query().Get("limit") != "" {
		query.Limit, err = strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64)
	}
	if err != nil || query.Limit < 1 || query.Limit > 100 || (query.State != "open" && query.State != "history") {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "ATTENTION_QUERY_INVALID", "state must be open or history, beforeSequence positive, and limit between 1 and 100", nil)
		return
	}
	page, err := svc.Attention(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), query)
	if err != nil {
		writeConversationError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, attentionPageResponse(page))
}

func (c *ConversationsController) attentionSource(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.Svc.(attentionConversationService)
	if !ok {
		apispec.NotImplemented(w, r, http.MethodGet, "/api/v1/sessions/{sessionId}/attention/{activityId}")
		return
	}
	page, err := svc.Attention(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), domain.ConversationAttentionQuery{State: "all", ActivityID: chi.URLParam(r, "activityId"), Limit: 1})
	if err == nil && len(page.Items) == 0 {
		err = domain.ErrAttentionChanged
	}
	if err != nil {
		writeConversationError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, attentionPageResponse(page))
}

func (c *ConversationsController) attentionRead(w http.ResponseWriter, r *http.Request) {
	svc, ok := c.Svc.(attentionConversationService)
	if !ok {
		apispec.NotImplemented(w, r, http.MethodPost, "/api/v1/sessions/{sessionId}/attention/{activityId}/read")
		return
	}
	request := AttentionReadRequest{ExpectedRevision: -1}
	if !decodeConversationBody(w, r, &request) {
		return
	}
	if request.ExpectedRevision < 0 || request.ExpectedBranchID == "" {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "ATTENTION_READ_INVALID", "expectedBranchId and nonnegative expectedRevision are required", nil)
		return
	}
	if err := svc.MarkAttentionRead(r.Context(), domain.SessionID(chi.URLParam(r, "sessionId")), request.ExpectedBranchID, chi.URLParam(r, "activityId"), request.ExpectedRevision); err != nil {
		writeConversationError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func attentionPageResponse(page domain.ConversationAttentionPage) SessionAttentionResponse {
	response := SessionAttentionResponse{ConversationID: page.ConversationID, ActiveBranchID: page.ActiveBranchID,
		Items: make([]AttentionItemResponse, 0, len(page.Items)), Counts: AttentionCountsResponse{Open: page.Counts.Open, Unread: page.Counts.Unread, History: page.Counts.History}, HasMoreBefore: page.HasMoreBefore, NextBeforeSequence: page.NextBeforeSequence}
	for _, item := range page.Items {
		a := item.Activity
		response.Items = append(response.Items, AttentionItemResponse{BranchID: item.BranchID, Actionable: item.Actionable, Read: item.Read,
			Activity: ConversationActivityResponse{Kind: "activity", ID: a.ID, TurnID: a.TurnID, Sequence: a.Sequence, Revision: a.Revision, ActivityKind: string(a.Kind), Status: string(a.Status), Summary: a.Summary, Detail: activityDetailPayload(a), RequestID: a.RequestID, ProviderItemID: a.ProviderItemID, CreatedAt: a.CreatedAt.UTC().Format(time.RFC3339)}})
	}
	return response
}
