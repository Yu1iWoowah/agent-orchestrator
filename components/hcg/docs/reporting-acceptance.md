# 固定外部報表：操作與驗證

I-PR1 的入口是 `ao usage`，經 primary loopback HTTP → AO service → `components/hcg/reporting` → `internal/integration`。沒有新增 HCG executable、listener 或 dashboard。canonical usage 不寫入、不補缺，外部結果每次整份替換。

## 部署與操作

部署者另行安裝 `llm-usage-metrics@0.14.3` 與 Node 24，並核對套件與依賴來源。AO 不會自動安裝、下載、改 PATH 或找全域 engine。明示登錄時解析部署環境的 Node 絕對路徑，驗版本後記錄 Node 與 package/dependency tree 的 digest；報表不再以 PATH 找 runtime。版本或檔案改變時回 unavailable。

Linux 需要可用的 `/usr/bin/bwrap` 與 namespace 能力。Windows／darwin 可編譯，尚無等價 OS 沙盒時報表 unavailable；編譯通過不等於原生執行通過。

```sh
ao usage integration register --engine llm-usage-metrics --executable /absolute/installation/node_modules/.bin/llm-usage --version 0.14.3 --json
ao usage integration get --session SESSION --json
ao usage integration set --session SESSION --enabled true --json
ao usage report --session SESSION --engine llm-usage-metrics --json
ao usage integration set --session SESSION --enabled false --json
ao usage integration clear --session SESSION --json
ao usage integration emergency-stop --enabled true --json
ao usage integration emergency-stop --enabled false --json
```

專案與 worker／orchestrator 角色沿既有完整 project config replacement，新增 `hcgReporting.enabled`。null／未填是 inherit，false 是明示關閉。project → role → session，host-stop 最高；解除 stop 不改寫其餘政策。reviewer／side 不隱式掛載。所有新 HCG 路徑都受 native loopback 與實際 LAN listener 阻擋，不能用偽造 Host 繞過。

manifest 將 launch 的 `mountInputs`／`mountPolicy`／`mountEngine`／`mountDigest` 與當前解析的 inputs／effective／digest 分開記錄，避免 reconnect 將新預設當成過去掛載。當前有效快照變動時原子保存並增加 revision。manifest 位於明示 AO data root 的 `hcg/reporting/<namespace>/manifest.json`，含 AO identity、設定來源／digest、工具登錄、requested／active 與 launch/native 對應。host-stop／engine 在同一 root 的固定 host 檔案。缺失 manifest 的被動查詢／報表不以當前預設猜過去；明示 set／clear 可從 AO 權威身份建立新 manifest。未知版本／損毀檔案拒絕覆寫。同 native live reconnect 保留掛載快照；新 launch 重新解析已保存 session override 與專案／角色設定。報表當次仍核 current policy、host-stop 與 native ownership，失敗不動原 session。

啟動通知依 I-PR1 Q3 裁決 (a)：Chat 只在 durable owner／ControllerReady 完成且 controller 已發布後通知；TUI 只在最後 spawn／restore 成功後通知。通知只含固定 typed metadata，立即以記憶體快照凍結 policy／engine，再由可取消的私有 worker 核對 AO committed owner 並寫 manifest；不在 session 鎖內等 I/O、不執行工具。晚到與重複通知不覆寫新 owner。review／side 在 producer 與 consumer 兩側排除。沒有修改 DB trigger、schema、migration 或 CDC consumer，也沒有借用 grant 介面。

## 輸出限制

只讀 AO 權威 binding/source 已明示的當前主線 native 檔案。每個檔案以 root-confined descriptor 複製到 job 私有視圖，不掃全 history、不讀工具私有 DB。引擎只取得唯讀安裝／runtime／來源視圖、私有可寫 HOME／cache；乾淨 env、`LLM_USAGE_SKIP_UPDATE_CHECK=1`、events 的 `--pricing-offline`、events/tools 的 `--no-sync --no-history` 與 OS 網路隔離一起使用。

回傳 `source=external_analysis`、AO project/session/native 身份、版本、觀測時間、安全數值與有限工具分類。原文、repoRoot、工具名稱／參數、命令與工具提供的 request key 不穿透。沒有逐請求 join、repair 或 canonical/external 合計。引擎回傳非零、schema/identity 不符、來源越界、timeout／取消時不回局部成功；CLI 仍輸出安全狀態 JSON，再 exit 1。

固定版本合成 Claude：I=100、R=30、W=40、O=20，含 cache input=170、引擎 total=190。Codex 原生 input=100、cached=30、output=20、reasoning=5 時，引擎 I=70、R=30、W=0，含 cache input=100；reasoning 不再加到 output。原生缺 bucket 時引擎仍可能輸出 0，故明示 `bucketAvailability=engine_normalized_native_availability_unknown`，不能據此把 canonical null 補成 0。

report 的整體執行上限為 30 秒；caller 取消、session override 改變、engine 替換、host-stop 會取消自有報表 context。Linux PID namespace 收束自己的 child tree；不 kill 主 session、daemon 或其他程序。外部 I/O 不持 policy 鎖，取消後拒過期成功。

## 可重跑驗證

測試只用合成 fixtures。一般 suite 不安裝外部工具；完整引擎證據須另明示既有隔離安裝，不能把 skip 當通過。依 worker 環境規則清除 AO／harness／provider 憑證、設私有 HOME／TMPDIR 後，在 repo root 執行：

```sh
export HCG_ENGINE_TESTS=1
export HCG_TEST_NODE=/absolute/node24/bin/node
export HCG_TEST_ENGINE=/absolute/isolated/node_modules/.bin/llm-usage
GOWORK=off go -C components/hcg test -race -count=1 ./...
GOWORK=off go -C backend test -race ./internal/cli -run '^TestUsageReportingSameCaller$'
```

| 主張 | 測例／成立條件 |
| --- | --- |
| false／inherit、stop 與重啟 | `TestPolicy`、`TestConsumerPolicyPersistence`；兩種 parent 及明示 false 分開核對。 |
| reconnect／新 launch／未知版本／data root | `TestManifestRestore`；不得以新預設倒寫 live 掛載，未知狀態不建立。 |
| 固定引擎只讀授權來源與 cache 向量 | `TestFixedEngineAuthorizedView`、`TestFixedEngineCodexMissingBuckets`；相鄰未授權檔未讀、cache-write 非零及缺 bucket 都有實跑。 |
| 憑證／網路／唯讀／子程序邊界 | `TestSandboxBoundary`；連不到實際主機 listener、掛載拒寫、取消後延遲 child 不可寫 marker。 |
| 非零／schema／timeout／stop | `TestConsumerReportFailures`、`TestStopCancelsOnlyOwnedReports`；不回成功或局部數字，不改 policy。 |
| 同 caller 開關與數值核對 | `TestUsageReportingSameCaller`；真 CLI → HTTP → service → 固定引擎，與同一合成輸入的唯讀 ledger fixture 並列，不宣稱 live／逐筆對帳。 |
| 同 caller 故障閉環 | `TestUsageReportingSameCallerFailures`；同 CLI→HTTP→service 注入先輸出合法數值後非零、壞 schema、正式 30 秒 timeout，以及引擎在途時 host-stop；安全 unavailable JSON／exit1、空數值陣列，原 session/ledger/project/role/session policy 不變（stop 解除後回原設定）。 |
| admission、Mount 與設定順序 | `TestReportAdmissionRejectsPolicyChangeBeforeSourcesReturn` 驗 source 查詢暫停時 off／off→on 都零引擎啟動；`TestOverridePreservesExistingLaunchMount` 驗原四個 Mount 欄位不受 set/clear 改寫；`TestWorkspaceRegistrationFeedsFreshLaunchPolicy`、`TestPolicyNotificationsCannotReorderCommittedSettings` 驗 workspace 與三種正常 writer 的 durable policy 到 fresh launch。 |
| 前處理取消 | `TestPreprocessingCopyStopsReadingAfterCancellation` 控制第一塊寫入後取消，後續讀取為零；`TestReportPreprocessingPreservesCancellationReason`、Linux `TestCancelledReportDoesNotOpenEngineFiles` 驗取消原因與 engine IN_OPEN 為零。安裝 metadata/tree/Node 與 source staging 共用逐塊 context 檢查，不聲稱可中斷 kernel 已在進行的單次 regular-file syscall。 |
| Chat 成功邊界與故障隔離 | `TestReportingLaunchAfterChatPublication`、`TestReportingLaunchChatFailureAndReviewerExcluded`、`TestReportingLaunchPanicDoesNotUndoChatSuccess`；只有 durable owner 與 controller 發布後通知，fresh/live 分開，原生失敗不通知，報表 panic 不影響成功。 |
| TUI 成功邊界 | `TestReportingLaunchTUIMarksFinalSpawnAndRestore`、`TestReportingLaunchTUIPromptFailureAfterMarkSpawned`、`TestReportingLaunchTUIPanicDoesNotUndoSuccess`；最後讀回及 prompt 傳送後才通知，MarkSpawned 本身不是成功證據。 |
| metadata 快照、晚到與 owner 排除 | `TestCapturedLaunchFreezesPolicyAndIsIdempotent`、`TestCapturedLaunchFailsClosedWithoutBlocking`、`TestLaunchReadyFreshLiveDuplicateAndLateOwner`、`TestLaunchReadyReviewerSideAndUnknownAreExcluded`、`TestLaunchReadyMetadataFailureKeepsCommittedSession`；只取成功邊界的 typed snapshot，背景核 committed owner，缺失 live 不倒填，失敗只讓報表 unavailable。 |
| side 與 LAN 邊界 | `TestCodexSideReopenUsesConnectedNativeFork`、`TestHCGReportingRequiresPrimaryNativeLoopback`；side fork/reopen 不新增 main launch 通知，偽造 Host／Origin 不放行。 |

同 caller 的 ledger 是唯讀合成 store，沒有開真實 AO DB／真 agent。這不證明所有 live native 身份、帳號歷史歸因、來源捕獲率或節省。backend 與 HCG 必須各跑 suite；`GOWORK=off` backend 建置仍需 repo 內的本地 HCG replace 來源。
