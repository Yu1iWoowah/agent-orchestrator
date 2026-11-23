# HCG

HCG 是獨立 Go module，提供固定代讀 `hcg_delegate_read` 的核心（`internal/delegate`）、AO backend 使用的薄接點（`reporting`），以及與 RCA 共用的 policy 解析（`policy`、`rca`）。daemon 的既有 `model_usage_events` ledger 是唯一用量正本；代讀的 API 花費經 execution receipt 寫入正本。

代讀的離線驗收見 [delegation-offline-acceptance.md](docs/delegation-offline-acceptance.md)，lab 規程見 [lab-runbook.md](docs/lab-runbook.md)，PAL 採用見 [pal-adoption.md](docs/pal-adoption.md)。

## 驗證入口

```bash
cd components/hcg
GOWORK=off go vet ./...
GOWORK=off go test -race -count=1 ./...
GOWORK=off go build ./...
```

`.github/workflows/hcg.yml` 在 ready PR／main 執行同一套獨立 module 檢查。`go.work` 同時登錄此 module，本機從 root 可執行 `go test ./components/hcg/...`；只跑 `backend/...` 不會涵蓋 HCG。
