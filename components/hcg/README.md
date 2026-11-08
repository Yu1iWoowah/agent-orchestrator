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

**Q2 真人證據：本工項未取得、未證。** 未讀 live AO DB／真帳號或啟動真 provider。四種模式的實際 native 身份、來源完整度、順序／finality、compaction 後單次 usage 與帳號歸因仍需另授權取證；這些案例不證明 current 可為 known，也不代表節省或完整使用量。current／cohort 不在本 PR。
