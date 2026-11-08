# 固定三步交接腳本（RH-SCRIPT1）

這是 Linux、`agent-orchestrator` 專案、Claude Code／Codex Chat 的合作交接工具。
依契約 v3.1、Q1 澄清及 D-2 初始 prompt 例外實作；測試只使用 fake CLI、fixture SQLite 與私有 Git repository。
真實 provider／lifecycle 的資格演練與首次使用須另外授權，本 PR 沒有執行真實交接。

## 操作界面

外部 start 入口只收 old 與明確 target：

```text
python3 tools/reusable-handoff/handoff.py start --old <old-id> \
  --harness <claude-code|codex> --account <account-id> \
  --model <exact-model-id> --effort <exact-effort> \
  --approval-mode bypass-permissions
python3 <private-run-root>/<run-id>/handoff.py status --run <canonical-UUID>
python3 <private-run-root>/<run-id>/handoff.py continue --run <same-UUID>
```

`start` 先完成必要前驗，再 detach 並立即回 run-id／status 指令。
`status` 只讀 stage、old/new、原效果、最後進展、等待剩餘時間、責任人及下一步。
`continue` 只讀已綁定的 run，再啟動同 run 的 detached runner；不重讀啟動槽位、不重置原 T0。
明確手動 continue 可在原等待到限後另記一次 attempt，舊起點／期限保存至 `wait_history`；正常輪詢與 report 不重設期限，也不重送原效果或 nonce。
同一 old 只能有一個未決 run；同 run 用檔案鎖保證只有一個 runner。
同 request 再 start 只回原 run，不再次 detach。

`_run`、`inspect`、`bootstrap`、`finalize` 是內部入口。
new 依初始 prompt 使用固定 `inspect` 命令分頁讀取私有 run 材料，再以自己的 capability 執行 `bootstrap` managed write。
`finalize` 必須等 runner 核過同 native 的唯一 nonce，才由 new 自己通知 worker。

## 固定位置與資格

以正常 `ao project documents info --project agent-orchestrator --json` 得到 `sharedDir=S`，
run root 固定為 `R=parent(S)/.private-handoff-runs`，位於 AO data root 下、shared 與 old worktree 外。
目錄 0700，檔案 0600；拒絕 symlink、不同內容覆寫及越界參照。
start 在 detach 前把已資格化的腳本原 bytes 保存在 `R/<run-id>/handoff.py`；runner、new helper 與人類 status 都使用這份固定副本。
old worktree 之後消失不影響入口。固定副本的 status 可直接讀本 run 狀態，不依賴 daemon 在線。

初始定位只使用 PATH 上已安裝 CLI 的正常 documents 讀取。
後續固定使用 `R/.qualified/ao`，以及 0600 的 `R/.qualified/qualification.json`：

```json
{
  "schema": "rh-cli-qualification/1",
  "cli_sha256": "<qualified CLI bytes SHA256>",
  "script_sha256": "<this handoff.py bytes SHA256>"
}
```

這份資格資料與副本由另行授權的資格演練準備；腳本不安裝、不替換、不重啟 stable AO。
資格版須包含本 PR 的 `spawn --approval-mode`，並與 daemon 既有 `approvalMode` wire 相容。
start 也會核 CLI help、指定帳號的 email／signedIn／problem／quota，以及該帳號 catalog 的精確 model／effort。
CLI／script hash 或 daemon 定位改變即停止，不改用別的入口或 binding。

## old 準備的唯一啟動槽位

old 在呼叫 start 前，自己核對原人類指令、structured answer、後續取消／改向，並原子發布：

```text
R/.start/<old-id>/request.json
R/.start/<old-id>/evidence/<necessary-original-response>.json
```

`request.json` 使用 `rh-start-request/1`，包含以下欄位：

| 欄位 | 內容 |
| --- | --- |
| `request_id` | 完整 canonical UUID；也就是 run-id |
| `project_id`、`old_id` | 本專案及 old 的精確 id |
| `prepared_by` | old `session_id` 與真 preparing `turn_id`，可仍 running |
| `target` | `harness/account/model/effort/approval_mode/mode`，與外部逐欄一致，mode 固定 chat |
| `authority` | `source_refs`、`old_verified_scope=three-step-handoff-this-project`、`t0_source_ref/t0_created_at/t0_precision` |
| `workers` | 各 worker 的保留 stop D、hold、原題 owner／引用、待決、放行權與保留 resume D |
| `sources` | 唯一 ref、kind、conversation／branch／message id、revision、ask ref、原回應相對路徑及 bytes／正文 hash |

來源 kind 僅 `human-message`、`structured-answer`、`old-control-record`；最後一種不是人類授權根。
原回應使用正常 old session transcript 的原 JSON bytes，訊息依 structural role／origin 核身分，不能用文字前綴冒充人類。
structured answer 保留 `ask_ref={id,version,answer_id}`；缺原版本則照實保留 null，不重答原題。
hash 只綁定原件，old 仍負責原指令的語意範圍及取消／改向核對；腳本不從自然語言產生新授權。

worker 欄位為 `worker_id/reserved_stop_d/stop_d_source_refs/hold_state/hold_source_refs/task_owner_id/task_refs/pending_question_refs/release`。
`release` 含 `mode/authority_refs/reserved_resume_d/resume_d_source_refs`。
D 是 `D-<正整數>` 或 null，不自行遞增。
`hold_state=held|none|unknown`；`release.mode=keep-held|resume-original`。
既有 hold、未知放行權都保持 held；resume-original 需原授權、原題、明確保留 D 與來源引用。
需要通知停點的 active worker 缺 stop D 時有界等待；靜止者不為取得 D 被喚醒。

start 拒絕缺檔、半份／重複鍵 JSON、schema／型別／引用錯、target 不符或 hash 改變。
接受後保留 request 原 bytes 為 `R/<run-id>/start.json`，必要來源沿原相對路徑保全。
state 分別保存起始 worker index hash 與全 safe 後 stop index hash，不混用。

## 三個階段與回條

1. **worker 停點**：核本專案未 terminated 的 worker 集合。active worker 收具名 stop D，收束操作、保全 ref／dirty／原題，原子寫 `workers/<id>/stop.json`（`rh-worker-stop/1`），填真 turn-id，completed 後 hold。
   靜止代記須正常 idle/exited、無 queued/running／未結工具，兩份相隔至少兩秒的 Git 內容快照相同，再核正常狀態；明標「未經worker自述」。quota／沒回本身不算 safe。
2. **old 文件與原文**：全 safe 後一次具名 2a 控制訊息，old 用自己的 managed write／expected-hash 更新 A–F，正文帶 `handoff_run_id`，原子寫 `old/phase-2a.json`（`rh-old-2a/1`）。runner 核原 managed JSON、正本 hash、lease、run-id、真 completed；ready 回覆須給精確 status 與「10分鐘未見new首報就只查原run」提示。
   之後保存 cap0 active-lineage CLI 投影，以及 query_only transaction 中唯一 old conversation 的全分支白名單原文、必要附件／依賴。cut 不可變，delta 以父 hash 相連。
3. **切換與續接**：再核停點與 old idle，正常 kill 一次且正面 terminated 後，唯一精確 `RH3[<run-id>]` token spawn。new 以自己的 managed capability 寫接手收據 `bootstrap/new/receipt.json`（`rh-new-receipt/1`），向使用者正式回報後 completed。
   runner 核 actual account／target／approval／native／lease、原件 hash 與工具目的地。再以同回應的 S1/S2 序號、last completed turn、idle 及一秒窗口，正常 exit/resume 各一次；resumeMode 必須 native，同 new/native/binding 不漂移。唯一 nonce 需真正 assistant／completed/providerTurnId 與 known settings。

回條詳細必填鍵及 producer 模板由具名控制 payload 提供；回報 note 不作機器資料來源。
worker 自述與代記都要核註冊 workspace 的實際 staged／unstaged／untracked 內容，不能只比較 status 字串。
註冊路徑由既有 session `branch` 與 project `path` 對 Git `worktree list --porcelain -z` 唯一匹配，並重核 symbolic HEAD；不依賴 session API 未提供的 workspacePath。
同一輪會對所有 live worker 收檔或送出原保留 D；請求由 intent 冪等，不等第一位 safe 才通知下一位。
正常 CLI `incomplete` 是 queued／running turn、streaming／未結工具的清單，交由各 predicate 有界等待；真正截斷仍停止。
無法保全必要敏感／不可讀原件時具名停止，不越權讀取或 force clean／commit。

## old Git 保全與正常回收（v3.1 §6）

2a 的 old 自己核對並原子發布 `old/preservation.json`（`rh-old-preservation/1`）：
`run_id/old_id/evidence_turn_id/original_ref/classification[]`。
每筆分類含 `path`、`disposition=copy|rebuildable|discardable` 及具體 `basis`；
可重建／可丟棄的目錄可用 `scope=directory` 涵蓋子樹，不能只憑檔名或 Git clean 推斷。
必要或唯一不可重建檔案逐一列 `copy={path,sha256}`，先保存於本 run 的 `old/saved/`，
按原相對關係存放；證據另記原 mode／相對路徑，副本不冒稱原目錄。

runner 列出整個 old worktree 的檔案／目錄範圍，閉合 untracked、ignored 與空目錄的分類，
核副本 hash，獨立保存 HEAD、status、index、staged／dirty binary patch、必要 linked-worktree metadata。
`original_ref` 必須是回收範圍外 common repository 的既存 `refs/heads/...` 並保護 HEAD；
assume-unchanged、skip-worktree 或其他未核清 index flags 在 kill 前停止，不清旗標或重設內容。
未推 commit 仍受此規則保護。腳本不建立 ref；無保護 ref 的 detached HEAD 停止。
submodule、巢狀 repo 或未知 per-worktree metadata 視為依賴未核清，切前停止，不提供通用備份。

run、shared、old artifact、common repository 與材料副本不得和回收範圍重疊。
正常 project metadata 必須正面可讀；目前只接受適用 `preRemove` 為空的情況。
非空 hook 的副作用無法從任意命令文字證明，clean／dirty 一律切前停止，交人工核清。
2a ready 的保留說明只稱「切前證據／預期處置」，不預告 kill 後已保留。
manifest 引用不可變 `old/preservation-evidence.json`；delta 及發出 kill 前重核無漂移。

正常 kill 原 intent、stdout、stderr、結果與 terminated 觀測分別保存。
clean old 原 worktree 目錄／註冊允許正常回收，當下仍存在也不稱 preserved；
dirty 則須原 kill 回覆 preserved，並再核原路徑、註冊、ref、Git 狀態與必要內容相同。
兩者都須再核原 ref、shared 正本、cut／delta 原文、材料副本 hash；陽性成立才 spawn。
cleanup pending、遺失回條或效果 UNKNOWN 即停止；continue 也不能僅憑 terminated 越過閘門，
只讀原效果，不重送 kill、不 restore、不第二 spawn，不保證日後 cleanup 永久豁免。
本例外只限本 run 的 old orchestrator；worker 的原物保全規則不變。

## 寫入範圍、未知效果與回報

**D-2 的初始 prompt 窄例外**：正常產品的 `QueueChatPrompt` 會把 spawn 初始 prompt 記為 `origin=human`，sender 為空。
只有建立 session 時的 bootstrap prompt 適用這個例外，不能視為新的人類授權。
prompt 只提供 run 位置與指示，授權只能從已綁定的 `start.json` 原 authority／T0 引用取得，不能在 prompt 內帶入或產生新授權。
runner 以唯一 spawn intent 的完整 prompt bytes／hash 核對該則原訊息，保存 message id 並標記 `grants_authority=false`；不把其他 human 訊息套用這個例外。
後續 send／steer 控制訊息仍固定 old 傳輸身分，保持非 human。

理由是交接指示必須在建立 session 時就放進 prompt，不能先建空 session 再補送；old 已 terminated，不能依賴其活 turn 補發初始交接。
`OriginHuman` 是既有產品行為，本工項不改 daemon／API，也不改 v3 的序列。

runner 清除其他 AO 控制變數、capability／settings grant 及 provider 憑證環境，固定 old 為 CLI 傳輸歸屬。
每個 payload 帶 detached-runner、run-id、phase、recipient 與原 authority；不冒充人類或仍存活的 old agent。
new 的 managed write 與最終 worker 通知使用 new 自己的身分。

bootstrap 每個 shell 先設 `umask 077` 與本 run 私有 draft `TMPDIR`。
只接受固定 inspect／bootstrap helper、可完整拆解的 shell wrapper，以及已辨識的唯讀工具。
其他 writer／opaque tool／真正截斷或不明目的地停止為 `UNVERIFIABLE_WRITE`。
Codex 的 `outputMayBePartial` 只描述聚合 stdout；固定 helper 的完整檔案原回應另行核 hash，不宣稱聚合輸出完整。
這只證正常 AO transcript 可見活動，不提供 OS 隔離或任意同 UID writer 的保證。

所有 mutation 先保存完整 intent／argv／payload／hash，fsync 原件與目錄，再呼叫正常 CLI。
guard 未通過時只有 intent／not-dispatched；sent 在 CLI deadline 檢查後、subprocess 前記錄。若再次檢查證明尚未啟動就過窗，保存未送出證據並撤銷該 sent marker，仍受原等待上限約束。合法人工 continue 可續此原 intent，不能把未送出誤報 UNKNOWN。
UNKNOWN send 只 recover 原 handle；kill 只查原效果，遺失原回條時即使 terminated 也不得 spawn。
spawn UNKNOWN 只認領同專案／kind、完整精確 token、createdAt 不早於 intent 的唯一原 new，再核 binding；零筆／多筆都交人工。
resume 不明或非 native 不 fallback，不第二 spawn、不 restore old、不用 AO 原生交接。
report 有正面 structural batch／中斷／時間證據時，每個 phase 最多送一次 continuation，只補未完回報，不重寫已成功 managed、不再發 nonce；第二次中斷交人工。
新 human 訊息不套用初始 prompt 例外；原切換核驗之後有新 human 輸入時，保留來源並停止自動切換／通知，交 operator recovery，不把文字推定為新授權。

本 run 停點後至合法放行／收尾 FYI 前，worker 只用 `ao report --needs-input` 短通知，safe 後不送 checkpoint/done，不用 stuck。
FYI 不帶 D、不解除既有 hold；確有原授權才由 new 用原保留 D 續原工作。
通知記 accepted／未讀／未執行，不等全員 ACK；不重送已 accepted 的效果。

manifest 固定揭露：尚未送達的 report outbox 不在 conversation cut/delta 範圍，接手後才由 AO 既有機制送達；不查 reports 表，不手動搬移／ack outbox。
DB 不讀 conversations 表；以正常 CLI 的 conversationId／latestSequence 綁 cut，核白名單 messages／activities 的 conversation_id／序號，再用 CLI 重核 cut 未漂移。

## 時間與故障等待

只保存原 human T0、來源精度、各 stage／等待起訖與 Tfinish；UNKNOWN 具名，不用 launcher 時間補造。
沒有 300 秒或任何總耗時 PASS／FAIL，歷史評價不改寫。
每個等待點保留最初單調時計起點與 boot-id，report 不重置期限；boot 改變時不猜 elapsed。

| 等待點 | 上限（秒） |
| --- | ---: |
| 唯讀 CLI／metadata | 30 |
| kill／exit／單筆通知 | 60 |
| spawn／resume | 180 |
| worker 停點 | 1,800 |
| old 2a／匯出 | 各 600 |
| new native 可觀測 | 120 |
| new bootstrap／每次安全 idle | 各 900 |
| nonce／最終回報 | 各 600 |

到限保存部分證據、停止新動作並 needs-input，不 interrupt 正在工作的 actor。
runner 死亡無法自行通知，依 old ready 提示由人查原 run status；不另建監督。

## 本機驗證

在私有 TMPDIR、清除 AO／provider 環境後執行：

```text
python3 -I -B -m unittest discover -s tools/reusable-handoff/tests -v
cd backend
go test -p 8 ./internal/cli -run '^TestSpawnApprovalMode$' -count=1
```

測試對照：TS-T01 停點／靜止／quota；T02 dirty 內容與集合；T03 白名單／分支／附件依賴／outbox；T04 四組 binding／UNKNOWN／approval／固定身分／start 重入；T05 三回條、工具寫入、S1/S2、首報與 nonce；T06 UNKNOWN／原 handle／report 續行／精確 token；T07 D／hold／原題／放行；T08 原時戳／獨立期限／無總時限。
這些是可重現 fake 證據，不是 provider 性能數字或真實交接資格證明。
