# 政策 B：woowah.6 降版手冊

此手冊只適用已核准的 `0.14.7-woowah.6`、Linux/amd64 精確發行包。
其他版本、平台、架構或 binary 不在例外範圍內。這是由使用者操作的退路；AO 不會自動降版。

## 降版前

1. 在新版 AO 的 Sidebar 或設定開啟「受保護 side」（英文為 `Protected sides`），查看隔離與關機原因。
2. 若 side 擋住正常關機，請決定是否使用「關閉 side」。確認後會依原關閉語意刪除對話與未送內容，保留未知壓縮結果的 metadata；這不是恢復對話的方法。
3. 從 AO 選單選擇 `Quit`，完全結束 AO。僅關閉視窗或留在系統匣不算 Quit；若出現阻擋提示，先處理「受保護 side」，再重試。
4. 確認 AO 已完全退出後，才自行安裝下列精確發行包，再啟動 AO。不要只替換 daemon 而讓原 Electron 繼續執行。

## 唯一核准的發行包

| 項目 | 固定值 |
| --- | --- |
| 發行版 | `0.14.7-woowah.6` |
| 平台 | Linux/amd64 |
| 套件 | `agent-orchestrator_0.14.7-woowah.6_amd64.deb` |
| deb SHA-256 | `7de030a8bcb6aff2bf495f96bb7d091d0f228aa74c2c261bde39f13a4c9a1083` |
| 包內 daemon | `usr/lib/agent-orchestrator/resources/daemon/ao` |
| daemon SHA-256 | `506e9979d87f22f61122d3d7fa9f336c7f680ec5f6f15ff0a7f5c9d1f894fc64` |

daemon CLI 自報 `dev`，不能以該字串識別發行版；應核對套件及包內 binary hash。
這份手冊不執行主機安裝、不提供換版腳本，也不改降版 floor 或真 input hold 的保護。

## legacy 期間的限制

woowah.6 不認識政策 B，**沒有 B 的隔離與關機 veto 保證**。
開機恢復、claim、retire 或 cleanup 可能停止或刪除 provider native 對話；native 完整性不保證。
新版留下的 B 紀錄、durable side 內容與真 S0 held/unknown input 必須保留。
不得把舊版成功關機解讀為壓縮已成功、已確定未送出，或 native 對話完整。

若沒有完全 Quit，仍活著的 Electron 可能把同一 `appRunId` 的非空 side snapshot 交給舊 daemon。
隔離 fixture 曾在 pending／frozen 的 recover-retire 兩格各觀察到一次 queued side question 送出，之後 interrupt/delete。
問題可能以不完整上下文送給模型、耗用額度並沿用舊 side 權限；回覆可能因清理而不可見，人工重送也可能重複。
這是 Q9 已揭露的觀察，不是「最多一次」保證，更不是建議流程。

完全 Quit 後的新 launch 不帶舊 Electron RAM；空 snapshot 邊界測試沒有 side native I/O，queue 仍保留。
此證據來自離線 fake provider 與隔離資料，不等於真 provider、實際套件安裝或 native 檔案完整性證明。
S0 真 input hold 不在 Q9 例外內，不能因這項風險而送出 held input。

## 回升新版

從 legacy 版本同樣先用選單 Quit 完全退出，再由使用者自行安裝新版並正常開啟。
新版走一般開機路徑，先讀 B 紀錄；未結受保護 side 不會自動恢復或續送。
重開後仍可從 Sidebar 或設定的「受保護 side」找到它，包含 parent 已刪除、結束、轉為 TUI，或 sideChats／developer mode 關閉的情況。
legacy 期間的任何寫入不會把原歷史 unknown 結算；不能承諾恢復完整 native 上下文。

schema 改變後，例外必須有最終 candidate 的逐檔 migration manifest、22 格生命週期、5 格 S0 smoke 與 Quit／空 snapshot 收據。
舊 schema 收據不能當作新 schema 通過；發現新副作用類型需重新交使用者裁決。
