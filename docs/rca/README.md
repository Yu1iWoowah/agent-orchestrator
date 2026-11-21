# Codex 已記錄內容粗估

RCA-DIR3-PR1 將 Codex worker 既有記錄的內容粗估與 AO canonical usage 並列。
它不重建 provider request；`requestComposition` 固定為 `UNKNOWN`。renderer 沿 typed client 在既有資源視窗與診斷頁提供已記錄內容面板。

## 選定與分析

僅接受 `agent-orchestrator` 專案、`worker`、`codex`。project 預設 off；設定的 `rca.enabled` 與 `worker.rca.enabled` 沿 HCG policy 的繼承方式解析，session override 與 reporting 分開保存。
先由使用者明示選定 AO session，再操作 CLI。以下 `<id>` 是 AO session ID，不接受 native path、native ID 或 config 位置。

```sh
ao usage composition set --session <id> --enabled true
ao usage composition get --session <id> --analyze --json
ao usage composition get --session <id> --json
```

`--analyze` 明示要求新的 snapshot；普通 get 只讀現有狀態與最新 canonical 分頁。同 session 同時只有一個 job。讀取固定為分析開始的實體檔與 EOF；後來追加內容須再明示分析。重讀不累加到 usage ledger。

| 操作 | 效果 |
| --- | --- |
| `set --session <id> --enabled false` | 明示 false，撤回 RCA job；不是恢復繼承 |
| `clear --session <id>` | 移除 session override，重新依 project／worker policy 判定 |
| `emergency-stop --enabled true` | host RCA stop，取消 RCA job，保留設定與既有安全數字 |
| `emergency-stop --enabled false` | 明示恢復，重驗儲存與 policy；不啟動 native agent |
| `purge --session <id> --yes` | 撤回 job 並刪除 RCA 數字／scope，保留 override、canonical 與 native 檔 |

父層設定、owner／launch generation、source 身分及 revision 在讀後、寫回前再驗。off→on 的 ABA 不讓舊工作回填。容量或寫入故障後不自動重試；明示控制操作須先重新驗證儲存。

## 畫面與控制

啟用「開發人員模式」與「診斷」後，在「記憶體」視窗或「診斷」頁面找到 session 列。
只有本機 `agent-orchestrator` 專案的 Codex worker 提供「已記錄內容」按鈕；其他專案、orchestrator、Claude 與 remote session 不提供此入口。
點選這個按鈕明示選定 session，只查現有數字，不自動讀取來源。展開的面板沿正式 typed API fetch，不讓 renderer 讀 native 檔案。

1. 「Session RCA 設定」可選「開啟」「關閉」或「繼承」。面板另列 project／worker 角色與最終生效狀態；繼承不是強制開啟。
2. 按「分析所選 session」才要求新 snapshot。執行期間輪詢狀態，不重複提交分析；未開啟或主機暫停時不能要求分析。
3. 「停止此 session 的 RCA」明示關閉並撤回分析；「刪除粗估」撤回工作並刪除數字，保留 policy、canonical 與 native。
4. 「暫停此主機的 RCA」影響此主機所有 RCA，按「恢復此主機的 RCA」才恢復；不會暫停 native session 或改寫正本用量。

控制操作開始就撤下舊畫面數字，完成後重查。session 切換與 off→on 的舊回應不得回填新畫面。
關閉面板停止 renderer 輪詢，不持久化數字 query cache；要撤回分析請使用停止／刪除控制。
傳輸或控制失敗只顯示固定錯誤訊息，不將回應原文送到 renderer 錯誤邊界或 telemetry。

介面文字提供英文與繁中；其他現有語系的新 RCA 字串暫沿用英文。

### 數字的閱讀方式

- **已記錄內容粗估**：固定六類各列 bytes 與各可見片段 `ceil(bytes/4)` 後加總的粗估，沒有 request 占比、計費總量或隱藏 overhead。
- **Session／turn**：可選來源 snapshot、session snapshot、未配對內容與記錄 turn ordinal；ordinal 不是 provider request，也不與 canonical event 猜配。
- **Coverage**：同時列分析 bytes、固定讀窗、records、來源版本與品質缺口；partial 不顯示成完整輸入。
- **正本用量**：原 canonical events 的獨立數字頁，不和粗估相加；本頁無 rows 是 UNKNOWN，不是用量 0。尚有後續頁時明示本頁不是 session 總量。
- **基線，非此請求**：獨立的已核基線。缺附件或不符 digest 時保持 UNKNOWN，不啟 provider 補做。

已知固定品質碼尚無本地化標籤時直接顯示代碼，未辨識字串保持 UNKNOWN。
缺欄位顯示 UNKNOWN，只有實際觀察的空值可以是 0；可見 thinking 為 0 也不能表示隱藏 thinking 為 0。
完整逐 request composition 始終 UNKNOWN。此畫面只完成 Codex 方向三的可見閉環；原 native OTel PR2、原 PR3 與 Claude PR4 的交付／停點不因畫面而變更。

## 來源與品質

優先使用已登錄 binding/source 的主來源，失敗不改走其他候選。只有沒有主來源時，才以可靠 native UUID 精確比對核准 root 內 rollout 檔名的完整 UUID 後綴；此步只讀檔名 metadata。實體 alias 去重；多候選、跨 session/binding 衝突、列舉不完整均 `unavailable`。安全 open 後先核 header ID、非 subagent、版本 `0.167.0`，才分類內容。

核准 final root 僅為 `$HOME/.ao/data/agent-accounts/history/codex/{sessions,archived_sessions}` 與 `$HOME/.codex/{sessions,archived_sessions}`。AO resolved roots 必須符合；symlink alias 以最終實體 root 判斷，不擴掃 home、account config 或 credentials。

| 類別 | 計入的可見片段 |
| --- | --- |
| `system_developer` | base instructions、system／developer 文字 |
| `tool_schema` | 已記錄的 dynamic／additional tool schema JSON 片段 |
| `conversation_history` | user／assistant 訊息、可見 compaction summary |
| `tool_result` | function／custom tool output |
| `retained_thinking` | 可見 reasoning summary／文字；encrypted thinking 不計 |
| `other_observed` | 已見的 tool arguments／custom input 等未歸前五類片段 |

方法固定 `utf8_bytes_div4_v1`，互斥片段各自 `ceil(UTF8 bytes/4)` 後加總。這不是 tokenizer、billed tokens 或模型實際 input；也不把未觀察內容放入 `other_observed`。無欄位是 `null`，明示空字串可為 0。只在明示 turn key 可證時給 AO ordinal；缺 key 歸 session `unpaired`，不以時間或 token 值猜 join。

串流最多讀每檔最後 **1024 MiB**，單行上限 **64 MiB encoded bytes**（含 newline），JSON 深度 **32**。不建立整窗 buffer。非零窗起點保守捨棄首筆未證完整的行，不向窗外補讀；缺 header／版本證據時整個來源 `unavailable`。這些限制不是 process RSS 硬界。

`window_head_gap`、`record_limit`、`parse_limit`、`incomplete_tail`、`invalid_record`、`unknown_schema`、`unknown_fields`、`missing_key`、`unsupported_content`、`compaction_gap`、`fork_context_unknown`、`hidden_thinking`、`source_truncated` 等固定 reason 明示缺口。`source_version_unknown`／`unsupported_source_version`、身分／generation／scope 錯誤不推定完整來源。`ready` 僅指核准記錄片段可讀，完整 request 仍 UNKNOWN。

## 查詢邊界與保存

新增 API：`GET/DELETE /api/v1/usage/sessions/{sessionId}/composition`、`PUT/DELETE /api/v1/sessions/{sessionId}/rca/override`、`PUT /api/v1/rca/emergency-stop`。get 使用既有 `afterId`／`throughId`／`limit` canonical 分頁，額外 `analyze=true`；其他 query key 拒絕。

canonical 每次透過既有 EventsReader 重讀，僅投影數字及既有 AO source/binding 引用，不建立第二 ledger。baseline 是 PR1 核收的獨立合成數字，不貼到任何 live request；證據 digest 不符時 UNKNOWN。typed renderer client 與 CLI 只見白名單投影，不見原文、native ID、path、工具名稱或來源 hash。

RCA 僅允許 loopback native caller（無 browser metadata）或設定中的 exact renderer Origin。LAN／tunnel 在實際 transport handler 封鎖，偽造 Host／Origin／bearer 不放行。RCA 的精確路由在正常、拒絕、off、5xx 與 storage failure 都不進逐請求 access/error log、raw error capture 或外部 diagnostics；只保留固定 status-class counters。background failure 也不輸出 error／panic 原文。

只在 AO data dir 的 `rca/state.json` 保存安全數字與 policy metadata，atomic staging 為 `state.next`。數字留 **7 天**；過期不提供，清理失敗停止 RCA 寫入。metadata、舊檔加 replacement staging 峰值均計入 **16 MiB/session、128 MiB/host（跨專案）**，因此可用的淨 snapshot 容量低於名目 ceiling。writer 序列化並行預留；無 WAL、原文 cache、retry spool 或新增 diagnostics 檔。

每次寫入先由實際目的目錄 FD 查 volume 的 `Bavail`，`T=max(2 GiB, ceil(totalBytes/100))`；扣掉完整預計寫入與 staging 峰值後必須仍 ≥T。到限、未知／非法空間、volume 身分改變、write／flush／replace 失敗只停 RCA，回固定 reason，不取消 native／canonical。這是寫前停止界，**沒有預配置池、實體 reserve R 或滿碟必要寫入保證**；其他程序可在檢查後消耗空間。

目前 writer 的 statfs 支援 Linux／darwin；其他平台保持 storage unavailable。交叉編譯不代表該平台已完成原生驗收。

## 交付層次

本功能沿方向三契約（`rca-dir3-total-review-1` v3；P8 從簡確認 `rca-dir3-p8-confirm-1`）實作。真實 session 驗收必須先取得使用者明示選定與授權；合成測試不能替代 DA1。

原 RCA-P1-PR2 native OTel 仍 blocked，原 PR3 由本工項 supersede，不追認 OTel 完成。Claude PR4 維持 UNKNOWN/off；沒有為本功能啟模型、provider、OTel 或讀 Claude 來源。[PR1 能力證據](capabilities.md) 保留原 gate FAIL 與限制。
