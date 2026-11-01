package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// The CLI mirrors command envelopes and uses only daemon HTTP. Business state
// and origin ownership are never inferred from local files or provider output.
type askCreateDTO struct {
	ClientKey      string          `json:"clientKey"`
	Audience       string          `json:"audience"`
	Kind           string          `json:"kind"`
	Payload        json.RawMessage `json:"payload"`
	ExpiresAt      *time.Time      `json:"expiresAt,omitempty"`
	SourceReportID string          `json:"sourceReportId,omitempty"`
	SupersedesID   string          `json:"supersedesId,omitempty"`
}
type askRevisionDTO struct {
	ExpectedVersion int64           `json:"expectedVersion"`
	Payload         json.RawMessage `json:"payload"`
}
type askVersionDTO struct {
	ExpectedVersion int64 `json:"expectedVersion"`
}
type askCancelDTO struct {
	ExpectedVersion int64  `json:"expectedVersion"`
	Actor           string `json:"actor"`
	Reason          string `json:"reason"`
}
type askOptions struct {
	session string
	json    bool
}

func newAskCommand(ctx *commandContext) *cobra.Command {
	options := &askOptions{}
	cmd := &cobra.Command{Use: "ask", Short: "Create and answer durable structured questions for the user"}
	cmd.PersistentFlags().StringVar(&options.session, "session", "", "Origin session ID (default: AO_SESSION_ID)")
	cmd.PersistentFlags().BoolVar(&options.json, "json", false, "Print JSON including version and delivery receipt")
	cmd.AddCommand(newAskCreateCommand(ctx, options), newAskListCommand(ctx, options), newAskProjectCommand(ctx, options))
	for _, action := range []string{"get", "answer", "read", "cancel", "revise"} {
		cmd.AddCommand(newAskQuestionCommand(ctx, options, action))
	}
	return cmd
}

func askSessionPath(options *askOptions) (string, error) {
	id := strings.TrimSpace(options.session)
	if id == "" {
		id = strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
	}
	if id == "" {
		return "", usageError{errors.New("--session or AO_SESSION_ID is required")}
	}
	return "sessions/" + url.PathEscape(id) + "/questions", nil
}

func askPayloadFile(cmd *cobra.Command, path string, limit int) ([]byte, error) {
	if path == "" {
		return nil, usageError{errors.New("--payload-file is required")}
	}
	var source io.Reader
	if path == "-" {
		source = cmd.InOrStdin()
	} else {
		file, err := os.Open(path)
		if err != nil {
			return nil, usageError{fmt.Errorf("read payload: %w", err)}
		}
		defer func() { _ = file.Close() }()
		source = file
	}
	raw, err := io.ReadAll(io.LimitReader(source, int64(limit)+1))
	if err != nil {
		return nil, usageError{fmt.Errorf("read payload: %w", err)}
	}
	if len(raw) > limit {
		return nil, usageError{fmt.Errorf("payload exceeds %d bytes", limit)}
	}
	return raw, nil
}

func newAskCreateCommand(ctx *commandContext, options *askOptions) *cobra.Command {
	var key, audience, kind, file, expires, supersedes, sourceReport string
	cmd := &cobra.Command{Use: "create", Short: "Create one user-facing question group", Args: usageArgs(cobra.NoArgs), RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := askSessionPath(options)
		if err != nil {
			return err
		}
		if err = domain.ValidateAttentionClientKey(key); err != nil {
			return usageError{err}
		}
		if audience != "user" {
			return usageError{errors.New("--audience must be user")}
		}
		raw, err := askPayloadFile(cmd, file, domain.MaxAttentionCreateBytes)
		if err != nil {
			return err
		}
		if _, err = domain.ParseAttentionQuestionPayload(raw, domain.AttentionQuestionKind(kind)); err != nil {
			return usageError{err}
		}
		body := askCreateDTO{ClientKey: key, Audience: audience, Kind: kind, Payload: raw, SupersedesID: supersedes, SourceReportID: sourceReport}
		if expires != "" {
			at, err := time.Parse(time.RFC3339, expires)
			if err != nil {
				return usageError{errors.New("--expires-at must be RFC3339")}
			}
			body.ExpiresAt = &at
		}
		if err := validateAskEnvelope(body); err != nil {
			return err
		}
		var headers map[string]string
		if sourceReport != "" {
			actor := strings.TrimSpace(os.Getenv("AO_SESSION_ID"))
			capability := strings.TrimSpace(os.Getenv("AO_BROWSER_CAPABILITY"))
			if actor == "" || capability == "" || options.session != "" && strings.TrimSpace(options.session) != actor {
				return usageError{errors.New("report forwarding requires the owning AO_SESSION_ID and AO_BROWSER_CAPABILITY; --session cannot impersonate another session")}
			}
			headers = map[string]string{browserCapabilityHeader: capability}
		}
		var response json.RawMessage
		if err := ctx.doJSONPathWithHeaders(cmd.Context(), http.MethodPost, "/api/v1/"+path, body, &response, headers); err != nil {
			return err
		}
		return printAskResponse(cmd, options, response, "create")
	}}
	f := cmd.Flags()
	f.StringVar(&key, "key", "", "Stable 1–64 character client key for idempotent creation")
	f.StringVar(&audience, "audience", "user", "Question audience (user)")
	f.StringVar(&kind, "kind", "question", "question or authorization")
	f.StringVar(&file, "payload-file", "", "UTF-8 question payload JSON file, or - for stdin")
	f.StringVar(&expires, "expires-at", "", "Optional explicit RFC3339 deadline")
	f.StringVar(&sourceReport, "source-report", "", "Forward a worker report; requires the active same-project orchestrator session")
	f.StringVar(&supersedes, "supersedes-id", "", "Terminal question replaced by this new question")
	return cmd
}

func newAskListCommand(ctx *commandContext, options *askOptions) *cobra.Command {
	var state string
	var before, limit int64
	cmd := &cobra.Command{Use: "list", Aliases: []string{"ls"}, Short: "List questions and complete pending/unread counts", Args: usageArgs(cobra.NoArgs), RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := askSessionPath(options)
		if err != nil {
			return err
		}
		if (state != "open" && state != "history" && state != "all") || before < 0 || limit < 1 || limit > 100 {
			return usageError{errors.New("state must be open, history or all; before-sequence nonnegative; limit 1–100")}
		}
		query := url.Values{"state": {state}, "limit": {strconv.FormatInt(limit, 10)}}
		if before > 0 {
			query.Set("beforeSequence", strconv.FormatInt(before, 10))
		}
		var response json.RawMessage
		if err := ctx.getJSON(cmd.Context(), path+"?"+query.Encode(), &response); err != nil {
			return err
		}
		return printAskResponse(cmd, options, response, "list")
	}}
	cmd.Flags().StringVar(&state, "state", "open", "open, history or all")
	cmd.Flags().Int64Var(&before, "before-sequence", 0, "Page before this sequence")
	cmd.Flags().Int64Var(&limit, "limit", 20, "Page size (1–100)")
	return cmd
}

func newAskQuestionCommand(ctx *commandContext, options *askOptions, action string) *cobra.Command {
	var version int64
	var file, reason, actor string
	cmd := &cobra.Command{Use: action + " <question-id>", Short: strings.ToUpper(action[:1]) + action[1:] + " a structured question", Args: usageArgs(cobra.ExactArgs(1)), RunE: func(cmd *cobra.Command, args []string) error {
		path, err := askSessionPath(options)
		if err != nil {
			return err
		}
		path += "/" + url.PathEscape(args[0])
		var response json.RawMessage
		if action == "get" {
			if err := ctx.getJSON(cmd.Context(), path, &response); err != nil {
				return err
			}
			return printAskResponse(cmd, options, response, action)
		}
		var body any
		switch action {
		case "answer":
			raw, err := askPayloadFile(cmd, file, domain.MaxAttentionAnswerBytes)
			if err != nil {
				return err
			}
			answer, err := domain.ParseAttentionAnswer(raw)
			if err != nil {
				return usageError{err}
			}
			if answer.QuestionID != args[0] {
				return usageError{errors.New("questionId in payload must match the addressed question")}
			}
			body = answer
		case "read", "cancel", "revise":
			if version < 1 || version > domain.MaxAttentionQuestionVersion {
				return usageError{errors.New("--expected-version must be a positive safe integer")}
			}
			switch action {
			case "read":
				body = askVersionDTO{version}
			case "cancel":
				if strings.TrimSpace(reason) == "" || (actor != "agent" && actor != "user") {
					return usageError{errors.New("--reason is required and --actor must be agent or user")}
				}
				body = askCancelDTO{version, actor, reason}
			case "revise":
				raw, err := askPayloadFile(cmd, file, domain.MaxAttentionCreateBytes)
				if err != nil {
					return err
				}
				var metadata struct {
					Authorization json.RawMessage `json:"authorization"`
				}
				if err = json.Unmarshal(raw, &metadata); err != nil {
					return usageError{err}
				}
				kind := domain.AttentionQuestionKindQuestion
				if len(metadata.Authorization) > 0 {
					kind = domain.AttentionQuestionKindAuthorization
				}
				if _, err = domain.ParseAttentionQuestionPayload(raw, kind); err != nil {
					return usageError{err}
				}
				body = askRevisionDTO{version, raw}
			}
		}
		if action == "read" {
			if err := ctx.postJSON(cmd.Context(), path+"/read", body, nil); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Marked question version read.")
			return err
		}
		if action == "revise" {
			if err := validateAskEnvelope(body); err != nil {
				return err
			}
		}
		if err := ctx.postJSON(cmd.Context(), path+"/"+action, body, &response); err != nil {
			return err
		}
		return printAskResponse(cmd, options, response, action)
	}}
	if action == "answer" {
		cmd.Long = "Answer every prompt. Each answer may include optional notes alongside a choice or Other; top-level notes apply to the whole group. Notes and Other each allow 1–4000 Unicode characters, with non-whitespace content."
		cmd.Example = `  ao ask answer ask-1 --payload-file answer.json
  # answer.json:
  # {"questionId":"ask-1","expectedVersion":1,"clientAnswerId":"reply-1","answers":[{"questionKey":"target","selectedOptionIds":["stage"],"notes":"Stage first; hold PR3."}],"notes":"Can we deploy tomorrow?"}`
	}
	if action == "answer" || action == "revise" {
		cmd.Flags().StringVar(&file, "payload-file", "", "UTF-8 JSON file, or - for stdin")
	}
	if action == "read" || action == "cancel" || action == "revise" {
		cmd.Flags().Int64Var(&version, "expected-version", 0, "Exact version read before this command")
	}
	if action == "cancel" {
		cmd.Flags().StringVar(&reason, "reason", "", "Why this question is being cancelled")
		cmd.Flags().StringVar(&actor, "actor", "agent", "agent or user")
	}
	return cmd
}

func printAskResponse(cmd *cobra.Command, options *askOptions, raw json.RawMessage, action string) error {
	if options.json {
		return writeJSON(cmd.OutOrStdout(), raw)
	}
	if action == "answer" {
		var receipt domain.AttentionAnswerReceipt
		if err := json.Unmarshal(raw, &receipt); err != nil {
			return err
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "Saved answer %s; agent delivery: %s\n", receipt.ClientAnswerID, receipt.DeliveryState)
		return err
	}
	if action == "list" {
		var page domain.AttentionQuestionPage
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Open: %d; unread: %d; history: %d\n", page.Open, page.Unread, page.History); err != nil {
			return err
		}
		for _, q := range page.Items {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\tv%d\t%s\n", q.ID, q.State, q.Version, q.Payload.Title); err != nil {
				return err
			}
		}
		if page.HasMoreBefore {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "Older questions: --before-sequence %d\n", page.NextBeforeSequence)
			return err
		}
		return nil
	}
	var question domain.AttentionQuestionRecord
	if err := json.Unmarshal(raw, &question); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s [%s] version %d\n%s\n\n%s\n", question.ID, question.State, question.Version, question.Payload.Title, question.Payload.Explanation); err != nil {
		return err
	}
	// Keep the complete structured details readable, including option descriptions,
	// authorization scope and any saved answer, even in older text-only clients.
	return writeJSON(cmd.OutOrStdout(), question)
}

// Match the actual encoded HTTP envelope, including metadata outside payload.
func validateAskEnvelope(body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return usageError{err}
	}
	if len(raw) > domain.MaxAttentionCreateBytes {
		return usageError{fmt.Errorf("question envelope exceeds %d bytes", domain.MaxAttentionCreateBytes)}
	}
	return nil
}

func newAskProjectCommand(ctx *commandContext, options *askOptions) *cobra.Command {
	var project, audience, cursor string
	var limit int
	cmd := &cobra.Command{Use: "project", Short: "List project human decisions or orchestrator coordination reports", Args: usageArgs(cobra.NoArgs), RunE: func(cmd *cobra.Command, _ []string) error {
		id := strings.TrimSpace(project)
		if id == "" {
			id = strings.TrimSpace(os.Getenv("AO_PROJECT_ID"))
		}
		if id == "" || (audience != "user" && audience != "orchestrator") || limit < 1 || limit > 100 {
			return usageError{errors.New("--project or AO_PROJECT_ID required; audience user or orchestrator; limit 1–100")}
		}
		query := url.Values{"audience": {audience}, "limit": {strconv.Itoa(limit)}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var page domain.ProjectAttentionPage
		if err := ctx.getJSON(cmd.Context(), "projects/"+url.PathEscape(id)+"/attention?"+query.Encode(), &page); err != nil {
			return err
		}
		if options.json {
			return writeJSON(cmd.OutOrStdout(), page)
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Audience: %s; items: %d; unread: %d\n", audience, page.Open, page.Unread); err != nil {
			return err
		}
		for _, item := range page.Items {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", item.ID, item.SessionID, item.Title); err != nil {
				return err
			}
		}
		if page.NextCursor != "" {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "Next: --cursor %s\n", page.NextCursor)
			return err
		}
		return nil
	}}
	cmd.Flags().StringVar(&project, "project", "", "Project ID (default: AO_PROJECT_ID)")
	cmd.Flags().StringVar(&audience, "audience", "user", "user decisions or orchestrator reports")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Opaque nextCursor from the previous page")
	cmd.Flags().IntVar(&limit, "limit", 20, "Page size (1–100)")
	return cmd
}
