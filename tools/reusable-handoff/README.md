# 手動換主控腳本

`manual-handoff.sh` 是 `agent-orchestrator` 專案換主控的唯一入口：kill 舊主控，再帶初始 prompt 建新主控。舊的 RH 交接腳本（`handoff.py`）與 `manual-handoff.py` 已退役並刪除（使用者 2026-11-23 rh-postmortem-rules-1＝retire；manual-handoff-improve-1＝simple）。腳本不做自動恢復；任何一步失敗就停下，由人決定下一步。

## 舊主控在啟動前要做的事

1. 取得使用者換主控的授權，並請使用者確認一次新主控的 harness、帳號、model、effort。
2. 確認涉及的 worker 都停在安全點。
3. 把交接文件 `handoff/orchestrator-handoff.md` 寫到最新，含在途決定與存活 worker。kill 之後舊主控就不能再寫。
4. 告訴使用者 log 位置：`$AO_DATA_DIR/artifacts/<舊主控 id>/handoff/manual-handoff.log`。
5. 把腳本複製到自己 artifact 的 `handoff/`，再以 `setsid nohup` 啟動，作為本回合最後一個動作：

```sh
cd "$AO_DATA_DIR/artifacts/$AO_SESSION_ID/handoff" && \
  setsid nohup bash ./manual-handoff.sh --agent claude-code --account <帳號 id> --model <model> --effort <effort> \
  </dev/null >/dev/null 2>&1 &
```

四個設定參數都可省略；省略的不傳給 `ao spawn`，由專案設定的 orchestrator 決定。`--approval-mode` 預設 `bypass-permissions`；省略時 AO 會用 `auto`，新主控可能卡在核准提示。

## 腳本做什麼

| 步驟 | 動作 | 失敗時 |
| --- | --- | --- |
| 身分 | 舊主控 id、專案、資料目錄取自呼叫者的 `AO_SESSION_ID`、`AO_PROJECT_ID`、`AO_DATA_DIR`，不讓人輸入，避免 kill 到別的 session。 | 缺任一項就不啟動。 |
| kill | `ao session kill <舊主控> --project <專案>`。回覆在終止記錄完成後才回傳，不需要另外輪詢。 | 記 `STOP step=kill` 並停。 |
| spawn | `ao spawn --kind orchestrator --prompt <初始 prompt>`。CLI 會等到 session 建好；建立失敗會回非零。 | 記 `STOP step=spawn` 並停。 |

- 呼叫 `ao` 前先拿掉 `AO_SESSION_ID`、`AO_PROJECT_ID`，改為明帶 `--project`。CLI 會把 `AO_SESSION_ID` 當成呼叫者的回合；舊主控被 kill 之後，背景腳本不能再掛在它名下。
- 腳本以 `set -euo pipefail` 加 `trap` 執行：任何指令失敗都會在 log 寫下失敗的步驟與 exit code，然後退出。

## 新主控的初始 prompt

新主控依序做下面四步。任何一步失敗，就向使用者回報失敗的步驟並停止。

1. 用 `cp -a --update=none` 把舊主控 artifact 複製到自己的 artifact。
2. 用 `ao session settings` 讀 provider 回報的 model、effort 與帳號，用 `ao session get --json` 讀 harness，確認與核准設定一致。
3. 讀交接文件，向使用者回報接手；再以自己的身分寫 `handoff/receipt-<舊>-<新>.md`（`--expected-hash missing`）。寫入成功就表示已拿到共用資料夾的 lease。
4. 依交接文件親自通知存活 worker。

複製與核對由新主控自己做，所以不必等腳本放行。腳本只負責 kill 與 spawn，換手過程沒有固定等待，也沒有輪詢。

## 失敗時

看 log 最後一行的 `STOP step=`。kill 失敗時，舊主控仍在。spawn 失敗時，舊主控已終止，專案暫時沒有主控，下一步由使用者決定；這時已無法以舊主控身分重跑腳本。新主控那四步失敗，會由新主控自己回報。
