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

## PR2 的確認結果

- `20261103094403671`：fork 的 Plan overlay 走 `/plan` API，依 acceptance
  sequence 取最新版本，不受 `spansTurns`／`superseded` 旗標影響；旗標只影響
  timeline 上 Plan 副本的顯示。
- `20261113111818788`：只清除 provider switch 之後殘留的 reroute，runtime 的
  `CommitProviderSwitch` 也做同樣的清除；timeline 的 provider-switch／reroute
  紀錄保留。
- 兩支 migration 用到的欄位在依序套用時都已存在，最終 schema 也保有這些欄位。

## Rollback 與換版

Vsync Down 明確拒絕。這批 migration 包含 table rebuild 與資料變更；
schema 前進後，只裝回 u3 binary 不是安全 rollback。

換版前須另行授權，以一致備份完成可還原演練。
若 startup 失敗且尚未恢復工作，依該程序成對還原舊程式與停機資料備份。
若已寫入新工作，還原會遺失新增資料；必須交使用者決定前進修復或資料復原，
不能自動覆寫。PR1 不授權 stable 安裝、真實資料副本演練或 provider／lab 執行。

## PR2：固定 U 匯入

PR2 先在 `ao/upsync-design/upsync-integration` 分七個子 PR 審查，
最後以 merge commit 進 main。PR2 沒有新增或修改任何 migration，
仍是上方的 284 份。

| 基準 | SHA |
| --- | --- |
| F1（PR1 前的 fork main） | `b2c8f3bb44cc2407e52750501df3057dfc1781d4` |
| U（固定上游 main） | `53a545ad20fcd300c7c989f175f9a5c5d90fe58e` |
| merge-base B | `c8be3cbd452eccabdc7001536b917e0572cf5b89` |
| I0（PR1 #117 squash，integration 起點） | `6a438ca71fd8baef1966a163e7224b1d6df2ca27` |

| 子 PR | 合入方式 | PR head | integration commit | parents |
| --- | --- | --- | --- | --- |
| A #118 首次匯入 U | merge | `fb63aef99af5d930a08d8ab6d0f8cb2aee7da647` | `2dac2befac8696a7bb3f9cac42957475a32c6653`（tree `74dfd16e`） | I0、A head |
| B #119 runtime／帳號 | squash | `b0de24d9b689b8d28056a268426ed75c19840497` | `de64b11e0e5ff45b60889d89f478a54cc540037d` | `2dac2bef` |
| C #120 owner／交接／設定 | squash | `c0b9bc91546268c66e87db151e53c75a4c79c4fd` | `7762cccc038ccac04509ab3389241bc8da1aac5d` | `de64b11e` |
| D #122 UI／mobile | squash | `3baa808d3ba188bef8e6f468ca06f82a4ab133cb` | `17e7395c7ea12841e54fefe67d65f7617f15e15b` | `7762cccc` |
| E #123 fork 模組／Cloud | squash | `4544350f1daba7b4f81b5b477e27e0de97b78a9b` | `8ce61d632236e405790ab22f883c6fe90c789d05` | `17e7395c` |
| F #124 CI／wire／生成 | squash | `4ac94c643e3a51203559a95a0eb5005487ffcc47` | `51c33b0e438e8aa0f163e78f8bf4d6915c3c8501` | `8ce61d63` |
| G0 #125 main 合回 | merge | `5acbffca2270c782a75ebd931e503b59ad07c6f8` | `830bfe9c9c6643aba3a7ee1e713592c714e22dfe`（tree `9e6d69a6`） | `51c33b0e`、G0 head |
| G 整體驗證與本文件 | squash | 合入時記錄 | 合入時記錄 | `830bfe9c` |

G0 把 main 的 RH-REALDATA-FIX（#121，`da09b59d425f2f62fabe9d7c7eac060947659ae5`）
合回 integration，讓最終 PR2 進 main 時不需處理它。
最終 PR2 的 merge commit（FINAL_SHA）與兩個 parent 在合入後由主控記錄，
不寫回本文件。驗證 U 祖先：

```bash
git merge-base --is-ancestor 53a545ad20fcd300c7c989f175f9a5c5d90fe58e <integration-head>
git merge-base --is-ancestor 53a545ad20fcd300c7c989f175f9a5c5d90fe58e <main-after-PR2>
```

兩者都要 exit 0，且最終 main tree 等於核定的 integration tree。

A 的試合併有 249 個衝突路徑與 216 個雙方修改但自動合併的路徑
（含設計時的 245／172），逐列處置由 A 記錄，後續子 PR 的結案由 G 對照。
審查時另看 `U..head`（fork 保留差異）與 `F1..head`（本輪新增）兩個 diff。

### fork 保留差異

下列行為刻意與 U 不同，下次同步要保留：

- daemon LAN listener 封鎖 `/api/v1/agents/accounts` 整個 prefix，
  包括登入、登出、刪除與 PTY；device credential、method、偽造 Host 都不能繞過。
- 不導入 U 的 offline `ao triage` 直接啟動 agent，以及讀取 Conductor
  Application Support 資料庫的 session import；兩者違反 fork 的 hard rules。
- Chat 閒置 90 秒後 hibernate（U 是 10 分鐘）。
- 專案設定要明確按「儲存」，衝突時整筆拒絕；不採 U 的 650 ms autosave。
  PSS-PR5 退役的 project `agentAccounts` 鍵不復活。
- 保留 fork 的 delivery attempt／receipt／hold、replacement 單一 owner、
  managed handoff、原題回答路由、zh-TW、reply language、Plan overlay、
  managed documents 與 Archive。
- 保留 fork 的 `cloud/` 模組（使用者選 Q3-A）；`scripts/check-public-boundary.sh`
  改為核對 fork 所需 module 都在 checkout、public desktop／backend 不依賴上游
  私有控制平面。Cloud 分享只提供公開連結；私人連結、grants、multipart 上傳是
  上游私有控制平面的 API，fork Cloud 沒有，桌面端會隱藏或明確拒絕。
- remote host 改位址時要重新輸入密碼，不把已存的密碼送往新位址。
- Cloud 目錄把本機配對的 host 搬到新位址時，改用 Cloud account token，
  丟棄本機密碼或 device token，該 host 改由目錄管理；位址相同時不變。
- `login-again` 的重複判定沿用 U 的規則；新增登入的占用判定仍是 fork 的規則。
- GitHub Actions：`contributor-project.yml` 只服務上游 repo；
  `release-latest-guard.yml` 在 fork 是 `disabled_fork`，fork 不需要。
  `harness-release-feeds.yml` 已在 fork 啟用（使用者 `upsync-pr2f-feeds-1`＝enable，
  2026-11-18），PR 改到版本解析檔時自動跑；排程在 fork 由 job 條件跳過。

### 驗證缺口

- macOS 原生 job（CLI E2E、DMG layout、update helper）常因 GitHub runner
  容量取不到機器，需要重跑。
- 本機 Linux 沒有 zsh，`shellterm` 的 zsh 子測試只在 CI 執行。
- Windows ConPTY、macOS 簽章與 updater 只能由 CI 原生 job 證明。
- 真 Claude Code 的 isolated lab（2026-11-18）：Plan 會出現在 Chat；一個 session 串流時，
  另一個 session 的恢復不受阻塞；daemon 重啟後，persistent host 讓進行中的 turn
  對回原本的 turn 並完成，沒有重送。同一個 lab 看到兩個問題，都不在本輪修正：
  - idle hibernation 與 `ao stop` 都因 delivery 證據未結算（`shutdown_not_settled`）
    而被拒絕，provider 程序保留。這是 F1 既有的行為，根源與 backlog 214
    （Claude ACP 擋 `ao stop`）相同，併入 214，在 u4 安裝前處理。
  - session 設定變更回 `AGENT_SETTINGS_CAPABILITIES_UNAVAILABLE`。判定程式
    （`agentsettings`）是 F1 既有，F1→HEAD 沒有變動；目錄沒有標示預設模型是因為
    全新 HOME 沒有 Claude 的 model 設定。但明確指定模型也被拒的原因未定，是否與同步
    有關沒有判定：U 在 Claude 目錄指紋加了 salt（`modelcatalog/catalog.go:447`），
    這個可能也沒有排除。記 backlog（B 組）；u4 換版後要核對既有 session 的目錄快照，
    確認重新讀取目錄後設定能力會恢復。

### u4 換版界線

u4 是本輪換版代號。打包只能從最終 PR2 合入後核定的 FINAL_SHA 產生，
daemon、Electron、tmux、ACP／browser runtime 都要來自同一來源，並核對包內
revision、版本、檔案 SHA256 與 updater 目標；`AO_RELEASE_REPO` 限定為 fork，
不等於授權發布。安裝要另有具名計畫：凍結與未決工作清單、停止 stable、
一致備份、校驗包、安裝、核版本與 schema、恢復工作。u3 的「只換程式回滾」
不適用：PR1 引入的 table rebuild 與資料變更使 schema 無法倒退，
回滾要依上方「Rollback 與換版」成對還原程式與備份資料。

## UPSYNC2 A 段

整合分支 `ao/upsync2-design/upsync2-integration` 從 fork 的
`3beedae6eff6ea2299f2345a41a87b6fa7312658`（F）開始。A 的合併 commit 以 F 為第一親、
上游 `fc15acf55a68d530a4b71b5ee357ffd8a6ad36a8`（U）為第二親，涵蓋共同祖先
`53a545ad20fcd300c7c989f175f9a5c5d90fe58e` 之後 U 的 150 個 commit。
保留雙親能讓後續同步以真實的共同祖先計算差異。

### 保留與後續接點

| 下一段 | A 段保留的行為與路徑 |
|---|---|
| B | 284 份 F migration 與既有 goose、query、sqlc 和 SQLite store。Reviewer／report、帳號占用、側終端、新 harness、PR 歷史列表及 preview poller 仍走 F 的 schema 與 caller。18 份新 SQL 與其呼叫端在 B 一起啟用。 |
| C | 保留 `ao chat-host`、`ao mcp` 以及原 build、bundle 與啟動路徑。Provider switch 的直接 HTTP／CLI 入口與 force 留待 C 的 replacement 准入驗證。沿用 fork 的 turn receipt、恢復與停止語意，以及工作樹擁有權檢查。 |
| D | 保留 desktop 的 early supervisor、Cloud 公開分享、project 顯式儲存、remote 改位址及帳號身分規則。會觸及這些邊界的 IPC、上傳、mobile 和 renderer caller 跟隨各功能閉包。 |
| E | 保留 fork 文件、專案 Setup、`predev` 及既有版號。`dev:warm` 連同 fork 本機模組的快取輸入在 E 接入。 |

CLI 仍走 daemon HTTP，Cloud、HCG 與 `go.work` 保留 fork 原來源。LAN 保留整個
帳號 API prefix 的封鎖，以及原 loopback 與 LAN auth 邊界。Conductor 的 skill
和說明頁維持刪除，先前排除的 offline triage 仍維持原裁決。

### CI 與差異審查

新整合分支的 ready PR 使用全量 lane。同一 fork 的 ready PR 改到此 base 時，
由新的 `pull_request.edited` 事件執行 CI。Changes job 核對 merge ref、checkout SHA、
事件的 base 和 head 父節點，並印出 tree。原本的 retarget workflow 繼續處理
`main` 與前一輪的整合分支。

- 用 `git diff -M F A` 核對 A 的實際影響，用 `git diff -M U A` 找 fork 保留及後段處置。
- 用 `git ls-tree -r` 比對各路徑的 mode 和 blob。A 與 U 一致且和 F 有差異的路徑，
  由 Git 清單記錄一次。rename 與刪除沿 `git diff -M --name-status` 記錄。
- A 的實作報告附原始衝突裁決與差異清單。後續以 `git diff -M A <head>` 核對，
  追加變動的處置和證據。

### A 段第一層審查處置

| PR #134 thread | A 段處置 | 後段驗收 |
|---|---|---|
| `PRRT_kwDOVZZGR8652AK2` | 設定各語系與 CLI 文件恢復 F 的刪除語意。封存自動清理目前會刪除 artifact 檔。 | 由 UPSYNC2-PR2.08 承接。依 `upsync2-pr2-q1=a_include` 一起啟用本機封存保留、CLI／HTTP 參數、Artifacts 頁面與各語系文案。Cloud share 與遠端 artifact 留 D。 |
| `PRRT_kwDOVZZGR8652AK7` | 恢復 F 的 Kimi hooks 與 lifecycle。撤回 `1a5d2647` 已匯入的 callback、reducer 及對應測試。 | C 接入同一提交的 CLI scoped approval identity、真實 payload 的 CLI→HTTP 測試與 reducer。 |
| `PRRT_kwDOVZZGR8652ALA` | 把逐 harness 的驗證錯誤傳給共用 `describeHarness`。父元件測試實際觸發 Check again，驗 Health、Account 與成功重試。 | A 驗證。 |
| `PRRT_kwDOVZZGR8652ALF` | 恢復 F 的 minimap E2E，配對 A 現有 inspector 行為。 | D 將按 chat 寬度顯示的實作與 E2E 一起接入。 |

### A 段 audit 處置

| 發現 | A 段處置 | 驗收 |
|---|---|---|
| I1 | `ed765f56bb` 的 23 條路徑恢復 F，包括 TLS 表單、解析器、新增／編輯呼叫端與測試。 | D.02 一起接入位址推導 TLS、wire 與呼叫端驗收。 |
| I2 | 撤回 `4b5d3d3f80` 的九條手勢路徑。ChatMarkdown 已維持 F，ChatTimelineItems 的手勢 hunk 也已維持 F，保留 A 的時間格式匯出。 | D.01 一起接入 #8709 的 wheel ownership、通知、圖片、程式碼、表格與 timeline 呼叫端。 |
| I3 | 舊 retarget 由同一 head 選取實際執行 `changes` 的 run。 | 雙 run fixture 核對略過新 skipped run。新整合分支以 fresh merge ref 驗收。 |

### B 段裁決（UPSYNC2-PR2.06）

| 子工項 | 合併處置 | 驗證接點 |
|---|---|---|
| .01、.02、.05 | 加入 U 的 18 份原樣 SQL。沿用 fork 的分段 migration runner、rebuild 與已發布收據，再由具名欄和查詢重產 sqlc。 | fresh、u3、F1、u4／F 以 production Open 驗 schema、收據、資料與第二次開庫。 |
| .03 | UTC startup fence 與背景修復使用同一儲存邊界。保留 Guardian trigger 產生的 UTC 表示，以及有 active hold 的 conversation queue 順序。WITHOUT ROWID 表以主鍵分批續跑。 | 合成同 instant 帳號碰撞依較高 rowid 保留，記錄修復前後 effective owner 與來源。不同 automation 執行和不允許改寫的列保留並記 skip。AU1 實測政策，AU2 在首次寫入前重新做適用性檢查。 |
| .04、.07 | review run 狀態機、draft guard、terminal notice、report retry 與 DB trigger CDC 一起接入。保留 fork reviewer 帳號設定與既有 process ownership 判定。 | 兩個帳號使用查詢以 launch reservation 區分 queued，launch、cancel、cleanup 與既有未 exited reviewer 一起驗。 |
| .08 | 依 `upsync2-pr2-q1=a_include` 接入本機刪除預設保留、封存清理、HTTP／CLI 參數、Artifacts 頁面與文案。 | 註冊檔案與獨立檔案由原 artifact 目錄供唯讀檢視及明確刪除。Cloud share 與 remote artifact 的上游差異留 D。 |

#### B 段第一層審查處置（D-12）

| 發現 | 處置 | 回歸驗證 |
|---|---|---|
| P1 · .04／.07 | 帳號 reconcile 遇到 pending stop 時保留 reviewer handle 與 SQLite owner；reaper 以子程序停止確認結算。force-stop 未確認的重試不依 pane 是否存在判定。 | pane 已拆除但仍有 survivor 時，帳號查詢顯示 blocking owner，移除被拒；停止確認後兩個儲存查詢均解除占用。 |
| P2 · .04 | reviewer 派送只納入取得 launching 的 run。全部 CAS 落空時提早返回持久狀態，跳過 Spawn／Notify。 | SQLite CAS 暫停時並行取消，涵蓋新啟動與重用 reviewer。 |
| P2 · .04 | Mobile 讀取 daemon 的 `completed` 與 `failureReason`，保留舊 daemon 與 Cloud 的 `complete`／`delivered`。 | 手機 local API 的完成結果可送 worker，無 body 的失敗可顯示原因，並驗 Cloud 映射。 |
| P2 · .02／.04 | 依有效 claim token 固定 batch 的第一次 render；同一 id 在重啟後送相同文字。新增 fork additive SQL `20261120084919464_report_delivery_payloads.sql`。 | 真實 Chat 收下含失敗摘要的訊息後模擬 ack 中斷，新 coordinator 重試及結算，Chat 只有原本一則。 |

#### B 段 audit 處置（D-13）

| 發現 | 處置 | 驗證接點 |
|---|---|---|
| I1 · .08 | 保留刪除先讀 session 的 artifact 目錄。遇到非預設路徑時拒絕，保留 session、登錄及檔案。 | 預設目錄為空且原檔仍可見時，手動保留刪除與封存自動清理均保留原資料。 |
| I2 · .08 | 已移除 project 的未刪除 session 投影未採用，超出 .08 核准範圍。已刪除 session 的實際 kept 登錄仍可在無 project 群組開啟。 | 只讀 kept 登錄，移除 archived project 掃描、`project-removed` 表示及專用測例。 |
| M1 | 實作報告的證據索引改列實際壓縮檔名與各自的受測 SHA。 | 按 test 與 package 核對壓縮輸出的通過事件。 |
| M2 | 既有 Chat queued excerpt 時序已列 backlog。 | 保留全量原始結果。 |

#### B 段差異審查處置（D-14／D-15）

| 發現 | 處置 | 驗證接點 |
|---|---|---|
| D-14 P1 · .07 | B 的 `cancel_unconfirmed` 與 captured survivor 新增了跨重啟清理義務。依 D-19 收據補強。 | F 與 A 的相關路徑相同。B 的 boot reaper 只讀 active runs，現有帳號查詢以 review 當前 launch 與 handle 保持占用。 |
| D-14 P2 · .07 | 已領取而尚無 handle 的 pass 進入 cancelling。Trigger 於 Preflight 返回後和派送前重查，略過已結束或正在取消的工作。已進入 Spawn 的 pass 交回 handle 後可中斷，等待停止確認。 | SQLite barrier 分別驗 Preflight 前後與 in-flight Spawn，並保留 before-CAS 測例。 |
| D-15 R1-I1 · .02／.04 | 移除暫加的 report payload 表，恢復原定 302 份 migrations。新文字只取既有 report 內容與固定 session ID。Chat 重送沿用既有已接受訊息的 bytes。 | ACK 前中斷、改名、重啟後驗同一 key 和文字、只一則訊息與結算。測例涵蓋新舊 renderer。 |

#### B 段持久化停止收據（D-19）

| 依據 | 處置 | 驗證接點 |
|---|---|---|
| `upsync2-pr2-q2=a_sqlite`、`upsync2-pr2-contract-1=approve` · .07、AC5 | Force-stop 先為原 launch 寫 SQLite 收據、程序身分及帳號快照。未封口保留原根證據。開機和後續恢復只觀察。兩個 owner 查詢由收據維持阻擋，確認原範圍全退才以精確世代結算。 | 重開 SQLite、Engine 與 launcher 後，查詢及帳號移除仍擋。程序全退才放行。同 root、同 machine-id 雜湊但不同 boot ID 時保留占用，記 `boot_changed_origin_unverified`。 |
| 契約 rev2.1 §5、AC2、AU1／AU2 | 唯一新增 fork migration `20261120110913324_reviewer_stop_receipts.sql`。固定 F 284、U 18 與此版本，終態為 303。 | 四組 production Open／reopen 逐 ID 驗唯一套用、DDL／FK。撤回的 report 版本為未核准前態，migration／timestamp 寫入前停止。 |

#### B 段第二輪差異審查處置（D-20）

| 發現 | 處置 | 驗證接點 |
|---|---|---|
| P2 · .07 | 缺跨重啟比較範圍時，同一 daemon 在原 launch 與 handle／Chat 世代仍相符的情況下照常執行既有停止。收據及已知程序身分先入帳，取得原生停止確認後才結算。Chat detach 或 controller 缺席不算停止確認。 | 預設 OS hooks 實際停止測試程序。SQLite 重開、新 Engine／launcher 沒有可比較 scope 時維持兩個帳號 owner 查詢的阻擋。Windows／Darwin 驗交叉編譯。 |
