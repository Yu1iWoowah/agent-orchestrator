package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
)

// QuestionService is the controller boundary for durable question operations.
type QuestionService interface {
	ProjectList(context.Context, domain.ProjectID, domain.ProjectAttentionQuery) (domain.ProjectAttentionPage, error)
	ReadReport(context.Context, domain.ProjectID, string) error
	Create(context.Context, domain.AttentionQuestionCreate) (domain.AttentionQuestionRecord, bool, error)
	Get(context.Context, domain.SessionID, string) (domain.AttentionQuestionRecord, error)
	List(context.Context, domain.SessionID, domain.AttentionQuestionQuery) (domain.AttentionQuestionPage, error)
	Answer(context.Context, domain.SessionID, domain.AttentionAnswerSubmission) (domain.AttentionAnswerReceipt, error)
	Revise(context.Context, domain.SessionID, string, int64, []byte) (domain.AttentionQuestionRecord, error)
	Cancel(context.Context, domain.SessionID, string, int64, string, string) (domain.AttentionQuestionRecord, error)
	Read(context.Context, domain.SessionID, string, int64) error
}

// QuestionsController adapts session question HTTP commands to the service.
type QuestionsController struct{ Svc QuestionService }

// Register installs the session-scoped question routes.
func (c *QuestionsController) Register(r chi.Router) {
	const root = "/sessions/{sessionId}/questions"
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.Svc == nil {
					apispec.NotImplemented(w, r, r.Method, r.URL.Path)
					return
				}
				next.ServeHTTP(w, r)
			})
		})
		r.Get("/projects/{id}/attention", c.projectList)
		r.Post("/projects/{id}/attention/reports/{reportId}/read", c.reportRead)
		r.Get(root, c.list)
		r.Post(root, c.create)
		r.Get(root+"/{questionId}", c.get)
		r.Post(root+"/{questionId}/answer", c.answer)
		r.Post(root+"/{questionId}/read", c.read)
		r.Post(root+"/{questionId}/cancel", c.cancel)
		r.Post(root+"/{questionId}/revise", c.revise)
	})
}

func questionScope(r *http.Request) (domain.SessionID, string) {
	return domain.SessionID(chi.URLParam(r, "sessionId")), chi.URLParam(r, "questionId")
}

func writeQuestionError(w http.ResponseWriter, r *http.Request, err error) {
	var validation *domain.AttentionValidationError
	switch {
	case errors.As(err, &validation):
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", "QUESTION_INVALID", validation.Message, map[string]any{"fieldErrors": map[string]string{validation.Field: validation.Message}})
	case errors.Is(err, domain.ErrAttentionQuestionForbidden):
		envelope.WriteAPIError(w, r, http.StatusForbidden, "forbidden", "QUESTION_FORWARD_FORBIDDEN", err.Error(), nil)
	case errors.Is(err, domain.ErrAttentionQuestionConflict):
		envelope.WriteAPIError(w, r, http.StatusConflict, "conflict", "QUESTION_CHANGED", "Question changed or is no longer open; reload before answering", nil)
	case errors.Is(err, domain.ErrAttentionQuestionNotFound):
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found", "QUESTION_NOT_FOUND", "Question or origin session not found", nil)
	default:
		envelope.WriteError(w, r, err)
	}
}

func readQuestionBody(w http.ResponseWriter, r *http.Request, limit int, target any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(limit)))
	if err != nil {
		writeQuestionError(w, r, &domain.AttentionValidationError{Field: "request", Message: "Cannot read question command or size limit exceeded"})
		return false
	}
	if err = domain.DecodeAttentionRequest(raw, limit, target); err != nil {
		writeQuestionError(w, r, err)
		return false
	}
	return true
}

// RawMessage retains unknown fields and explicit nulls for domain validation.
type questionCreateBody struct {
	ClientKey      string                       `json:"clientKey"`
	Audience       string                       `json:"audience"`
	Kind           domain.AttentionQuestionKind `json:"kind"`
	Payload        json.RawMessage              `json:"payload"`
	ExpiresAt      *time.Time                   `json:"expiresAt,omitempty"`
	SourceReportID string                       `json:"sourceReportId,omitempty"`
	SupersedesID   string                       `json:"supersedesId,omitempty"`
}

func (c *QuestionsController) create(w http.ResponseWriter, r *http.Request) {
	var body questionCreateBody
	if !readQuestionBody(w, r, domain.MaxAttentionCreateBytes, &body) {
		return
	}
	session, _ := questionScope(r)
	question, created, err := c.Svc.Create(r.Context(), domain.AttentionQuestionCreate{ActorCapability: r.Header.Get(browserCapabilityHeader), OriginSessionID: session, ClientKey: body.ClientKey, Audience: body.Audience, Kind: body.Kind, Payload: body.Payload, ExpiresAt: body.ExpiresAt, SupersedesID: body.SupersedesID, SourceReportID: body.SourceReportID})
	if err != nil {
		writeQuestionError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	envelope.WriteJSON(w, status, QuestionResponse(question))
}

func (c *QuestionsController) get(w http.ResponseWriter, r *http.Request) {
	session, id := questionScope(r)
	question, err := c.Svc.Get(r.Context(), session, id)
	if err != nil {
		writeQuestionError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, QuestionResponse(question))
}

func (c *QuestionsController) list(w http.ResponseWriter, r *http.Request) {
	query := domain.AttentionQuestionQuery{State: r.URL.Query().Get("state"), Limit: 20}
	var err error
	query.BeforeSequence, err = optionalPositiveInt64(r.URL.Query().Get("beforeSequence"))
	if err == nil && r.URL.Query().Get("limit") != "" {
		query.Limit, err = strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64)
	}
	if err != nil || query.Limit < 1 || query.Limit > 100 {
		writeQuestionError(w, r, &domain.AttentionValidationError{Field: "query", Message: "beforeSequence must be positive; limit must be between 1 and 100"})
		return
	}
	session, _ := questionScope(r)
	page, err := c.Svc.List(r.Context(), session, query)
	if err != nil {
		writeQuestionError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, QuestionPageResponse(page))
}

func (c *QuestionsController) answer(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, domain.MaxAttentionAnswerBytes))
	if err != nil {
		writeQuestionError(w, r, &domain.AttentionValidationError{Field: "answers", Message: "Answer JSON exceeds 32 KiB or cannot be read"})
		return
	}
	submission, err := domain.ParseAttentionAnswer(raw)
	if err != nil {
		writeQuestionError(w, r, err)
		return
	}
	session, id := questionScope(r)
	if submission.QuestionID != id {
		writeQuestionError(w, r, &domain.AttentionValidationError{Field: "questionId", Message: "must match the addressed question"})
		return
	}
	receipt, err := c.Svc.Answer(r.Context(), session, submission)
	if err != nil {
		writeQuestionError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, QuestionAnswerResponse(receipt))
}

func validQuestionVersion(w http.ResponseWriter, r *http.Request, version int64) bool {
	if version < 1 || version > domain.MaxAttentionQuestionVersion {
		writeQuestionError(w, r, &domain.AttentionValidationError{Field: "expectedVersion", Message: "must be a positive safe integer"})
		return false
	}
	return true
}

func (c *QuestionsController) read(w http.ResponseWriter, r *http.Request) {
	var body QuestionReadRequest
	if !readQuestionBody(w, r, 4096, &body) || !validQuestionVersion(w, r, body.ExpectedVersion) {
		return
	}
	session, id := questionScope(r)
	if err := c.Svc.Read(r.Context(), session, id, body.ExpectedVersion); err != nil {
		writeQuestionError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *QuestionsController) cancel(w http.ResponseWriter, r *http.Request) {
	var body QuestionCancelRequest
	if !readQuestionBody(w, r, 32*1024, &body) || !validQuestionVersion(w, r, body.ExpectedVersion) {
		return
	}
	session, id := questionScope(r)
	question, err := c.Svc.Cancel(r.Context(), session, id, body.ExpectedVersion, body.Actor, body.Reason)
	if err != nil {
		writeQuestionError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, QuestionResponse(question))
}

func (c *QuestionsController) revise(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ExpectedVersion int64           `json:"expectedVersion"`
		Payload         json.RawMessage `json:"payload"`
	}
	if !readQuestionBody(w, r, domain.MaxAttentionCreateBytes, &body) || !validQuestionVersion(w, r, body.ExpectedVersion) {
		return
	}
	session, id := questionScope(r)
	question, err := c.Svc.Revise(r.Context(), session, id, body.ExpectedVersion, body.Payload)
	if err != nil {
		writeQuestionError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, QuestionResponse(question))
}

func (c *QuestionsController) projectList(w http.ResponseWriter, r *http.Request) {
	query := domain.ProjectAttentionQuery{Audience: r.URL.Query().Get("audience"), Cursor: r.URL.Query().Get("cursor"), Limit: 20}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			writeQuestionError(w, r, &domain.AttentionValidationError{Field: "limit", Message: "must be an integer"})
			return
		}
		query.Limit = value
	}
	page, err := c.Svc.ProjectList(r.Context(), domain.ProjectID(chi.URLParam(r, "id")), query)
	if err != nil {
		writeQuestionError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, page)
}
func (c *QuestionsController) reportRead(w http.ResponseWriter, r *http.Request) {
	if err := c.Svc.ReadReport(r.Context(), domain.ProjectID(chi.URLParam(r, "id")), chi.URLParam(r, "reportId")); err != nil {
		writeQuestionError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
