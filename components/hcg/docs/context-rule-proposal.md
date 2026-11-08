# HCG 上下文管理規則提案

本檔是 HCG-P0-PR3.02 的提案，不是已套用設定。依契約 rev9 §5 A3，具名替換 shared `rules/orchestrator-rules.md:70` 的上下文管理條（以該條上下文為準），並維持 `tools/orchestrator_context_size.py` 停用。worker 不修改 shared 正本、AO 專案設定或 live 設定；正式採用由主控交使用者核准。

## 建議條文

> 上下文管理：不用 compact；`tools/orchestrator_context_size.py` 維持停用。HCG P0 已部署且完成本主控來源取證後，執行 `hcg usage current --session "$AO_SESSION_ID" --json`。只有 `status=known` 且 `metric=last_observed_prompt_input_tokens` 時讀 `tokens`（最近一次已送出 request 的輸入，含快取、不含輸出）；同時記 `source_event_id` 與 `event_at`。`status=unknown`、指令失敗或 metric 不符，一律記 UNKNOWN，不自報估計、不當低於門檻、不改用舊工具、累計 token 或額度百分比；換手時機仍由使用者決定，既有已達門檻的警戒不因 UNKNOWN 解除。可信 `tokens` 達 400,000 時，在下一個不中斷 worker 的時點更新 §A–§F，告知使用者讀值與出處並請求換手；達 450,000 時不再開新工作線，只處理進行中的回報，優先完成換手。換手仍須使用者同意。P0 部署／來源尚未證實前維持 UNKNOWN 暫行做法。

## 判讀與取證限制

規則只消費 `metric/status/tokens/band/source_event_id`，並記錄 `event_at`；不讀累計、外部報表評分或 occupancy。門檻為 399,999 → below_400k、400,000 → handoff_400k、449,999 → handoff_400k、450,000 → urgent_450k；UNKNOWN 的 band 是 unknown。

本 PR 只有合成 fixture，Q2 真人版本／主線身份、單次 input、順序與完整性證據未取得。C1 沒有 sequence/finality/latest-pending；即使有完整 Claude 三 bucket 或 Codex last_token_usage，也不能只由 id/time 證明最近送出。因此目前 current 合法回 UNKNOWN，tokens/source_event_id/event_at/age 為 null；source_ref 只表示觀察到的主線來源 metadata，synced_at 是讀取時間，不是當前 request 新鮮度的證明。不為此新增 schema、原生 parser 或可自行聲稱已驗證的旗標。

本工具不觸發交接、compact、重啟或其他 lifecycle。未證來源的正面 current 留待主控另行具名取證／裁決，不以舊值、child、cumulative delta、歷史最大或中性 throughput 補值。
