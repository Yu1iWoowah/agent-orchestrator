# Replacement dry-run／fake harness

這個 harness 只支援 R0 4A 的規劃與 fixture 驗證。它不啟動 AO daemon、Electron、Claude 或任何真 provider，也不呼叫 stable API。`--mode live` 一律拒絕；4B、專用帳號及付費預算需要另行授權。

## 先檢查隔離規劃

`--lab-root` 必須位於使用者的 `~/.ao/scratch/7-*/` 下面，不能是 stable data、Electron state 或任何 symlink alias。每次使用新的 lab 子目錄。daemon URL 只接受專用的 `127.0.0.1` port；拒絕 port 3001 與繼承的 `AO_PORT`。本輪即使 URL 有效也不連線。

```bash
python3 test/orchestrator-replacement-live/harness.py \
  --lab-root "$HOME/.ao/scratch/7-example/replacement-lab"
```

預設 dry-run 只輸出 JSON，不建立 lab 或啟動程序。manifest 包含固定六格的 SHA256、新規劃的 lab subject IDs、隔離 HOME／data／runfile／Electron／Claude config 路徑、usage 前置條件與 cleanup registry。不能指定現有 project／source／worker IDs。所有 live 格及 A15／A23 保持 `NOT_RUN`；這不是帳號或 native profile 能力檢查。

## 驗證 fake 路徑

遵守專案的 W4 負載與併行限制；fake runner 啟動前也會檢查 load。執行時先設 `umask 022`，使用自己的 0700 scratch。不要把真帳號 config 複製進 lab。

```bash
python3 test/orchestrator-replacement-live/harness.py \
  --lab-root "$HOME/.ao/scratch/7-example/replacement-fake" \
  --mode fake
```

Linux fake runner 只執行三個固定的 Go fixture tests：coordinator 的 gate 外通知與 lost reply、busy Chat queue 去重、TUI 單次受保護寫入。它設定 private HOME／TMPDIR／TMUX_TMPDIR、清除繼承的 AO/provider 環境，使用既有 Go cache，禁止下載 toolchain／module。它不接受任意 command 或 test selector。

結果留在新 lab 的 `fake-tests.log` 與 `manifest.json`，不覆寫舊證據。registry 記錄該次 Go test PID／start time／exit code。timeout 只處理已驗證的自身 process group，不按名稱廣泛 kill。fixture 的資源由各測試 cleanup；Go process exit 不能替代真 provider tree 的停止證明。

fake `PASS` 只證明這三項 fixture 行為，不代表六格 live、原生 `/export` 對照、Claude 帳號、桌面操作或 R0 全部完成。provider usage／費用尚未觀測，不將 `providerCalls: 0` 當成未來 live 的費用預估。

## Harness 自測

先依 worker 規則準備 private HOME／TMPDIR，並把 TMPDIR 放在自己的 `~/.ao/scratch/7-*/`。以下只跑 dry-run／拒絕測試：

```bash
python3 -m unittest discover -s test/orchestrator-replacement-live -p 'test_*.py'
```

拒絕矩陣包含 stable path／port、runfile/root symlink、外部 subject ID、未授權 live。新加實際 live executor 前，必須先取得 4B 的帳號／預算／D 授權，並重新審查 identity、PID、cutoff、usage 與 cleanup 邊界。
