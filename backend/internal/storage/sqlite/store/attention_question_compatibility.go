package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

func questionContext(question domain.AttentionQuestionRecord) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Question %s (version %d): %s\n\n%s\n", question.ID, question.Version, question.Payload.Title, question.Payload.Explanation)
	if auth := question.Payload.Authorization; auth != nil {
		fmt.Fprintf(&out, "\nAuthorization action: %s\nScope: %s\nLimits: %s\nEvidence:\n%s\n", auth.Action, auth.Scope, auth.Limits, strings.Join(auth.Evidence, "\n"))
	}
	for _, prompt := range question.Payload.Questions {
		fmt.Fprintf(&out, "\n[%s] %s: %s\n", prompt.Key, prompt.Header, prompt.Question)
		if prompt.Explanation != nil {
			fmt.Fprintf(&out, "%s\n", *prompt.Explanation)
		}
		for _, option := range prompt.Options {
			recommended := ""
			if option.Recommended {
				recommended = " (Recommended)"
			}
			fmt.Fprintf(&out, "- %s: %s%s — %s\n", option.ID, option.Label, recommended, option.Description)
			if option.Preview != nil {
				fmt.Fprintf(&out, "Preview (%s):\n%s\n", option.Preview.Format, option.Preview.Content)
			}
		}
		fmt.Fprintf(&out, "- Other: free text. Multiple selection: %t.\n", prompt.MultiSelect)
	}
	out.WriteString("\nAnswer this structured question using its question ID and expected version. Ordinary chat does not close it.\n")
	return out.String()
}

func questionAnswerContext(question domain.AttentionQuestionRecord, answers []domain.AttentionQuestionAnswer) string {
	var out strings.Builder
	fmt.Fprintf(&out, "The user answered question %s (version %d). This is human direction, not approval of any provider tool request.\n\n%s", question.ID, question.Version, questionContext(question))
	for i, answer := range answers {
		prompt := question.Payload.Questions[i]
		fmt.Fprintf(&out, "\nAnswer [%s] %s:\n", prompt.Key, prompt.Question)
		for _, option := range prompt.Options {
			for _, selected := range answer.SelectedOptionIDs {
				if selected == option.ID {
					fmt.Fprintf(&out, "- %s: %s — %s\n", option.ID, option.Label, option.Description)
				}
			}
		}
		if answer.OtherText != nil {
			fmt.Fprintf(&out, "- Other: %s\n", *answer.OtherText)
		}
	}
	return out.String()
}

// The readable compatibility projection commits with its canonical question or
// answer. It remains a system row, never a synthetic provider approval.
func appendQuestionCompatibility(ctx context.Context, q *gen.Queries, question domain.AttentionQuestionRecord, event, message string, at time.Time) error {
	conversation, err := q.SelectConversationBySession(ctx, &question.OriginSessionID)
	if errors.Is(err, sql.ErrNoRows) {
		id := uuid.NewString()
		root := id + ":root"
		owner, getErr := q.GetSession(ctx, question.OriginSessionID)
		if getErr != nil {
			return getErr
		}
		if err := q.InsertConversation(ctx, gen.InsertConversationParams{ID: id, Scope: domain.ConversationScopeSession, ProjectID: optionalProjectID(question.ProjectID), SessionID: &question.OriginSessionID, CurrentSessionID: &question.OriginSessionID, ActiveBranchID: root, CreatedAt: at.UTC(), UpdatedAt: at.UTC()}); err != nil {
			return err
		}
		if err := q.InsertConversationBranch(ctx, gen.InsertConversationBranchParams{ID: root, ConversationID: id, SessionID: nullableString(string(question.OriginSessionID)), ProviderConversationID: owner.ProviderConversationID, ProviderScopeID: root, ProviderIdsScoped: 1, CreatedAt: at.UTC()}); err != nil {
			return err
		}
		conversation, err = q.SelectConversationBySession(ctx, &question.OriginSessionID)
	}
	if err != nil {
		return err
	}
	sequence, err := q.NextConversationSequence(ctx, gen.NextConversationSequenceParams{ID: conversation.ID, UpdatedAt: at.UTC()})
	if err != nil {
		return err
	}
	detail, err := json.Marshal(struct {
		Event      string `json:"event"`
		QuestionID string `json:"questionId"`
		Text       string `json:"text"`
	}{"agent_question." + event, question.ID, message})
	if err != nil {
		return err
	}
	summary := message
	if runes := []rune(summary); len(runes) > 512 {
		summary = string(runes[:511]) + "…"
	}
	return q.InsertConversationActivity(ctx, gen.InsertConversationActivityParams{ID: uuid.NewString(), ConversationID: conversation.ID, Sequence: sequence, Kind: domain.ActivityKindSystem, Status: domain.ActivityStatusCompleted, Summary: summary, DetailJson: string(detail), CreatedAt: at.UTC(), UpdatedAt: at.UTC()})
}
