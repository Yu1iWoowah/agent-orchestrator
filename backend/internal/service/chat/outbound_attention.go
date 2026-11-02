package chat

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type outboundAttentionStore interface {
	deliveryAttentionWriter
	ListAttentionQuestions(context.Context, domain.SessionID, domain.AttentionQuestionQuery, time.Time) (domain.AttentionQuestionPage, error)
	CancelAttentionQuestion(context.Context, domain.SessionID, string, int64, string, string, time.Time) (domain.AttentionQuestionRecord, error)
}

// A failed shutdown must leave a visible reason and a way to retry native
// reconciliation. A user's acknowledgement is never execution evidence.
func outboundAttention(ctx context.Context, store any, session domain.SessionID, scope string, settled bool, at time.Time) error {
	questions, ok := store.(outboundAttentionStore)
	if !ok {
		return nil
	}
	digest := sha256.Sum256([]byte(scope))
	prefix := fmt.Sprintf("outbound-%x-", digest[:8])
	var before int64
	found := false
	for {
		page, err := questions.ListAttentionQuestions(ctx, session, domain.AttentionQuestionQuery{State: "open", Limit: 100, BeforeSequence: before}, at)
		if err != nil {
			return err
		}
		for _, q := range page.Items {
			if !strings.HasPrefix(q.ClientKey, prefix) {
				continue
			}
			found = true
			if settled {
				if _, err := questions.CancelAttentionQuestion(ctx, session, q.ID, q.Version, "agent", "已取得原生結算證據", at); err != nil {
					return err
				}
			}
		}
		if !page.HasMoreBefore {
			break
		}
		before = page.NextBeforeSequence
	}
	if settled || found {
		return nil
	}
	payload, err := json.Marshal(domain.AttentionQuestionPayload{SchemaVersion: 1, Title: "原生操作尚待確認", Explanation: "AO 保留原連線與操作。新輸入會保留等待。請檢查原執行；可重試壓縮或再次送出問題以查證原操作，亦可在執行結束或連線恢復後重試關閉。若無法取得證據，可使用 session 的 Exit agent、封存該 review 或關閉該 side chat 來明確停止原執行。僅確認提示不會解除保護，也不會重送內容。", Questions: []domain.AttentionPrompt{{Key: "outbound", Header: "操作待確認", Question: "請確認已看到未結算操作；檢查原執行後可重試操作或關閉。", Options: []domain.AttentionQuestionOption{{ID: "inspect", Label: "檢查原執行後重試", Description: "保留原操作，重試時依原生證據查證。", Recommended: true}, {ID: "keep", Label: "保留原操作並等待", Description: "等待原生證據或連線恢復，再重試關閉。"}}}}})
	if err != nil {
		return err
	}
	_, _, err = questions.CreateAttentionQuestion(ctx, domain.AttentionQuestionCreate{ID: uuid.NewString(), OriginSessionID: session, ClientKey: prefix + uuid.NewString(), Audience: "user", Kind: domain.AttentionQuestionKindQuestion, Payload: payload}, at)
	return err
}

func (c *Controller) outboundAttention(ctx context.Context, settled bool) error {
	if _, ok := c.store.(outboundAttentionStore); !ok {
		return nil
	}
	return outboundAttention(ctx, c.store, c.sessionID, "conversation:"+c.ConversationID(), settled, c.now())
}

func (c *Controller) clearOutboundAttention(ctx context.Context) {
	if err := c.outboundAttention(ctx, true); err != nil {
		c.log.Warn("outbound attention cleanup failed", "session", c.sessionID, "code", "CHAT_ATTENTION_CLEANUP_FAILED")
	}
}
