package orchestratorreplacement

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func quoteArgument(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

func preparationPrompt(op domain.OrchestratorReplacement) string {
	p := op.Preparation
	metadata, _ := json.Marshal(PreparationMetadata{Version: 1, OperationID: op.ID, AttemptID: p.AttemptID, Nonce: p.Nonce, SourceSessionID: op.SourceSessionID, LeaseEpoch: p.LeaseEpoch})
	sessions, _ := json.Marshal(p.ExpectedSessions)
	draft := filepath.Join(p.ScratchDir, "handoff-draft.md")
	return fmt.Sprintf(`AO 正在執行 orchestrator replacement。這是 daemon 的 preparing_handoff control turn。
只準備交接文件，不派新工作，不自行 stop / kill / spawn / replace，不變更使用者授權，也不要打斷 workers。
Source session: %s
Source native main conversation: %s
Operation: %s
Shared root（living files 的固定位置）: %s
Configured handoff relative path: %s
Draft path（可寫的 AO scratch，並非正式 evidence）: %s
本次 lease epoch: %d
§B 至少列出這些 source session ids: %s

文件第一行必須逐字使用下列 metadata，且不包在 code fence 中：
<!-- ao-handoff %s -->

依序使用以下六個 Markdown headings（只用這六個 ## headings；子題用 ###）：
## A — 一句話現況
## B — session 地圖
## C — 停點與放行條件
## D — 使用者常設指示
## E — 等使用者決定的事項
## F — 不變的規則、工具與環境事實

§B 必須使用欄位 sessionId | task | state | nextD | lastACK | unansweredQ。
每個 session 一列、不得留空；無待答事項填「無」，未確認填「未確認」。Q 編號綁定 source session id。
已發但待 ACK 的 D，須在 task / state 保留原 D 編號與來源 reference。不要重問 Q、重發 D、推定 A-Q / ACK 或擴大授權。
先讀取 ao orchestrator replacement intake %s --json 中的 queued 指示、完整附件 reference 與 pending worker reports，把未處理事項記入交接，不在此回合執行新工作。
這是唯讀盤點；queued / claimed 不代表 native 已送達、已讀或已執行，不要 claim / ack reports。
內容必須包含你目前已知的實際狀態；AO 的格式檢查不會代你證明語意。文件限制為 128 KiB UTF-8。

若既有 living files 尚未一次性納管，先依專案規則透過 ao project documents adopt 納管；不要直接 shell 改 shared root。
把 draft 寫到上述 scratch path，再透過 AO lease-aware 工具寫入 configured handoff。先讀目前 hash（不存在用 missing，adopt 後重新讀）；不要直接寫 living file。
讀取：ao project documents read %s --project %s --json
寫入：ao project documents write %s --project %s --file %s --expected-hash '<currentHash-or-missing>' --json
保留寫入回應的 hash 與 writeReceiptId。只有 touch、舊檔或一句「完成」無法通過驗證。
提交：ao orchestrator replacement prepared %s --attempt %s --nonce %s --path %s --hash '<write-hash>' --write-receipt-id '<writeReceiptId>' --json
提交成功後結束本回合；daemon 會另外確認成功 settle / 無 pending approval、凍結、匯出全文與安全 replacement。你不要自行替換 session。
`, quoteArgument(string(op.SourceSessionID)), quoteArgument(sourceNativeID(op.SourceOwner)), op.ID, quoteArgument(op.SharedDir), quoteArgument(op.DocumentPath), quoteArgument(draft), p.LeaseEpoch, sessions, metadata, quoteArgument(op.ID), quoteArgument(op.DocumentPath), quoteArgument(string(op.ProjectID)), quoteArgument(op.DocumentPath), quoteArgument(string(op.ProjectID)), quoteArgument(draft), quoteArgument(op.ID), quoteArgument(p.AttemptID), quoteArgument(p.Nonce), quoteArgument(op.DocumentPath))
}
