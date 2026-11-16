# UPSYNC schema 邊界

UPSYNC-PR1 在 F1 `b2c8f3bb44cc2407e52750501df3057dfc1781d4`（PSS-PR5 後）
匯入固定 U `53a545ad20fcd300c7c989f175f9a5c5d90fe58e` 的 41 份獨有 migration。
F1 的 242 份與匯入 SQL 全部保留原 ID、原 bytes；歷史聯集 283 份，
加上 `20261116001259486_upstream_sync_schema.sql`（Vsync）共 284 份。

PR1 保留 F1 queries 與 runtime／API／UI 行為。`npm run sqlc` 從聯集 SQL
重產型別；新欄位使部分查詢回傳獨立 row 型別時，store 保留原欄位投影。
U query API 與完整 caller 整合屬於 PR2。

## Startup 與最終 schema

production `Open` 完成 migration 後才回傳 connection pools。
migration 仍在 daemon data-directory lock 內，由 `gooseMu` 與單一 startup
writer 序列執行。

| 邊界 | 處理 |
| --- | --- |
| U `20261031173316629` 長 context 重新計價 | 保存既有 execution rows 的四項成本與 `pricing_version`，執行原 U SQL 後恢復；保存、SQL、恢復與 receipt 同一交易提交。native usage 仍由原 SQL 清空舊價格。 |
| F `20261101152248341` turn 重建 | 保全既有 U `failure_code` 非空值與索引，在原 FK 邊界執行原 SQL，再恢復擴充欄位。 |
| F `20261109083558284` usage 重建 | 先保全既有 U tier／message ID 與三個索引，執行原 SQL，再恢復擴充欄位。 |
| U `20261107181854835`、F `20261113162757663` | 僅在版本 pending 時核對兩個同名索引；以交易移除已知定義，再執行整份原 migration。 |
| Vsync | 核對既有欄位與索引，在同一交易完成所有缺少／新增效果與新 receipt。未知定義拒絕 startup，不記 Vsync 已套用。 |

重建保全使用 `_upstream_sync_<table>` 與對應 `_indexes` 表。
這些是 startup journal，恢復交易 commit 後才移除。
原重建尚未執行，或其 SQL **與 Goose receipt 均已 commit** 但尚未恢復欄位時，
下一次 startup 會使用 durable copy，不會以重建後的空值覆蓋它。
原生重建交易失敗會 rollback，並恢復原 FK 設定。

既有 `NO TRANSACTION` migration 仍可能在自身 SQL `COMMIT` 與 Goose 寫入
receipt 之間遇到程序退出。PR1 不偽造這種歷史 receipt，也不承諾採納任意
upstream／nightly 的部分 schema。若該時點留下無法重跑的 pending 重建，
startup 拒絕並保留 copy，須依核准復原程序處理。
這個限制與上表已驗證可恢復的分段點分開記錄。

| 物件 | 最終定義 |
| --- | --- |
| `conversation_turns.failure_code` | `TEXT NOT NULL DEFAULT ''` |
| `idx_conversation_turns_failed_recent` | `(completed_at) WHERE state='failed'` |
| `model_usage_events.service_tier` | nullable `TEXT`；非 NULL 時 trim 後非空、長度 ≤64 |
| `model_usage_events.provider_message_id` | nullable `TEXT`；非 NULL 時 trim 後非空、長度 ≤256 |
| `idx_model_usage_events_provider_message` | `UNIQUE (provider_id,provider_message_id) WHERE provider_message_id IS NOT NULL` |
| `idx_model_usage_events_priced_version` | `(pricing_version,id) WHERE estimated_cost_nanos IS NOT NULL` |
| `idx_model_usage_events_model` | `(model_id,id)` |
| `idx_conversation_{messages,activities}_turn` | F covering 索引：`(conversation_id,turn_id,sequence)` |
| `idx_conversation_{messages,activities}_turn_delete` | U delete 索引：`(turn_id) WHERE turn_id IS NOT NULL` |

Vsync SQL 同時是 sqlc schema 來源，包含 SQL parser 重播原重建後所需的三個
`ADD COLUMN`。production runner 只在驗明 type、nullability、default、CHECK
宣告後採納既有欄位，不忽略 duplicate-column 錯誤。
比對與剝除 `ALTER` 前，runner 先將記憶體內的 Vsync SQL 正規化成 LF；
CRLF checkout 不影響採納，repository 中的 SQL bytes 不變。
索引比對保留 SQL 字串常值的大小寫與空白。

U 的 open usage-kind migration 已取代舊 0117 enum shape。
startup 核對其已套用 receipt，避免舊 Codex-profile 修復每次 Open 都刪除並重寫
0117。PSS-PR5 的 project `agentAccounts` preflight 與資料 migration 保留。

## 驗證範圍

`TestUpstreamSyncInstallsAndUpgrades` 排除 41 個 U-only 版本，重建已發布的
fork 歷史，再以 production `Open` 驗證 fresh 聯集排序、u3／F0 原 241 份 SQL
再補 U／PSS，以及 F1 原 242 份 SQL 再補 U。

測試比較 table、index、trigger、view 的完整 SQL；只正規化 `app_settings`、
`conversation_turns`、`model_usage_events` 的欄位順序。
CHECK／FK／索引 predicate 仍在比較範圍，實際欄位順序記在 log。
每個 embedded version 都須有且只有一筆 applied receipt、FK check 零違反；
第二次 `Open` 的 schema 與完整 ledger 必須不變。

非空 fixture 保全 held input／immutable actions、Plan heads／versions、
disabled 且 in-use 的 account、歷史 `source=project` session／reviewer binding、
未決 delivery／receipts、side compaction obligation，以及 HCG execution
provenance／usage cost facts。三個新 U 欄位在合法部分升級點填值，
不假稱 u3 原本已有這些欄位。

u3／F1 的 execution fixture 使用 300,000 input tokens、非 NULL
`billing_provider_id` 與四項成本，覆蓋 U 長 context UPDATE 的條件。
另驗證成本恢復／receipt 寫入失敗皆 rollback、重啟可重試、NULL／zero
成本保留，以及尚無 execution 欄位時與既有 fork 的 native usage 重新計價。

其餘案例涵蓋兩個 index 分段、兩個 rebuild copy 執行前／commit 後的重啟、
欄位已存在／缺少時的 Vsync receipt failure、未知欄位／索引拒絕、原生交易
失敗的 FK 恢復及不安全 Down 拒絕。既有單一 migration 往返測試停在自己的
目標版本，避免跨過 Vsync 才測舊版 Down。
managed settings、TUI settings 與 project attention 的原 facts／CDC 斷言
也在 `migrate(db)` 到最高版本後驗證；Down 段仍只從各自目標版本執行。

F1 queries 使用 repository pinned `npm run sqlc`。
實作證據另以 sqlc v1.31.1 在 scratch 生成固定 U 原文 `queries/usage.sql`，
並對 production 建立的資料庫執行 generated insert、repricing read、
provider message 去重與 tier repair；PR1 沒有匯入 U query API。

## PR2 待確認

- 整合 fork Plan overlay 時，確認 `20261103094403671` 補入的
  `spansTurns`／`superseded` 旗標如何影響舊 Plan 顯示與已接受版本。
- 整合 composer 時，確認 `20261113111818788` 清除過期
  `model_reroute_json` 後的顯示，並保留 timeline 的 provider-switch／reroute 證據。

兩項都保留 U 的資料修復語意；PR1 僅記錄接點，不修改這些 migration 或 UI。

## Rollback 與換版

Vsync Down 明確拒絕。這批 migration 包含 table rebuild 與資料變更；
schema 前進後，只裝回 u3 binary 不是安全 rollback。

換版前須另行授權，以一致備份完成可還原演練。
若 startup 失敗且尚未恢復工作，依該程序成對還原舊程式與停機資料備份。
若已寫入新工作，還原會遺失新增資料；必須交使用者決定前進修復或資料復原，
不能自動覆寫。PR1 不授權 stable 安裝、真實資料副本演練或 provider／lab 執行。
