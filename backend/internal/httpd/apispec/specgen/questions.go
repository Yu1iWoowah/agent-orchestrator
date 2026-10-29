package specgen

import (
	"net/http"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

type questionListQuery struct {
	State          string `query:"state" enum:"open,history,all"`
	BeforeSequence int64  `query:"beforeSequence" minimum:"1"`
	Limit          int64  `query:"limit" minimum:"1" maximum:"100"`
}

func questionOperations() []operation {
	const root = "/api/v1/sessions/{sessionId}/questions"
	common := []respUnit{{http.StatusForbidden, envelope.APIError{}}, {http.StatusBadRequest, envelope.APIError{}}, {http.StatusNotFound, envelope.APIError{}}, {http.StatusConflict, envelope.APIError{}}, {http.StatusInternalServerError, envelope.APIError{}}, {http.StatusNotImplemented, envelope.APIError{}}}
	responses := func(status int, body any) []respUnit { return append([]respUnit{{status, body}}, common...) }
	session := []any{controllers.SessionIDParam{}}
	question := []any{controllers.SessionIDParam{}, controllers.QuestionIDParam{}}
	createResponses := responses(http.StatusCreated, controllers.QuestionResponse{})
	createResponses = append(createResponses, respUnit{http.StatusOK, controllers.QuestionResponse{}})
	return []operation{
		{method: http.MethodGet, path: "/api/v1/projects/{id}/attention", id: "listProjectAttention", tag: "projects", summary: "Read project human decisions or coordination reports with complete counts", pathParams: []any{controllers.ProjectIDParam{}, projectAttentionQuery{}}, resps: responses(http.StatusOK, domain.ProjectAttentionPage{})},
		{method: http.MethodPost, path: "/api/v1/projects/{id}/attention/reports/{reportId}/read", id: "readProjectAttentionReport", tag: "projects", summary: "Mark an inspected report seen without changing delivery or answer state", pathParams: []any{controllers.ProjectIDParam{}, attentionReportIDParam{}}, resps: responses(http.StatusNoContent, nil)},
		{method: http.MethodPost, path: root, id: "createSessionQuestion", tag: "conversations", summary: "Create an idempotent user question and its readable compatibility record", pathParams: session, reqBody: controllers.QuestionCreateRequest{}, resps: createResponses},
		{method: http.MethodGet, path: root, id: "listSessionQuestions", tag: "conversations", summary: "List structured questions with complete open, unread and history counts", pathParams: append([]any{controllers.SessionIDParam{}}, questionListQuery{}), resps: responses(http.StatusOK, controllers.QuestionPageResponse{})},
		{method: http.MethodGet, path: root + "/{questionId}", id: "getSessionQuestion", tag: "conversations", summary: "Read a fresh question version and any saved answer receipt", pathParams: question, resps: responses(http.StatusOK, controllers.QuestionResponse{})},
		{method: http.MethodPost, path: root + "/{questionId}/answer", id: "answerSessionQuestion", tag: "conversations", summary: "Atomically save all answers and queue durable delivery to the origin agent", pathParams: question, reqBody: domain.AttentionAnswerSubmission{}, resps: responses(http.StatusOK, controllers.QuestionAnswerResponse{})},
		{method: http.MethodPost, path: root + "/{questionId}/read", id: "readSessionQuestion", tag: "conversations", summary: "Mark an exact question version seen without answering it", pathParams: question, reqBody: controllers.QuestionReadRequest{}, resps: responses(http.StatusNoContent, nil)},
		{method: http.MethodPost, path: root + "/{questionId}/cancel", id: "cancelSessionQuestion", tag: "conversations", summary: "Cancel an open question using compare-and-set", pathParams: question, reqBody: controllers.QuestionCancelRequest{}, resps: responses(http.StatusOK, controllers.QuestionResponse{})},
		{method: http.MethodPost, path: root + "/{questionId}/revise", id: "reviseSessionQuestion", tag: "conversations", summary: "Replace an open question payload and advance its version", pathParams: question, reqBody: controllers.QuestionReviseRequest{}, resps: responses(http.StatusOK, controllers.QuestionResponse{})},
	}
}

type projectAttentionQuery struct {
	Audience string `query:"audience" enum:"user,orchestrator"`
	Cursor   string `query:"cursor"`
	Limit    int    `query:"limit" minimum:"1" maximum:"100"`
}
type attentionReportIDParam struct {
	ReportID string `path:"reportId"`
}
