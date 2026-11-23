# HCG

HCG 是獨立 Go module，提供固定代讀 `hcg_delegate_read` 的核心（`internal/delegate`）、AO backend 使用的薄接點（`reporting`），以及與 RCA 共用的 policy 解析（`policy`、`rca`）。daemon 的既有 `model_usage_events` ledger 是唯一用量正本；代讀的 API 花費經 execution receipt 寫入正本。

## 代讀怎麼用

目前只開 OpenAI。

1. 專案設定開 `hcgDelegation.enabled`（可再分別開關 worker／orchestrator），並在 `allowedProfiles`／`selectedProfile` 選定 profile。
2. `ao usage delegation register --profile-json '<JSON>'` 登錄 profile（`deployment.profile` 含模型、context／輸出上限與價目；`deployment.credentialRef` 與 `deployment.workspace` 指向 key；`externalSpend` 是本月 AO 以外的已知支出）。登錄同時核准當月預算期間（月上限 openai 300、anthropic 400、合計 630 美元）。
3. 使用者本人放 key：`~/.ao/credentials/hcg/openai/<name>/<version>.json`，目錄 0700、檔案 0600，內容為 `{"provider":"openai","name":"<name>","version":"<version>","workspace":"<OpenAI project id>","secret":"<key>"}`。AO 只讀不寫；檔案不存在或權限不符時代讀回 `unavailable`。
4. 緊急停用：`ao usage delegation emergency-stop --enabled true`。

掛載條件：專案開啟、角色開啟、profile 已登錄且可用、沒有緊急停用。符合時，新開的 worker／orchestrator Chat session（Claude、Codex）與 Codex TUI 會多一個 `hcg` MCP server，工具名 `hcg_delegate_read`；Claude Chat 已預先批准這個工具。

`paths` 填 worktree 內的相對路徑；orchestrator 另可用絕對路徑讀 AO 資料根下的 `artifacts` 與專案 shared 目錄。daemon 讀檔時算 hash，依 profile 的 context 上限限制總量，排除秘密樣式與敏感路徑後一次送出；回覆中每條 evidence 的路徑、行號與逐字引文都經比對才交回。回傳 `unavailable` 時改用原生 read／rg。

## 驗證入口

```bash
cd components/hcg
GOWORK=off go vet ./...
GOWORK=off go test -race -count=1 ./...
GOWORK=off go build ./...
```

`.github/workflows/hcg.yml` 在 ready PR／main 執行同一套獨立 module 檢查。`go.work` 同時登錄此 module，本機從 root 可執行 `go test ./components/hcg/...`；只跑 `backend/...` 不會涵蓋 HCG。
