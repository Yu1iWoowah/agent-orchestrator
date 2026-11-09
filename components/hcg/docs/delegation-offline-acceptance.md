# HCG-P1-PR1 離線驗收與付費停點

本工項只驗固定 PAL 代讀的整合與離線閘門。production daemon 沒有 credential resolver／provider transport，註冊成功也不代表可以付費執行；`available=false` 時工具不掛載。合成 provider 只透過 constructor dependency 注入，沒有 HTTP、環境變數或工具參數可將 daemon 切成 fixture 模式。

## 固定依賴與唯一 caller

PAL commit 為 `7afc7c1cc96e23992c8f105f960132c657883bb1`。依賴鎖為 `internal/delegate/pal-requirements.lock`，只安裝在私有隔離環境；原始 PAL、依賴樹與 Python binary 每次工作都驗證並複製成唯讀 snapshot，安裝內容不入 repo。正式 public provider registry、`CustomProvider.generate_content` 與 `ChatTool.execute` 負責 PAL 組裝；不改 PAL、不 monkeypatch SDK／私有 context。

`ao usage delegation read` 與獨立 `hcg` MCP server 的唯一 `hcg_delegate_read` 都呼叫同一 loopback HTTP → HCG service → reporting 薄接點 → `Coordinator.Read` → `Consumer.DelegateRead`。CLI 為薄 client，不讀取來源、預算或 SQLite；只有 `_test.go` 為核對價目而唯讀自己的私有 fixture DB。

工具只接受 question、明示 relative paths、focus、output_limit，不接受 provider、profile、credential、session、source root、exec 或 env。server 不借用 `ao` 名稱；`readOnlyHint=false`、`openWorldHint=true` 如實保留，不加 approval bypass。

## 重跑條件與命令

使用自己的短路徑 scratch、私有 HOME／TMPDIR／TMUX_TMPDIR，清除繼承的 AO／provider 憑證環境。沿用專案指定的 Go／npm cache。以下名稱只引用隔離環境位置，不含任何 key；沒有固定 PAL 安裝時相關測例會 skip，不能把 skip 算通過。

```sh
GOWORK=off HCG_PAL_TESTS=1 \
  HCG_TEST_PAL_SOURCE="$FIXED_PAL_SOURCE" \
  HCG_TEST_PAL_PYTHON="$FIXED_PAL_PYTHON" \
  go -C components/hcg test -race -count=1 -p 8 ./...

GOWORK=off HCG_PAL_TESTS=1 \
  HCG_TEST_PAL_SOURCE="$FIXED_PAL_SOURCE" \
  HCG_TEST_PAL_PYTHON="$FIXED_PAL_PYTHON" \
  go -C backend test -race -count=1 -p 8 ./internal/cli \
  -run 'TestHCGDelegation|TestProjectSetConfigDelegationExactRefs'
```

交 PR 前另跑 brief 的 lint、frontend:typecheck、backend／HCG 完整 race、API 生成無漂移及受影響 Windows／darwin `go test -c`。跨平台編譯只證明能編譯，不能代替原生 runner 或真模式批准。

## 證據對照與成立條件

| 主張 | 測例／成立條件 |
| --- | --- |
| 正式 PAL 單一路徑 | `TestPALFormalCallerBothProfiles`：兩家均經固定版實際 `ChatTool.execute`，父 broker 每 attempt 只准一次 transport；不代表真帳號或真模型能力已核。 |
| O1 精確 coverage | `TestPALMissingCriticalScopeNeverDispatches`、scope／payload 測例：question、system、來源與模板精確相符，缺關鍵來源、過大、內容更動、路徑逃逸或 canary 必須零 provider calls；不能用 contains 取代完整比對。 |
| O3 引用可核 | `TestReadReplyEvidenceAndScope`：來源 ID／revision／原文 hash／行範圍／quote 完全對上；錯引用、無證據、自稱 coverage、超限與自由文字均拒絕。`machine_checked` 只驗引用，不表示語意為真。 |
| 同 caller 正本歸因 | `TestHCGDelegationSameCallerOffline`：兩家各經 CLI→HTTP→service→固定 PAL→合成 provider→可信 receipt→私有 SQLite。未知 cache 分桶保持 NULL；沒有 native binding／source 假資料，不改 session 或 project policy。 |
| 同 host 兩 project／兩家 | `TestHCGDelegationSharedHostRevisionBoundary`：共用一個 daemon／budget，各 project 只 allow/select 一家精確 ref；相同 request ID 不合併歸屬。 |
| 所有 policy 失效類別 | 同一 shared-host caller 暫停 project facts，project／role／live import／host-stop／session override 完成 off 或 off→on 後恢復，舊 token 必須零 transport。寫入沿既有有序 commit→cache/revision→cancel，不改 DB trigger／schema。 |
| F2 不偷換 revision | shared-host caller 在 reserve 後更新 host 拒送舊 attempt；明示選 v2 後才能新送。dispatch 後 host 換不同單價 v3，舊 caller 取消、跨家被 hold 擋住；遲到 canonical row 保留 v2 的 reference／價目／確定 total，未知 input cost 分桶仍 NULL。測試 SQL 限自己的 mode=ro fixture DB。 |
| K0–K4 硬預算與復原 | budget 測例核 OpenAI 300／Anthropic 400／合計 630 美元、未知價目／external spend、跨 project 競爭、重啟／月界與未確認已送 hold。`TestCoordinatorStartupReceiptRecoveryNeverCallsProvider`：重啟只補可信 canonical write，保留原 revision／price，不重呼 provider、不回過期答案。 |
| O2 取消與退出界 | `TestPALCallerFaultPhaseCancellationBounds`：在正式 caller 的組裝前、IPC 等待前、答案驗證前用 constructor-only checkpoint 注入不合作停滯，兩家 caller cancel／deadline 皆有界退出、自有工作目錄清理，已取得收據不遺失。這是受控故障點，不宣稱重現 PAL 自身缺陷。 |
| provider 不合作 | `TestPALCallerCancelsUncooperativeTransport`、late receipt 與 shared-host 測例：provider 不理取消仍不阻塞 caller；只停止自有 PAL 工具，不停止 native session。 |
| MCP 取消與薄操作 | `TestHCGDelegationMCPFixedToolAndCancellation`：取消通知傳到同 HTTP request，安全 unavailable、固定工具與參數限制；CLI validation／envelope 與 project JSON replacement 測例保留 error code/message/requestId 及其他設定。 |
| 實際 adapter 接線 | Session Manager Chat 新啟／restore 與 Codex TUI 新啟／restore 合成測例記錄實際產生的 server 設定；Codex adapter argv 測例核固定命令與既有 approval 完全不變。沒有啟動真 agent。reviewer／side 不走這條主 session 掛載與通知邊界。 |

PAL 與工具程序以 `bwrap --unshare-all --die-with-parent` 執行，沒有 provider 網路；父 broker 的測試 transport 只回合成回應，沒有真 key、付費 API 或 live 語料。故障注入、resolver 與合成引用結果不證明真模型品質、批准或節省 token。

## 操作與歷史

專案必須明示 opt-in，並在既有 `project set-config --config-json` 完整設定中給 allowedProfiles／selectedProfile 的精確 id/revision。role／session 只能 on／off／inherit，不能選 provider 或放寬 host cap。project clone 只接受建立者明示的 Config；沒有從另一 project 自動複製 enable／selected 的路徑，預設仍 off。

固定薄操作為 `usage delegation get|set|clear|emergency-stop|profiles|register|read`。register 的 JSON 只有已核 descriptor 與版本化安全引用，不接受 key 值或任意 endpoint。host current 更新後舊 project ref 回 `profile_revision_stale`，部署者需明示換 allow/select；不跟隨 latest/default/auto。live reconnect 保留原 Mount／launch，不把目前專案預設倒寫成歷史；缺失 manifest 為 unknown。工具未實際配置的 session 需等下一個安全 launch，不能只改 manifest 假裝已掛載。

## 後續付費停點

Q14 路徑 A 的離線正式接點已可由固定版精確覆蓋、兩家 raw usage／cap 與硬預算 fixture 驗證；若後續改版使 A 無法閉合，立即 needs-input，不自選 B／C。

付費 lab 仍需主控與使用者另核 Q11／Q13 的具體計畫：只限定合成語料、兩家固定不可變 descriptor、明確 cap／外部花費證明與 stop／receipt 復原方式；credential resolver／真 transport 必須先另核並驗。依 F4 從實際 Codex worker 新啟／resume 的批准與原生 read/rg 可用開始，受擋即 unsupported，不拿另一 harness 的成功代證、不放寬既有權限。

之後的小模型比較應使用同一合成 corpus、相同 question／引用 schema／coverage／完整 cost 口徑與 baseline；品質、效益、Claude 主控／備援、真 repo 語料、本機 GPU 或 key 的使用都不在本 PR，也未執行。
