package orchestratorreplacement_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	replacement "github.com/aoagents/agent-orchestrator/backend/internal/service/orchestratorreplacement"
)

func preparedDocument(t *testing.T) ([]byte, replacement.PreparationMetadata) {
	t.Helper()
	meta := replacement.PreparationMetadata{Version: 1, OperationID: "11111111-1111-4111-8111-111111111111", AttemptID: "22222222-2222-4222-8222-222222222222", Nonce: strings.Repeat("a", 64), SourceSessionID: "demo-1", LeaseEpoch: 4}
	b, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	text := "<!-- ao-handoff " + string(b) + " -->\n\n## A — 現況\n等待 review。\n\n## B — sessions\n| sessionId | task | state | nextD | lastACK | unansweredQ |\n| --- | --- | --- | --- | --- | --- |\n| demo-2 | D-8 尚待 ACK；ref=原 report | working | D-9 | D-7 | Q1、Q3（source=demo-2） |\n| demo-3 | 測試 | blocked | D-2 | 未確認 | 無 |\n\n## C — 停點與放行\n兩層 review 後等使用者。\n\n## D — 使用者指示\n不得 merge。\n\n## E — 待決\nworker demo-2 的 Q1、Q3。\n\n## F — 環境\n只使用隔離資料。\n"
	return []byte(text), meta
}
func TestReplacementPreparationFormatPreservesOpenQuestionsAndDirectives(t *testing.T) {
	b, meta := preparedDocument(t)
	if err := replacement.ValidatePreparation(b, meta, "missing", []domain.SessionID{"demo-2", "demo-3"}); err != nil {
		t.Fatal("valid current handoff refused:", err)
	}
	if !strings.Contains(string(b), "Q1、Q3（source=demo-2）") || !strings.Contains(string(b), "D-8 尚待 ACK") {
		t.Fatal("fixture lost source-scoped open Q/D")
	}
}
func TestReplacementPreparationRejectsStaleIncompleteOrUnboundedDocuments(t *testing.T) {
	original, meta := preparedDocument(t)
	for _, which := range []string{"wrong nonce", "wrong source", "wrong epoch", "unchanged", "missing F", "duplicate B", "missing worker", "blank Q", "empty C", "oversize", "invalid UTF8", "nul", "unknown metadata"} {
		t.Run(which, func(t *testing.T) {
			b := append([]byte(nil), original...)
			expected := meta
			baseline := "missing"
			switch which {
			case "wrong nonce":
				expected.Nonce = strings.Repeat("b", 64)
			case "wrong source":
				expected.SourceSessionID = "another-source"
			case "wrong epoch":
				expected.LeaseEpoch++
			case "unchanged":
				sum := sha256.Sum256(b)
				baseline = hex.EncodeToString(sum[:])
			case "missing F":
				b = []byte(strings.Split(string(b), "## F")[0])
			case "duplicate B":
				b = append(b, []byte("\n## B — duplicated\nbody\n")...)
			case "missing worker":
				b = []byte(strings.ReplaceAll(string(b), "| demo-3 | 測試 | blocked | D-2 | 未確認 | 無 |\n", ""))
			case "blank Q":
				b = []byte(strings.ReplaceAll(string(b), "| 無 |", "|  |"))
			case "empty C":
				b = []byte(strings.ReplaceAll(string(b), "兩層 review 後等使用者。", ""))
			case "oversize":
				b = append(b, []byte(strings.Repeat("x", replacement.MaxHandoffBytes))...)
			case "invalid UTF8":
				b = append(b, 0xff)
			case "nul":
				b = append(b, 0)
			case "unknown metadata":
				b = []byte(strings.Replace(string(b), `"version":1`, `"version":1,"unknown":"field"`, 1))
			}
			if err := replacement.ValidatePreparation(b, expected, baseline, []domain.SessionID{"demo-2", "demo-3"}); err == nil {
				t.Fatal("invalid preparation accepted")
			}
		})
	}
}
