# RCA native export capability evidence

本文件記錄 RCA-P1-PR1 的版本能力驗證，日期為 2026-11-08。**結果是 gate FAIL：Claude 原文來源維持 UNKNOWN/off。**
這是有效的能力驗證結果；沒有啟用 production capture、修改 launch driver 或建立第二份 usage ledger。

## 證據口徑與已決範圍（.01）

來源類別為 **實測／文件／UNKNOWN**。`supported / unsupported / unknown` 描述能力判定；本輪未證明不可用的來源一律 `unknown`，不寫成 `unsupported`。
事件可見、turn 完成與正式啟用是不同判定；V1/V2、秘密保存及必要的 storage gate 缺證時仍為 off。

固定依據為 contract-rev3（SHA-256 `bf5dd903fabc81bf14b4a5eef94d6e792c041525be5b0316eea8d00af7207950`）§3 V1/V2、§5 P1/P2/P6/P8、§6 M3/M6–M9、§8 F1/A4/A5 與 §9 PR1。
總審 `rca-total-review-rev3` 核准三個文件子項；後續 `rca-pr1-lab-order-1=claude-first`／D-3 決定先驗 Claude 兩格，Codex OTel 在本 PR 標 UNKNOWN，移至 **RCA-PR2** 由 AO 可信 `-c` 注入與隔離測試驗證。
D-5 已核收 FAIL 為契約預期 gate 結果，沒有授權追加 session 或輪次。

核收的 07 實跑報告是 `agent-orchestrator-258/runs/RCA-P1-PR1-report.md`，SHA-256：
`886cc3485d8bbb164ab49852b5f36f4394132c7962320ae1ae6e9596bfd22976`。
此 AO artifact 不隨 repository 發布；[數字附件](../../backend/internal/service/rca/testdata/capability-schema-v1.json) 提供 PR 內可讀的逐輪摘要。
它是 schema=1 的證據 fixture，**沒有 production reader 或啟用 registry**；不得以 fixture 存在就啟用來源。

實跑環境為安裝的 fork deb／stable AO 正常帳號機制下的 disposable lab project，不是隔離 dev build。
AO source 核對基底為 `9ee703a3a92be17a15a8132b51e26b4213a4fb08`；未證安裝 binary 可由此 SHA 重建。
沒有 proxy、付費 API、原文 file exporter、rollout-trace、帳號切換或 stable daemon 重啟。

### 固定版本與啟動邊界

| 項目 | 實測版本／digest | 來源與限制 |
| --- | --- | --- |
| OS | Linux 7.0.0-42-generic、x86_64 GNU/Linux | 實測；其他 OS 未驗 |
| Claude Code | 2.1.319；`dabafec82ca1319fa43d8be36a10fc61b1d11821cb98ad5df04ad95e199b8eed` | 實測版本／安裝檔 digest；Chat 走 ACP |
| Codex | 0.167.0；`cdcb45aaa01d0f14e298985e09465aebccd47f64ecd298aea44814af7b62bbda` | 實測版本／binary digest；本 worker 的 ancestor executable 路徑相符 |
| Claude ACP | 0.70.0；entrypoint digest `9d73d1f0f121fb96cc8badb28c22d5bff02d8582eb2e40360a81c189e1b9422a` | 實測安裝 metadata |
| Agent SDK／Node | 0.3.232／22.23.2 | 實測安裝 metadata；不等同 native OTel SDK 的 effective 版本 |
| native OTel SDK | UNKNOWN | 不從 wrapper 版本或 lockfile 推定已生效的 bytes／queue／等待界 |

在此主機，Claude launcher 為 `$HOME/.local/bin/claude`，實體檔為 `$HOME/.local/share/claude/versions/2.1.319`；
Codex launcher 為 `$HOME/.local/bin/codex`，實體檔為 `$HOME/.codex/packages/standalone/releases/0.167.0-x86_64-unknown-linux-musl/bin/codex`。
絕對路徑在私有 lab 盤點記錄核對；此處用 `$HOME` 正規化。AO 優先 managed launcher（`backend/internal/adapters/agent/binaryutil/managed.go`），不能通用地以 shell PATH 推定 AO 現用版本。

### 角色／模式／來源矩陣

| 格 | 來源類別 | 實際觀測 | 能力判定／正式啟用 |
| --- | --- | --- | --- |
| Claude orchestrator／Chat ACP／inline logs | 實測＋UNKNOWN | 18 parent turns 完成；request/response body、tool_result、subagent_completed 可見；有截斷／解析缺口 | unknown／off；V2、秘密、child identity 與 join 未證 |
| Claude fallback worker／Chat ACP／inline logs | 實測＋UNKNOWN | 獨立 18 parent turns 完成；同類事件與缺口；不從主控格推定 | unknown／off；同上 |
| Codex worker／Chat app-server／OTel | 文件＋UNKNOWN | 無本輪 exporter 實測；project env 入口不足 | unknown／off；依 D-3 移 RCA-PR2 |
| Codex 一般 rollout | 文件＋UNKNOWN | 未執行授權 reader、未讀任何 rollout 檔 | unknown；不能用上游欄位說法當本版實測 |
| Codex standalone debug baseline | 實測＋UNKNOWN | 一次成功，7 items／83,412 bytes，數字見 .03 | 指令執行已證；合成參考而非 active request capture，完整來源 gate 仍 unknown |
| TUI／reviewer | UNKNOWN | 未建立或觀測這些格 | unavailable（未驗），不代表 native 不支援 |

verifiedModes 只涵蓋上述兩個 **Claude Chat ACP 的有限觀測**及 **standalone debug baseline**；不代表 production-supported modes。
實跑只用核准 disposable session 與合成 prompt；兩個 native child 名額保守視為用完，沒有補問模型或重跑。

### 設定、signal 與歸因

Claude 以專用 lab project env 設 `CLAUDE_CODE_ENABLE_TELEMETRY=1`、`OTEL_LOG_RAW_API_BODIES=1`、`OTEL_LOGS_EXPORTER=otlp`，
protocol 為 `http/protobuf`，endpoint 僅為一次性 loopback `/v1/logs`；metrics/traces 為 none，沒有 console exporter。
headers 只含非秘密 lab marker，清除其他指定 headers／certificate／beta endpoint；沒有提高 native content limit。
完整 JSON／合成 prompt 表的 lab 規格 digest 為 `b01ac0023dab429c901b2b57518dfd1e20a640302475af77c17aeef37317ad59`；
helper digest 為 `66fdeb590cef098cc2f690a6a1f182ec574e0998744aceb09d69597ebf7e510b`。

AO project env 經 `agentlaunch.MergeEnv`、account guard、Chat launch 與 ACP 保留；contained 帳號不走 legacy settings sharing。
啟動前僅核 telemetry 鍵名：contained user settings 的相關鍵為 0，具名 Linux managed/drop-in 與 remote cache 檔不存在；
使用者另確認是無 server-managed policy 的個人方案。未讀 credentials、token、真實 session 或輸出設定值。
[官方 managed OTel 優先序](https://code.claude.com/docs/en/monitoring-usage#how-managed-settings-lock-the-otlp-destination)
仍是適用限制，不能把本次檢查推廣至其他帳號／設定。

receiver 僅投影白名單欄位存在性與數字；觀測到 `request_body_id`、`request_id`、`message.id`、`message.uuid`、`event.sequence`、`session.id` 欄位。
原生 ID 值沒有保留，沒有證明 native key→canonical source_event_key 的鏈。兩格重用非秘密 marker；
逐輪數字是同步窗內收到的 batch，不是逐請求／child 的已配對用量，也未去重。不可把 event 計數再加入 canonical ledger。

## native 緩衝、等待與秘密 gate（.02）

以下未知值不能填 0、SDK 預設或 AO 接收上限來替代。

| gate | 來源類別／本輪證據 | 未證的界與結果 |
| --- | --- | --- |
| 單事件／全 record bytes | UNKNOWN；只量收到的 encoded/body bytes | arguments/output/body/resource 及截斷前副本的最壞 bytes 未證；off |
| queue／batch | UNKNOWN；有限多工具與 receiver stall 觀測 | record 數、總 bytes、並行 batches、full 時阻擋／丟棄均未完整證；off |
| serialization／retry | 實測多次 HTTP 503 命中＋UNKNOWN | 並存副本數／bytes、同 batch retry 鏈、backoff／總等待硬界未證；off |
| shutdown／subagent | 實測 AO exit、completion 事件＋UNKNOWN | CLI 返回時間不是 SDK graceful flush；child 名稱／模型／實際 prompt 數不可核；off |
| diagnostics／dump／秘密 | 實測受控 receiver 無 raw 輸出＋UNKNOWN | native 新增 diagnostics／全部 canary 位置與增量保存未完整驗；off |
| P8 磁碟 B/R | UNKNOWN，未做受限 volume 實驗 | 不把本次 sandbox 或檔案 bytes 當 reservation；由 PR2.03 驗證 |

### 兩格故障觀測

| 案例 | 主控格 | 備援格 | 可成立的主張／限制 |
| --- | --- | --- | --- |
| receiver 拒連 | n3 native 完成 | n3 native 完成 | listener 已關；export attempt 未直接觀測，命中 UNKNOWN |
| receiver 程序退出 | n5 native 完成 | n5 native 完成 | 自有 receiver exit0；實際 export attempt 未直接觀測 |
| RST | fault=reset 1 次 | fault=reset 1 次 | 故障命中與 native 完成已觀測；不推 retry 界 |
| 停讀 body／不回應 | n9 1 次、n16 2 次 | n9 1 次、n16 2 次 | helper 讀完 headers 才停讀，最多 30 秒；實際停讀時長未量 |
| HTTP 413 | 1 次 | 1 次 | 拒收命中、native 完成；後續 ack 對照完成 |
| HTTP 503 | 5 次 | 10 次 | 被拒請求數，不能稱同 batch retry 次數或上限 |
| 大工具 output | 一次 320 KiB stdout，body 合計 31,682 bytes | 一次 320 KiB stdout，body 合計 27,810 bytes | body 無截斷標記不證完整含 output；工具端縮減／外置未核 |
| 多工具停讀 | 同 turn 8 次 command，完成 | 同 turn 8 次 command，完成 | 未證 queue 已滿或持續多批資源硬界；進度訊息不另計 turn |
| child | subagent_completed 1 筆 | subagent_completed 1 筆 | 名稱／模型 UNKNOWN，不由 DONE 推定模型符合 |
| 退出觀測窗 | CLI 2.074 秒；解除 stall 有 1 次 fault | CLI 2.068 秒；解除 stall 有 1 次 fault | 含 CLI 往返；AO exited／kill 不當正常 SDK flush 證據 |

故障後的下一 prompt/tool 對照均 completed（主控回條），沒有 restart native、人工 retry 或額外 session。
主控第18輪時間標記與先前時區不同，保留原記錄、不以時間猜 join。
兩格 n1、n18 均見 body_truncated 標記及無法解析 body，保持 partial／UNKNOWN，不修補 JSON 或猜缺少內容。

### 原文與殘留的界線

原始 OTLP／baseline stdout/stderr 只經 receiver/helper 記憶體；報告與 fixture 不含 body、prompt、工具長輸出、headers、native ID 值或 body hash。
lab helper 關閉 core dump、HTTP access/error log；這不是 native SDK 全鏈的零殘留證明。
正常 native transcript/rollout 是 P1 的既有原生保存，不宣稱從未落盤，也未讀／複製真實歷史。
完整 secret-canary 的 body/schema/error/header/疑似 ID 矩陣未完成，不能用「沒有掃到」宣稱 A4 通過。

receiver 三個 generation 均 exit0；兩個 lab session 已終止、project 已移除，registered worktree 已釋放，scratch 已刪除。
AO 建立的 lab 根及其 orchestrator 子目錄共兩個空目錄依清理範圍保留；共用 native history 未讀未刪。
此記錄不修改全機 swap／dump 政策，沒有硬體層零殘留保證。

## 合成 baseline 與關聯鍵（.03）

固定 binary 執行一次 `codex debug prompt-input`，使用新造獨立 CODEX_HOME、空 lab repo、環境白名單與合成短 prompt；
未拷貝 auth/config，OTel 三個 exporter 為 none、analytics 關閉、MCP config 清空；先核具名系統 config 不存在。
原 sandbox 執行，沒有因初始化失敗提權或補跑，亦未試 `--full`。

| 數字 | 實測結果 |
| --- | ---: |
| native exit code／attempts | 0／1 |
| JSON array items | 7 |
| stdout bytes／stderr bytes | 83,412／0 |
| `ceil(stdout UTF-8 bytes / 4)` | 20,853 |
| 新造 baseline 根檔數／邏輯 bytes | 51／308,316 |
| 核准 diagnostics 掃描範圍中的檔案 | 0；不當全鏈證明 |

方法為 `utf8_bytes_div4_v1`。此數字是**整份 baseline JSON 的粗估**，不是 provider tokenizer、互斥成分總和、真請求 input 或訂閱節省。
沒有 component classification／canonical join 就明示 UNKNOWN／unmatched，不將 baseline 貼到某次 canonical request。
JSON array items 亦不代表工具數或模型請求數。

## 給後續 PR 的證據與停止界

- **RCA-PR2／RCA-P1-PR2**：Codex OTel 以可信 `-c` 注入再驗；native typed config 未設定 exporter 時為 none，project-local `otel` 被過濾，AO 現有 project config 沒有任意 argv 入口。
  固定 Codex tag `rust-v0.167.0` 指向 commit `086ccc833bc9cf01024ad2dd6af7dbd421b6bf9a`；參見 [project key 過濾](https://github.com/openai/codex/blob/086ccc833bc9cf01024ad2dd6af7dbd421b6bf9a/codex-rs/config/src/loader/mod.rs#L90)、[exporter 預設](https://github.com/openai/codex/blob/086ccc833bc9cf01024ad2dd6af7dbd421b6bf9a/codex-rs/core/src/config/otel.rs#L10)。不借此改 shared CODEX_HOME。
- **PR2／PR4**：未證 native bytes／queue／序列化／retry／shutdown／秘密 gate 前不得 active；lab receiver 不是 production receiver，也沒有驗 renderer 或 LAN/tunnel 邊界。
- **PR3**：只可使用數字基線作獨立參考；未驗一般 rollout reader，不補讀已清理 lab 或真實 session 檔；native key 存在性不能替代已證 join。
- **PR4**：兩個 Claude 格的有限故障結果不能豁免完整 gate；closed-source 邊界證不出仍 off，不靠 restart/watchdog 或 raw file fallback。
- **全案**：本 PR 交付如實的能力矩陣與缺口，不宣稱階段1／主工項已完成。rollout-trace 仍未授權。
