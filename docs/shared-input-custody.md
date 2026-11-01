# Shared input custody（S0）

S0 提供獨立的 `ports.InputCustodyStore`，保留 queued／held／unknown input 的 custody、原始位置及 hold。既有 v1 intake runtime 不變；目前沒有 lifecycle 呼叫這些新方法。PR1 的 handoff owner coordinator 與 settings/delivery coordinator 後續接入，並負責 admission、provider/host evidence、人工處置與 outbound barrier。這個 schema 不啟用新的 provider 路徑。

## 資料與介面

| 物件 | 責任 |
| --- | --- |
| `conversation_turns.state = 'held'` | 離開舊版 queued／running／failed 派送與 Retry 集合；保留原 turn ID、payload 關聯及 provider evidence。 |
| `conversation_input_custody` | 原 state、conversation、branch、session、controller generation、intake sequence、requested time；目前 custodian 與 CAS revision。 |
| `conversation_input_holds` | 每個 hold key 的 ledger owner、reason、version 與 active fact；custody 移交不改寫這些欄位。 |
| `conversation_input_actions` | 不可變、具名且冪等的 register／hold／release／transfer／delete receipt。不是 provider delivery ledger。 |
| `orchestrator_replacement_held_turns` | 保留全部 v1 分支，UNION shared active holds；synthetic `shared:` operation ID 不會成為 v1 active operation。 |

`RegisterInputCustody` 只登記 provenance，不改派送 state。`HoldConversationInput` 在同一交易記錄 hold 並轉成 held。`TransferInputCustody` 只移交保管責任，保留 input、reason、hold version、provider evidence 與順序。`ConversationInputCustody` 以同一 read snapshot 列出 mixed inputs 與 holds，順序沿用 `requested_at, rowid`。

`ReleaseConversationInput` 要求 custodian/revision 與 hold 的 ledger owner/version；最後一層 hold 解除後才採用呼叫者已核准的 target state。API 不自行判斷 unknown 是否可重送，也不授予 linked resend 或延後權限。`DeleteConversationInput` 是獨立的 custody-qualified retention action，一般 cleanup 或 session cascade 不能冒充它。

同一 action key／相同 request 只回傳原 revision，不重播；同 key 不同 request、錯 owner 或過期版本拒絕。`InputCustodyStorageCompatibility` 檢查 guards、held view、FK 與 held/hold 一致性；它不代表 provider 或 host qualification。

## Migration 與依賴清冊

唯一 DDL：`backend/internal/storage/sqlite/migrations/20261101152248341_shared_input_custody.sql`。其他 owner 消費此 schema，不再重建 turn CHECK／shared hold／view。

`migrateUpTo` 在原有啟動資料鎖與單一 writer 邊界下先完成舊 migrations，僅 S0 重建期間暫停 FK enforcement。Goose 在原生 transaction 內完成 DDL、FK 校驗與 migration version；失敗全回滾，最後恢復原 pragma。migration 自身拒絕以 FK enforcement 仍開啟的模式直接重建，避免 cascade 丟失歷史關聯。提交前沒有 writer 能看到移除 guard 的中間態。

重建保留 turn 全部欄位、rowid、PK、五個具名索引與全部 FK。FK 包含 sessions/conversations/review、自身 retry/promotion、messages/activities 的 SET NULL，以及 branches 的 fork/replaced/replacement 三個關聯。

| 依賴 | 保留清冊 |
| --- | --- |
| 18 replacement triggers | `replacement_orchestrator_insert_fence`, `replacement_successor_bind`, `replacement_successor_delete_fence`, `replacement_orchestrator_restore_fence`, `replacement_turn_dispatch_fence`, `replacement_intake_insert`, `replacement_intake_update`, `replacement_intake_delete`, `replacement_intake_turn_settled`, `replacement_report_intake_insert`, `replacement_report_intake_update`, `replacement_report_intake_delete`, `replacement_repair_snapshot_no_update`, `replacement_repair_snapshot_no_delete`, `replacement_notice_identity`, `replacement_notice_no_delete`, `replacement_notice_history_no_update`, `replacement_notice_history_no_delete`。 |
| 2 views | `orchestrator_replacement_released_sessions` 保留 activated-native-writer 最後定義；`orchestrator_replacement_held_turns` 保留 v1 分支並加入 shared hold。 |
| 其他 turn triggers | `attention_turn_changed`, `conversation_turn_settings_snapshot_cdc`, `conversation_turns_auto_resume_cdc_update`, `conversation_turns_branch_insert`, `conversation_turns_cdc_update`。另兩個 turn triggers 已計入上述 18 個。 |
| 5 turn indexes | `conversation_turns_auto_resume`, `idx_conversation_turns_branch`, `idx_conversation_turns_conversation`, `idx_conversation_turns_provider`, `idx_conversation_turns_retry_source`。 |
| 新增 guards | `conversation_input_*`：immutable action、custody/hold CAS、held turn state/identity/promotion/delete、message/activity 變更與 replacement INSERT 防繞過。 |
| 新增 CDC | `conversation_input_action_cdc` 經既有 DB trigger／change_log 路徑通知，不手動另發 CDC。 |
| sqlc | `queries/input_custody.sql` 與產生的 `gen/input_custody.sql.go`／`gen/models.go`；SQLite catalog／PRAGMA gate 留在 storage 邊界。 |

`input_custody_migration_test.go` 比對重建前後全部既有 sqlite_master 定義；只允許 held CHECK 與 view UNION 差異。故障案例包含 DDL 中段及 Goose ledger 提交，驗證完整 schema、FK、input 順序與 version 一起回復。

## Backout 與 Down

binary backout 保留新 schema，不執行 Down；全部 shared holds、原 reason/version、provider evidence 與 action receipts 保留。舊 binary 沒有新 release 介面，不能解除新 hold。支援的精確 binary／host 及實測限制由 PR 驗證矩陣列出，未測版本不宣稱支援。

Down 僅允許從未使用 shared schema 的 DB：沒有 custody、hold、action history 或 held turn。即使明確刪除 input，critical action history 仍阻止 Down。不得靠刪 receipt 或改 DB 移除 guard；可讀 metadata 不等於 provider/host 控制權已通過資格。

依 A-Q1，A31 queued/held custody transfer 在 storage 介面與測試驗證；正式 handoff routing／owner CAS 接入由 PR1，delivery release 政策由 settings/delivery coordinator 負責，後續各自重跑混版整合門檻。

## 重跑 Linux binary／host 探針

先唯讀核對舊套件 hash，解包到私有 scratch，不安裝。對每個待測 daemon binary 執行：

```bash
python3 test/shared-input-custody-compat.py \
  --binary /path/to/extracted/resources/daemon/ao \
  --scratch-parent "$HOME/.ao/scratch"
```

腳本只建立全新 HOME、DB、linked worktree 與 loopback daemon，使用 `test/fixtures/input-custody-codex.py`，不讀既有 AO／provider 資料。預期 read=200、兩筆 held Retry=409、普通送訊=202、exit-agent=200；`thread/resume` 保持原 thread，既有 queued 與新普通 input 都進 provider，held I/O=0，停止後原 input／hold／action facts 不變。每個結果列 binary SHA-256，最後停止自己的 daemon 並清除 fixture；`--keep` 僅供診斷。

探針執行待測 binary 的真實 daemon／chat-host，外部 Codex provider 使用離線替身。這是 shared-schema 相容驗證，不替代正式 provider 的 freshness、權限、handoff lifecycle 或真內容演練，也不授權啟用新路徑。
