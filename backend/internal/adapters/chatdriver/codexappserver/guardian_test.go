package codexappserver

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestA2A8ReviewMetadataCompatibilityAndWhitelist(t *testing.T) {
	const secret = "SENSITIVE_CONTENT_SENTINEL"
	for _, action := range []string{"command", "execve", "writeStdin", "applyPatch", "networkAccess", "mcpToolCall", "requestPermissions", "futureAction"} {
		for _, method := range []string{"item/autoApprovalReview/started", "item/autoApprovalReview/completed"} {
			t.Run(action+"/"+method, func(t *testing.T) {
				payload := `{"threadId":"parent","turnId":"turn","reviewId":"review","targetItemId":null,"startedAtMs":1700000000000,"completedAtMs":1700000001000,"decisionSource":"agent","review":{"status":"approved","rationale":"` + secret + `"},"action":{"type":"` + action + `","command":"` + secret + `","stdin":"` + secret + `","argv":["` + secret + `"],"cwd":"` + secret + `","files":["` + secret + `"],"host":"` + secret + `","server":"` + secret + `","toolName":"` + secret + `","reason":"` + secret + `","future":{"message":"` + secret + `"}}}`
				events := normalizeNotification(notification{Method: method, Params: json.RawMessage(payload)}, time.Now())
				if len(events) != 1 || events[0].GuardianReview == nil {
					t.Fatal("review event lost")
				}
				r := events[0].GuardianReview
				if r.Action != domain.GuardianAction(action) || r.ParentThreadID != "parent" || r.TargetItemID != nil {
					t.Fatal("metadata mismatch")
				}
				safe, err := json.Marshal(r)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(safe), secret) {
					t.Fatal("provider content entered metadata")
				}
				// The existing Chat detail behavior remains independent of observation.
				if len(events[0].Detail) == 0 {
					t.Fatal("existing activity lost")
				}
			})
		}
	}
}
