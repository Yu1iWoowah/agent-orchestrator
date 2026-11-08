# HCG usage reader

HCG 是獨立 Go module，透過 daemon 唯讀 API 查詢「模型請求事件」。daemon 的既有 `model_usage_events` ledger 是唯一正本；這個元件沒有原生檔 reader、持久 token DB、cursor 或手工匯入格式。

## 操作與資料根

在 repository 根目錄執行：

```bash
go run ./components/hcg/cmd/hcg usage events --session <session-id> --json
go run ./components/hcg/cmd/hcg usage sync --session <session-id> --json
```

兩者都重新掃描當次 API，輸出可丟棄 JSON 數值快照，不寫入檔案。`--session` 可由 `AO_SESSION_ID` 提供預設；參數錯誤 exit 2、讀取失敗 exit 1。成功輸出 JSON 不代表來源已收齊。

預設使用 `~/.ao/running.json` 的 loopback port 與 `~/.ao/data` 的資料 namespace。`AO_RUN_FILE` 選擇 daemon，`AO_DATA_DIR` 記錄設定的資料根；AO 本身的 `AO_DATA_DIR` 不會改變 run-file 位置，所以設定它時必須同時明示 `AO_RUN_FILE`。此處不讀取 DB，也無法由 run-file 證明 daemon 的實際資料根；`dataRootStatus=unverified_configuration`，namespace 設定不是歷史帳號／原生來源歸屬證明。隔離 fixture 必須同時指向自己的 run-file 和資料根，不要只設資料根後連到 stable。

## 數值與掃描限制

- `source=daemon_ledger`；未整合外部報表，不與 `external_analysis` 相加或互補。
- API 第一頁取得 `throughId`，後續頁面固定此新增列上界；它不是隔離的歷史 transaction snapshot。掃描期間既有列的 repair 與 binding/source/session metadata 都可能改變，下次查詢重新讀取。
- `scanStatus=complete` 只表示有限分頁成功完成。失敗／重複或衝突 id/key／錯誤 cursor 會回 partial 或 unavailable，保留本次已接受頁面，exit 1；不補入先前快照。
- input 已含 cache、output 分開；null 不補零、不重算 canonical token。id 是寫入次序，不能當最近已送出 request 順序。事件不等於完整輪次。
- binding/source 包含 root／child identities、generation、state、error 與 anomaly；即使沒有事件也會列出已發現來源。沒有事件不等於沒有消耗，沒有原生總事件數分母就不能宣稱捕獲率 100%。
- `sourceAsOf`／`metadataAsOf` 是 daemon 本次讀取時間；role/mode 是查詢當下 metadata，account 只附查詢時 settings 的可得 id，未知維持 null，不能據此推定歷史帳號歸因。
- provider usage 是 daemon 的安全數值白名單投影；未知／非法／超過 JavaScript 安全整數範圍的值不輸出，狀態隨事件保留；projection complete 只指已存欄位都可安全投影，不代表單次 usage 欄位完整。正文、路徑、憑證不經此欄位輸出。

## 驗證入口與來源證據

```bash
cd components/hcg
GOWORK=off go vet ./...
GOWORK=off go test -race -count=1 ./...
GOWORK=off go build ./...
```

`.github/workflows/hcg.yml` 在 ready PR／main 執行同一套獨立 module 檢查。`go.work` 同時登錄此 module，本機從 root 可執行 `go test ./components/hcg/...`；只跑 `backend/...` 不會涵蓋 HCG。

`testdata/usage/events.json` 與 CLI HTTP fixture 涵蓋 Claude Chat、Claude TUI、Codex Chat、Codex TUI 的 root／child metadata、null 與 source error；backend 整合測試經真實暫存 SQLite → service → HTTP 驗證讀取。這些都是合成案例，只證明既有資料的讀取與投影，不改 collector 的探測／綁定行為。

**Q2 真人證據：本工項未取得、未證。** 未讀 live AO DB／真帳號或啟動真 provider。四種模式的實際 native 身份、來源完整度、順序／finality、compaction 後單次 usage 與帳號歸因仍需另授權取證；這些案例不證明 current 可為 known，也不代表節省或完整使用量。current 目前回 UNKNOWN；cohort 只彙總已記錄向量。


## current 與規則提案

```bash
go run ./components/hcg/cmd/hcg usage current --session <session-id> --json
```

current 同樣可用 AO_SESSION_ID 作 session 預設。成功查詢但無法證明指標時 exit 0，回 `metric=last_observed_prompt_input_tokens`、`status=unknown`、`tokens=null`、`band=unknown`；runtime 失敗仍輸出結構化 UNKNOWN 並 exit 1，參數錯誤 exit 2。來源／掃描異常不退回較舊值。

本輪沒有 Q2 指定版本／來源的正面證據，C1 也沒有 sequence/finality/latest-pending，所以沒有可合法選定的最新 request。完整單次 usage、最大 id、timestamp、cumulative 差值或 occupancy 都不足以證明最近送出。current 因此不產出 known，不提供自報已驗證旗標；`source_event_id/event_at/age` 保留 null。`source_ref` 只列可觀察的主線來源 metadata、排除 child；`synced_at` 是讀取時間，不是 request 新鮮度證據。age 若無可選定事件即未知，不拿同步 age 冒充。

[context-rule-proposal.md](docs/context-rule-proposal.md) 是具名替換 shared orchestrator 規則及停用舊腳本的提案，尚未套用。只在將來具名來源可證時才讀 known/tokens：399,999 → below_400k、400,000 → handoff_400k、449,999 → handoff_400k、450,000 → urgent_450k；UNKNOWN 不當低於門檻，也不解除既有警戒。工具不執行交接或 compact。

## canonical cohort baseline

```bash
go run ./components/hcg/cmd/hcg usage baseline --sessions <id-a>,<id-b> --from 2026-11-08T00:00:00Z --to 2026-11-09T00:00:00Z --json
```

session 清單必須明示、唯一且非空，baseline 不使用 AO_SESSION_ID 預設；from/to 必須是 UTC RFC3339、from 早於 to。報表原樣帶回清單順序與輸入時間字串，task 就是 session id，不儲存 cohort 標籤或 task mapping。

`recorded` 只含 from（含）至 to（不含）間的既有事件；created_at=null 的事件永遠另列 `undated`，不偷放進日期範圍。只有來源明示的 0 才是 0；任一已列事件某個 counter=null，該層 counter 合計就是 null，incompleteEvents 只按四個中性 token 欄位計，不因角色／帳號缺失降級。input 已含 cache，output 分開；provider 原始 bucket 不重新改算 ledger。

報表含整個 cohort、各 session、各 source 的階層總量；同一 event 在每一層只加一次，**不要把父層與子層相加**。來源保留 root/child/unknown、native identities、state/error/anomaly，以及已納入事件的 measurementKinds 原值集合；native_reported 也可能來自 cumulative delta／last fallback，不提升為已證單次 request 精度。沒有事件的已發現來源也保留。role/mode/account 等 metadata 只是查詢時附註，無法證明歷史歸屬。

status=complete 僅指指定掃描與加總完成，仍不能推定完整使用量。來源探測／error／unsupported 與數值分開展示；沒有原生事件總數分母，captureRate 固定 null、coverage=unknown_no_native_event_denominator。部分掃描回 partial/unavailable 與 exit 1，不補舊值；numeric overflow 亦停止、保留 partial 並 exit 1。

同 cohort 只代表相同 session 清單／日期界線；C1 的 repair 與多 session 不同觀察時間可能改變結果，sourceAsOf/metadataAsOf/throughId/consistency 隨報表保留，不宣稱隔離歷史 snapshot。沒有安裝或呼叫 llm-usage-metrics 等外部引擎；外部分析只能是將來 `source=external_analysis` 的獨立完整 snapshot。無穩定 request id 就不 append/repair join、不與 daemon_ledger 相加或補缺。
