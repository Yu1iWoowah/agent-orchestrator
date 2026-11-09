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
| `prepared_by` | 同一 old 的語意準備 A 輪；A completed 且全部引用訊息 settled |
| `target` | `harness/account/model/effort/approval_mode/mode`，與外部逐欄一致，mode 固定 chat |
| `authority` | `source_refs`、`old_verified_scope=three-step-handoff-this-project`、`t0_source_ref/t0_created_at/t0_precision` |
| `workers` | 各 worker 的保留 stop D、hold、原題 owner／引用、待決、放行權與保留 resume D |
| `workspace_index` | old 與待處理 worker 各一筆 `session_id/kind/path/source_refs`；絕對實際路徑，不用 branch 推算 |
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
workspace_index 隨 start 原 bytes／start_sha256 綁定；同 session 不重複、不同 session 不共用路徑，缺索引立即停止，continue 不讀另一份可變索引。
old 自身與非標準 worker 路徑必須引用 Q1 的 old-control-record，原文具名記 session／path 並屬 old preparing turn；保存副本及 live 訊息都核此 turn，runner 不從 A–F 猜路徑。

### 同一 old 的兩輪準備

A 核原 authority／target／T0、worker／D／hold／原題及實際 workspace，留下本輪具名 old-control-record，正常 completed。
A 不發布最終 request、不 start、不先改 A–F 或執行 worker 停點。
既有 operator 用正常公開 transcript 核 A 真 completed、所有引用 assistant 訊息 streaming=false、工具已結束，才發一次非 human B 續行通知。
idle 單獨、failed／recovered／interrupted 都不算 A 完成；不能讓 A 前景等待自己 completed。

同一 old 在 B 自己讀 A 定稿原件，重核 conversation／branch／message／role／turn／正文、原授權版本與後續取消／改向，以及 A 準備的全部事實仍適用。
有 worker、D／hold／原題或路徑漂移、來源不足即停，不以 B 的新決定補 A，不自動重做 A。
B 保存正常 CLI 回應原 bytes，從中取實際 revision、正文 hash 及 raw hash；不合成原件、不硬填 revision、不正規化正文。
B 原子發布新 UUID request，prepared_by.turn_id=A，來源不引用 B；同 B 輪呼叫既有 start，回真 run-id／status 後結束。
原 human／T0／精度不變，不請人類重送授權；任何 STOP 不改包救過、不重試或擅自 continue。
後續仍完整走 worker、2a、匯出／保全與 native 核驗，A completed 不等於 old 可 kill。

### 作廢與資格界線

start 拒絕固定 R/.voided/<uuid> 已存在的 UUID，包括未完成封存；hidden archive 不是另一個啟動入口。
作廢是具名核准的 operator 操作，不提供通用清理器；不能證明無控制效果、有 runner 或 UNKNOWN 的 run 不可自行退役。
已建但未 launch 的部分 run 不能當成「從未建 run」直接覆寫；同 request 再 start 可能只回 cached state。

RH-FIX1 核准的 R1 個案採完整封存後新 UUID，不續用舊 run；實際執行仍須另有具名 D。
operator 先依同序取得 old start 鎖與該 run runner 鎖，排除並行啟動，不 force 解鎖。
核固定版本、state／來源與全部 wire，正面證明無 runner、control intent／sent／result、managed 或 lifecycle 效果；未知檔案或缺證即停。
鎖內保存完整 manifest，整份 run 原子 rename 到同 filesystem 的 R/.voided/<uuid>，原槽整槽封存在其 retired-start-slot/，逐檔核 hash／mode／相對關係並 fsync 父目錄。
另原子保存具名 void-receipt.json、決定來源、前後路徑及 manifest／原資格 hash；不改歷史 state 冒充 completed，不刪失敗原件。
此只限具名零控制效果個案，不是所有 prepared/pending run 都可作廢；中途失敗保留已有效果並停，不自動重搬或新 start。

merge 後另經明確授權，保全原 package／資格／CLI／script bytes 與 hash，確認無 active run／runner後，鎖內原子發布完整 qualification 三欄。
記新 merge／script／CLI hash、wire 測試證據與新 package manifest；binary 未變仍核 hash/help/wire，不只手換 script hash 字串。
不得原地換已綁 run 的副本再 continue；封存的舊 script 只作證據，禁止再由 archive 執行。
A 只在來源與全部準備事實仍適用時沿用；B 必須重做，由 old 自行核、發新 UUID，原 human／T0 不變。
本節不是任何 lab／production 的操作授權，shared 與歷史文件不由此改寫。

## 三個階段與回條

1. **worker 停點**：核本專案未 terminated 的 worker 集合。active worker 收具名 stop D，收束操作、保全 ref／dirty／原題，填語意 statement 後由固定 helper 原子寫 `workers/<id>/stop.json`（`rh-worker-stop/2`），填真 turn-id，completed 後 hold。
   靜止代記須正常 idle/exited、無 queued/running／未結工具，兩份相隔至少兩秒的 Git 內容快照相同，再核正常狀態；明標「未經worker自述」。quota／沒回本身不算 safe。
2. **old 文件與原文**：全 safe 後一次具名 2a 控制訊息，old 用自己的 managed write／expected-hash 更新 A–F，正文帶 `handoff_run_id`，原子寫 `old/phase-2a.json`（`rh-old-2a/1`）。runner 核原 managed JSON、正本 hash、lease、run-id、真 completed；ready 回覆須給精確 status 與「10分鐘未見new首報就只查原run」提示。
   之後保存 cap0 active-lineage CLI 投影，以及 query_only transaction 中唯一 old conversation 的全分支白名單原文、必要附件／依賴。cut 不可變，delta 以父 hash 相連。
3. **切換與續接**：再核停點與 old idle，正常 kill 一次且正面 terminated 後，唯一精確 `RH3[<run-id>]` token spawn。new 以自己的 managed capability 寫接手收據 `bootstrap/new/receipt.json`（`rh-new-receipt/1`），向使用者正式回報後 completed。
   runner 核 actual account／target／approval／native／lease、原件 hash 與工具目的地。再以同回應的 S1/S2 序號、last completed turn、idle 及一秒窗口，正常 exit/resume 各一次；resumeMode 必須 native，同 new/native/binding 不漂移。唯一 nonce 需真正 assistant／completed/providerTurnId 與 known settings。

回條詳細必填鍵及 producer 模板由具名控制 payload 提供；回報 note 不作機器資料來源。
worker 自述與代記都要核註冊 workspace 的實際 staged／unstaged／untracked 內容，不能只比較 status 字串。
註冊路徑只取本 run workspace_index，以 `git --no-optional-locks worktree list --porcelain -z` 依 path 精確唯一匹配，再核 top-level／common repository；不依賴 API 未提供的 workspacePath，也不退回 branch 匹配。
CLI 的非空 AO_DATA_DIR 優先、否則 home/.ao/data；相對值在啟動時絕對化並固定。managedRoot 固定 D/worktrees，以 project 的 sharedDir 設定與 documents info 核 D/projects/<project>/orchestrator/<shared設定>，不向上倒推資料根。
標準 worker 須對上 managedRoot/<project>/<session-id>；非標準路徑只有正面 preparing-turn 來源才接受，且仍須在本 project 管理範圍。不是同 repo 任一路徑即可。
session.branch／實際 branch／HEAD 只作證據；同路徑 root、sibling、detached 都能定位，detached branch 記 null。保存原註冊清單與 path／Git metadata 身分；安全停點後 branch／HEAD／內容／註冊漂移仍停止。零筆、多筆、prunable、缺失、矛盾或別名均停止。
同一輪會對所有 live worker 收檔或送出原保留 D；請求由 intent 冪等，不等第一位 safe 才通知下一位。
正常 CLI `incomplete` 是 queued／running turn、streaming／未結工具的清單，交由各 predicate 有界等待；真正截斷仍停止。
無法保全必要敏感／不可讀原件時具名停止，不越權讀取或 force clean／commit。

## old Git 保全與正常回收（v3.1 §6）

2a 的 old 自己核對並填 `old/draft/statement.json`（`rh-old-statement/1`）；
固定 old-receipt helper 產 `old/preservation.json`（`rh-old-preservation/2`）及 phase-2a 回條。
每筆分類含 `path`、`disposition=copy|rebuildable|discardable` 及具體 `basis`；
可重建／可丟棄的目錄可用 `scope=directory` 涵蓋子樹，不能只憑檔名或 Git clean 推斷。
必要或唯一不可重建檔案逐一列 disposition=copy；helper 決定 `copy={path,sha256}` 並先保存於本 run 的 `old/saved/`，
按原相對關係存放；證據另記原 mode／相對路徑，副本不冒稱原目錄。

runner 列出整個 old worktree 的檔案／目錄範圍，閉合 untracked、ignored 與空目錄的分類，
核副本 hash，獨立保存 HEAD、status、index、staged／dirty binary patch、必要 linked-worktree metadata。
`original_ref` 必須是回收範圍外 common repository 的既存 `refs/heads/...` 並保護 HEAD；
assume-unchanged、skip-worktree 或其他未核清 index flags 在 kill 前停止，不清旗標或重設內容。
未推 commit 仍受此規則保護。腳本不建立 ref；無保護 ref 的 detached HEAD 停止。
gitlink 採下節的有限規則；巢狀 repo 或未知 per-worktree metadata 仍切前停止，不提供通用備份。

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
已保存成功 native resume 回條但 stage 尚未完成時，continue 讀原回條並重核同 new／native／binding，不再要求已恢復的 actor 仍 exited，也不重發 exit／resume。
report 有正面 structural batch／中斷／時間證據時，每個 phase 最多送一次 continuation，只補未完回報，不重寫已成功 managed、不再發 nonce；第二次中斷交人工。
新 human 訊息不套用初始 prompt 例外；原切換核驗之後有新 human 輸入時，保留來源並停止自動切換／通知，交 operator recovery，不把文字推定為新授權。

本 run 停點後至合法放行／收尾 FYI 前，worker 只用 `ao report --needs-input` 短通知，safe 後不送 checkpoint/done，不用 stuck。
FYI 不帶 D、不解除既有 hold；確有原授權才由 new 用原保留 D 續原工作。
通知記 accepted／未讀／未執行，不等全員 ACK；不重送已 accepted 的效果。

manifest 固定揭露：尚未送達的 report outbox 不在 conversation cut/delta 範圍，接手後才由 AO 既有機制送達；不查 reports 表，不手動搬移／ack outbox。
DB 不讀 conversations 表；以正常 CLI 的 conversationId／latestSequence 綁 cut，核白名單 messages／activities 的 conversation_id／序號，再用 CLI 重核 cut 未漂移。
plan settle 可使 latestSequence 高於原文列最大序號；只要求列序號不超 cut，且 CLI 最新可見 entry 在匯出列中，不把合法序號空隙當缺文。

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

Linux 需要 Go 與 python3。在私有 HOME／TMPDIR、清除 AO／provider 環境後，唯一 focused 入口：

```text
cd backend
go test -count=1 -v -run '^TestReusableHandoff(Wire|Producer)$' ./internal/cli ./internal/adapters/chatdriver/codexappserver ./internal/adapters/chatdriver/acp
go test -race -count=1 -p 8 ./...
```

TestReusableHandoffWire 用真 Cobra stdout 與現行 Go DTO 產 wire fixture 並驗 CLI 邊界。
兩個 TestReusableHandoffProducer 各執行完整 Python suite：真 helper 子程序的同次 argv／stdout／exit 先經現有 Codex normalizeItem 或 ACP toolEvent，再由測試專用 projector 的真 Cobra session transcript 格式化，最後交 Python consumer。
projector 只處理合成 transport 與時間／身份，不替換 producer 的 output／content／內容衍生旗標；Go test 不遞迴呼叫 go test。
CLI list 為 data/meta、row.role/activity 字串；detail 為 session.kind/activity 物件；不能互換或猜 workspace 欄位，路徑仍由 C3 workspace_index＋Git核。
Python 只替換 subprocess／HTTP I/O；無 producer fixture 或缺 python3 明確失敗，不退回手組封套。activity golden 在 tests/fixtures/activities，由兩個 driver 每次重算逐 bytes 比對；只有明確設定 RH_UPDATE_ACTIVITIES=1 才可更新，CI 不自動寫檔。
控制時序 fake 只改合成情境值，模板取同次 producer。-count=1 避免 Go test cache 忽略 Python 變更；-v 顯示 Python 確實執行。
Go workflow 的 PR paths 包含 tools/reusable-handoff/**，僅 Python／README 變更也會選中既有 Go 測試，無新增 workflow。
非 Linux 不執行此 Linux 交接腳本，但仍編譯原生受影響 Go 套件；不由 fixture 通過推論 provider 或實際交接資格。

測試對照：TS-T01 停點／靜止／quota；T02 dirty 內容與集合；T03 白名單／分支／附件依賴／outbox；T04 四組 binding／UNKNOWN／approval／固定身分／start 重入；T05 三回條、工具寫入、S1/S2、首報與 nonce；T06 UNKNOWN／原 handle／report 續行／精確 token；T07 D／hold／原題／放行；T08 原時戳／獨立期限／無總時限。
這些是可重現 fake 證據，不是 provider 性能數字或真實交接資格證明。


## 固定 actor helper 與 statement（RH-FIX2）

控制 payload 的 `command` 是唯一命令來源。固定形式如下；實際使用 payload 的精確 U／run-id／sid，不自行拼其他 root：

```text
umask 077; export TMPDIR=U/workers/<sid>/draft; python3 -I -B U/handoff.py worker-receipt --run <uuid>
umask 077; export TMPDIR=U/old/draft; python3 -I -B U/handoff.py old-receipt --run <uuid>
umask 077; export TMPDIR=U/bootstrap/new/draft; python3 -I -B U/handoff.py inspect --run <uuid> --path start.json --offset 0
```

允許的 shell 包裝只有規格化的單一 sh／bash／zsh `-c`／`-lc`。附加 writer、背景命令、任意 Python、inspect 的展開／glob／額外 operator 都停止。
目錄 0700、檔案 0600；不要自行寫 stop、snapshot、preservation、phase-2a 或完成時刻。

worker 填固定 `workers/<sid>/draft/statement.json`，schema=`rh-worker-statement/1`：
`run_id/worker_id/requested_state/block_reason/preservation_sources/in_flight/pending_question_refs`。
必要原件每筆填 `path/kind/basis`，來源只限已綁 workspace 或自己 AO artifact 的普通非敏感檔案。
helper 將材料固定存到 `workers/<sid>/saved/<workspace|artifact>/<relative>`，保留原物，記 mode／hash／來源相對路徑與 provenance。
在途操作填 `reference/activity_id/state/receipt_path`，只能引用同一次原操作的 terminal 證據；不能把手填 completed 當終態。
stop／D／hold／原題 owner／放行權沿用 start，缺 D 不猜，缺放行權保持 hold。

worker helper 保存原 CLI 回應與 exclusion proof，只排除本次唯一 running turn、本 turn streaming assistant、精確自身 helper activity。
其他 queued／running turn、同 turn 其他工具、核准、無法映射的 incomplete、真正外部 job 都阻擋 safe。
runner 仍等真 completed 且沒有必要 incomplete；idle 沒有自身 helper 例外。
worker 自述及 script-quiescent 都用同一 `rh-workspace-snapshot/2`，保存相同 regular/index/patch bytes 與 gitlink facts。
缺證據時 blocked／pending 可有 null snapshot 並具體說明原因；所有提供的引用仍須在該 worker 固定目錄內且 hash 正確。
正常流程拒絕 stop/1，不以 blocked 繞過路徑檢查。

old 先自己以正常 managed write／expected-hash 更新 A–F，原 `--json` bytes 保存為 `old/managed.json`。
old statement 的 `classification[].path` 只以 old workspace 為基準，必為乾淨相對路徑；填 disposition／basis，必要檔逐檔 copy，可重建／可丟棄目錄才用 scope=directory。
不接受 actor 自選 copy 目的地、絕對路徑、漏分類／重疊、缺保護 ref 或 lease 不符。
old-receipt 核當下真 turn／managed 原回應／正文／lease／worker index，先固定必要副本與 preservation/2，最後才發布 phase-2a 作信號。
無最後信號只 WAIT；有信號但引用缺失即 STOP。成功重呼叫僅相同 intent／輸入／原件 hash 可讀原結果，部分發布或 managed 效果不明不自動補寫。

普通 helper／inspect JSON 的 lease.session_id、prepared_by.session_id 或讀檔文字不是背景工作。
同一共用解碼器先核 AO status／turn／incomplete，再核精確 helper 與原檔，最後才從其他工具結果外層的明確 async envelope／傳輸 running handle 取 reference。
outputMayBePartial 本身不代表 job；必要結果截斷、未知 handle、錯終態 reference 仍 WAIT／STOP。
測試含真 bootstrap stdout 超過 64×1024 的反例，producer 真截斷旗標原封不動進 consumer，不能拿短 golden 替換。

## gitlink 的有限保全

真 index mode 160000、stage 0／OID 是資料來源。缺目錄或空目錄明記 uninitialized，不假稱子 repo 物件已保存。
clean initialized worker 必須 HEAD=OID、無 dirty／untracked／ignored／nested、無 shallow／alternates／grafts、無隱藏 index flags，原工作目錄保留。
old 還要求 child common 位於 parent common 內、在 old workspace 與會回收的 per-worktree metadata 外，同 filesystem，且既存 branch ref 保護 child HEAD 與完整物件。
不新增 ref；切前漂移、切後 common/ref/object hash 不符均 STOP。非空假目錄、symlink、stage 衝突與不明狀態均不會降成 uninitialized。

## 已派送 run 的具名窄退役

`retire --run 33da56c1-a652-4d16-b1aa-1042d3322f3f` 只實作此具名歷史案例，不提供任意 run／root／force。
merge 不授權執行；須使用者裁決及主控新 D。operator 在固定 `R/.retirement/<run>/authorization.json` 記 schema=rh-retirement-authorization/1、operator_session_id、decision_ref、directive_ref、old_id、start_sha256、精確 expected_effects 與必要 materials（worker_id/path/sha256/basis）。
這是營運授權引用，不是簽章，腳本不產生人類授權。

守門要求 workers/stopped、只有兩筆已知 D-3 accepted 且原傳遞／completed／blocked／held 可核，old 仍負責、兩 worker 無後來改向、原 runner 的 PID/SID/boot/啟動記錄正面可核為已結束。
取得 old/start 鎖後才取 runner.lock，不 force、不發訊息或 lifecycle。slot 必須仍是原 start bytes，原材料可保全，任何 UNKNOWN 停。
歷史 stop/1 只保全原 bytes 與 blocked／hold 事實，不跟讀其越界引用、不升版 safe。

固定 intent／manifest／原資格及 CLI bytes、必要材料與 precondition 原件先落盤；整 run 原子 rename 到 `R/.retired/<run>/`，整槽移到其 retired-start-slot/，建立空唯一槽位。
原 state 與 accepted／FAIL 歷史不改成 complete 或零效果。最後 `R/.retirement/<run>/receipt.json` 綁原件 hash／mode／layout、兩個 closed effects、責任及授權引用。
pending intent／半途 archive／缺 receipt 都阻擋同 old 的新 start；退役 UUID 永遠不能 start／load_run／continue／_run。
中斷不搬回、不刪歷史、不重播；只在新的具名人工恢復授權（固定 recovery-authorization.json，原 intent_sha256、operator_session_id、新 directive_ref）下核原件並完成原提交。
RH-FIX1 的 .voided/60cd2080 零效果作廢是不同規則，不能套到已有兩筆 D-3 的 run。

## 已提交範本的 workspace 窄例外（RH-FIX3）

敏感檔名 pattern 保持：.env、其點號後綴、credentials/auth/token/private-key 等名稱預設停止。
只有最末 component 恰為 .env.example、父路徑沒有敏感 component，且同一 HEAD 在精確路徑有普通 blob，才可能放行。
index 必須是相同 mode/OID 的唯一 stage0，沒有隱藏 flags；工作檔的執行權及 raw bytes 必須與該 HEAD blob 相同。
clean/smudge filter、Git status clean、只是 tracked 或只有 git add 都不是證明。
.env.sample/.env.template、已 commit 的其他敏感名稱、未追蹤或 ignored 敏感檔、dirty/staged/deleted/renamed/conflicted 範本都停止。

worker snapshot、old Git 保全及兩個固定 helper 都先核全部名稱（含 HEAD/index 的刪除路徑與 ignored），再保存內容。
範本 objects 使用當次已比對的 raw bytes；old 以原 ref/object 保護，內容 hash 納入切前／切後漂移核對。
兩種 patch 只含本次盤點的非敏感精確路徑，禁跨路徑 rename；空路徑集合產空 patch。
過程中 HEAD/index/名稱/範本 bytes 或檔案觀測漂移，不發布有效證據；old 不能以 discardable/rebuildable 分類跳過此閘門。

此例外不適用 source_inputs、worker 必要材料、old 必要 copy 或 preserve_dependencies 的明列 seed；.env.example 在這些入口仍拒絕。
不要把已有 snapshot/ref 保護的範本另加成必要 copy 或匯出依賴。
這不是 secret scanner，不能證明已 commit 範本或普通檔名中沒有秘密；範本信任沿現有原碼審查／資格。
程式只在記憶體比較值，錯誤只列路徑與原因，不把內容／diff 輸出到 agent 或報告。

## ff137a22 的第二個具名退役案例

同一 retire 入口額外只接受 ff137a22-e159-4603-ad70-1abd41681e5d，不提供任意 UUID/stage/force 政策。
此 run 已 launch，但在 workers/stopped，completed_stages/effects 空、無 control 或 new_id，只有部分 before-snapshot。
它不是未 launch 的 .voided/60cd2080，也不是有兩筆 D-3 派送的 .retired/33da56c1；不得改 state 偽裝成任一舊案例。

authorization 沿 rh-retirement-authorization/1，另綁 project_id、target、script_sha256、cli_sha256、qualification_sha256，expected_effects 必須空。
先核舊 script／CLI／資格、slot/start/source 原 bytes、只讀 wire、原 runner PID/SID/boot/launch、排他鎖及 actor 真終態／hold。
已閉合的 33da56c1 receipt 及原 stop/final 歷史保持：D-3 received、dispatch turn completed、原 ACK、blocked/held 分開保存。
不創造本 run 的 stop 回條，不重播 D-3/D-4，不發訊息讓 actor 變 idle。
任一 control/UNKNOWN/producer/後續 phase、活 runner、slot 換包或未知檔案都停止。

共用原 manifest/intent/receipt 提交：整 run 及 slot 原子封存到 .retired/ff137a22，保留舊資格及所有部分輸出，最後才發布 receipt。
closed_effects=[] 只描述 ff137a22，不能抹掉 33da56c1 的兩效果；兩份 record 同時核驗，任一 pending 阻擋同 old 新 start，兩個退役 UUID 永不能執行。
中斷仍需新的具名 recovery-authorization，不刪原件／搬回／改 outcome；merge 不等於實際退役授權。

營運順序須另具名 D：保全原件 → 用新程式和仍未覆寫的舊 qualified bytes 退役 ff137a22 → 完整 receipt → 原子發布新 script 資格 → 新 A completed/settled → 新 B → 新 UUID/start 一次。
不在 ff137a22 原地換碼／改 hash／continue；舊 A″/B 保留歷史，新 A 必須反映新資格／退役與當前事實。
D-4若仍合法唯一且未派送，可由新 A 重新具名保留，不自動 D+1。
原 human a0b12708…/revision0 與 T0=2026-11-08T19:39:02Z/second 只在原授權仍適用且無取消／增限時沿用，不請使用者重送來重設 T0。
R1 任一 STOP 不 retry、不擅自 continue；R1 成功後仍在 R2 前停等真人確認。本 PR 不操作 lab，也不實作 B-RH-A-FACTCHECK。

## merge 後資格與 R1／R2 邊界

先由主控另發具名 D，保存舊資格／FAIL 原件並以新 script bytes、實際 CLI bytes 重做資格，不替換 stable。
已收 D-3 的事實使舊 A 不再足夠：R1 需同一 old 的新 A completed／settled，重核 worker 現況、原授權與新保留 D，再由合法非 human B 通知發布新 UUID request 並 start。
不自行遞增 D、不重播舊 handle、不拿舊 run 的 blocked 當新 safe。
原 human a0b12708-fba2-4a7e-9024-bf4c8c8cee2e／revision 0／原正文及 T0=2026-11-08T19:39:02Z（second）只在原授權仍涵蓋 R1、沒有取消改向或新限制時沿用；新 script／UUID／A/B/D 不重設 T0。
R1 成功後交 07 與 O2 真人清單並停等使用者另一次 O2 確認，不能自動進 R2。
本 RH-FIX2 實作 PR 不操作既有 lab、dev 3002、三 actor，不實際退役、更新資格或重跑 R1。

審查補強：固定 producer 沿原 clientMessageId 同時核對 active steer 的 system activity/detail 與 idle fallback 的 user message，sender、payload、turn 必須一致。退役遇到較晚 durable steer 一律視為新 directive／authority UNKNOWN。退役前 CLI 原始回應先存入私有 preconditions，每次核對獨立保存，再由 manifest／intent／完成回條綁 hash；恢復不覆寫舊觀測。initialized gitlink 使用 Git 的 git-path 核對 merge／rebase／cherry-pick／revert／bisect／sequencer metadata；存在進行中狀態即 blocked，metadata facts 也參與漂移核對。

message 有 consumedByTurnId 時以它核對實際 dispatch turn，原 turnId／message ID／clientMessageId 保留。退役在 manifest 已發布、intent 尚未發布時，可核對原 preparation 授權／效果綁定、run／槽位／副本／preconditions 後沿同一 manifest 續建 intent；不刪改原件，任何漂移仍 STOP。
