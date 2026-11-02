# C0 本機操作者確認

`operator-consent/1` 是獨立核心。domain service 仍負責 admission、revisions、
recovery epoch、reservation、effect gate 與 provider I/O。native 確認只記錄同意，
不啟動 executor。`coord.policy.update` 保持停用；C0 不會自動啟用 H／S 或 U1 consumer。

## Native 權威鏈

目前只有 Linux 可進入下列 issuer 資格流程，且仍須完成全部 core acceptance 才能
宣稱可用。**macOS／Windows：未驗、fail closed、已知缺口**；取得該平台真實
runtime 收據前，private native channel 拒絕握手、不 mint，readiness 固定
`authorityGo=false`、`native_platform_unverified`。沒有環境變數或設定可略過此 gate。
以下 macOS／Windows peer 原語僅為尚未啟用的實作，不代表平台通過驗收。

封裝桌面主程序為 owned daemon 產生獨立隨機 C0 token，透過 inherited stdin
交付一次。它與 Browser／diagnostics token 不同。stdin marker 或 caller 自選的
nonce 本身不構成 native 身分。owned 的原 owner 程序仍存活時，hello 必須帶相符的
私有 token；即使 peer／bundle 合格，空 token 也不能取得 issuer slot，包含視窗關閉與
同一 owner 重連之間的空窗。token 仍不能取代 OS peer／程式內容驗證。

Linux 在 daemon entry、讀 stdin 與 migrations 之前，用 pidfd 固定原始直接父程序的身分。
production bundled／dev launch 都直接 spawn daemon；只有預期的桌面 basename 可成為
該 watch 的 owner，並須與當次 bundle 的 executable／ASAR 為同一檔案身分。[pidfd](https://man7.org/linux/man-pages/man2/pidfd_open.2.html) 的
exit／reap 事件確認原程序結束後，新 instance 才可不持舊 token，以 peer＋pin 重新配對
為 attach。PID 重用不會把新程序當成舊 owner。CLI standalone daemon 沒有 owned token，
直接走 attach。pidfd 不可用、非直接桌面父程序、捕捉時已失去 owner 或其他不明狀態，
都不當成死亡證據：token 仍為必要條件，不能靠斷線降級；需要重新啟動合格 daemon。
觀察 fd 關閉也不算 owner 結束。mint 材料不寫入檔案、環境、run file、log、artifact 或 renderer。

C0 使用既有 Browser runtime IPC listener 的獨立協定；不共用 Browser grant／command。
HTTP 與 mobile API 不提供 prepare、challenge、mint、confirm、consume 入口。
request reference、`actor=user`、`--confirm` 與 `local_operator_unverified` 都不是權威。

OS peer 來源分別為 Linux `SO_PEERCRED` 與 `/proc/<pid>/exe`、macOS
`LOCAL_PEERCRED`／`LOCAL_PEERPID` 與 `PROC_PIDPATHINFO` executable vnode、Windows
`GetNamedPipeClientProcessId`、process image 與相同 user SID。macOS 使用
[Apple proc_pidpath 對應的 kernel 查詢](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/proc_info.c)，不以 argv 判定 executable。

daemon 啟動後在背景將所屬封裝程式的 executable、macOS Electron framework 與 `app.asar`
的 digest 固定於記憶體，不阻擋其他 listener 啟動；pin 完成前握手與 readiness fail closed。
peer executable basename 須與 pinned 名稱相同，內容也必須相符；每個相異檔案只 hash 一次。
digest 不是可 mint credential。已佔用 issuer 的多餘連線在 hash 前拒絕。
Electron v1 fuse 必須關閉 RunAsNode／main inspector，並開啟 ASAR integrity／
OnlyLoadAppFromAsar；universal binary 每個 slice 都要符合。相同 AppImage 重新 mount
可配對；缺資訊、內容改變、未知 fuse 或通用未封裝 dev Electron，兩種模式均 fail closed。

信任起點是 daemon 啟動時的受信任安裝 bundle；這是程式內容 pinning，並非 publisher
signature attestation。保證限 AO API／session grant 隔離。相同 OS 帳號的 exec／讀檔、
重打包或替換程式、程序記憶體／debug、XTEST 與其他 OS 注入仍屬殘餘風險。
另依賴 Electron 對 packaged app 的 `NODE_OPTIONS` 過濾；目前 fuse 保留此環境選項，
並未以 fuses 關閉全部 NODE_OPTIONS。正式 bundle 對 `--require` 等注入的過濾仍須
實機收據確認，不能由 sealed fixture 或 fuse 檢查推定已通過。
native dialog 不證明物理人類身分；fake dialog 測試不構成真人手勢或阻擋 OS 攻擊的證據。

## 固定計畫與效期

`PrepareOperatorAction` 保存已 admission 的固定計畫，不自動彈窗或執行。
操作者從 native **Operator confirmation → Review next fixed action plan** 選單
主動檢視全部欄位（Linux 的原生選單可用 Alt 開啟）。Review 不註冊 accelerator，
click handler 也拒絕 accelerator 來源，避免 Browser view 合成按鍵直接開啟對話框：purpose、subject、固定 batch、
scope、revisions、risk codes、digests 與 expiry。頁面同時限制字數與行數，不截斷計畫；
所有確認 dialog 預設取消。最後一頁另顯示各 purpose 的固定風險說明、digest、期限及 OS 邊界；
成功後以原生提示回饋「已記錄此計畫的確認」，不宣稱已執行。選單／提示支援 en 與 zh-TW。

Review next 以每個 issuer epoch 的記憶體 cursor 前進，末端循環；Cancel 不撤銷計畫，也不會
阻擋後面的計畫。同 daemon／issuer epoch 的 live consent 不再列入，過期或舊 issuer consent
仍可重新檢視，domain deadline 不變。永久拒絕（包括版本／peer 不符）停止重連；
pin pending／沒有 hello 回應最多六次嘗試，間隔 2／4／8／16／30 秒。
BUSY 不計入失敗上限，仍按相同間隔退避，最多每 30 秒重試一次，直到 slot 釋出或 app 關閉。
未連線時點原生 Review，會顯示不可用原因及「重新連線」。原 owner 退出或 daemon 狀態恢復後，
可手動重新開始配對（包含 TOKEN_REQUIRED 或自動重試耗盡）；仍須通過完整 token／peer／pin 檢查，
不會自動開啟計畫或建立 consent。kernel peer／basename 在佔 slot 前檢查；hash 僅讀取
一般檔案，大小必須符合 pin，Linux nonblocking／no-follow 開檔防止 FIFO 或 symlink 阻塞。
單筆 plan 內容無法顯示時，只取消該次 Review，保留連線與 cursor，下一次可檢視後續計畫。
Prepare 拒絕超過 JavaScript 安全整數上限（2^53−1）的 revision，確保原生對話框能精確顯示。

challenge 只存在記憶體，綁 request／digest，最長五分鐘或 domain 計畫更早的期限。
consent 自 mint 起也最多五分鐘，受相同 domain deadline 限制。mint 在交易中重驗
固定計畫、boot／issuer epoch、revocation 與 deadline。重複 Confirm／回應遺失回原有
live consent；已 consume 的 action 保留原 domain reservation，不延長 domain authority。

issuer 切換、斷線、主視窗關閉或 daemon 重啟，使未使用 challenge／proof 失效。
domain 可重新 prepare 完全相同且仍有效的計畫，再由操作者重新確認；domain 要求的新
recovery epoch 仍須另行取得。已受理 action 沿原 action key 查結果。

既有 `expired` state 以 receipt 的 domain `ValidUntil` 區分兩種情況：

| `expired` 且 | 意義 |
| --- | --- |
| `ValidUntil` 在未來 | consent 過期；未改動的固定計畫仍可重新檢視／確認。 |
| `ValidUntil` 已經過期 | plan 過期；對此固定 action key 已是終態。 |

重新確認不改變 `ValidUntil`；撤銷不回滾已受理的副作用。expiry／issuer 變化都不能讓
replay 再建立一份 reservation。

## Consumer 交易與 readiness

mint 僅由 bootstrap 將 `Store.NativeOperatorIssuer()` 專用 adapter 交給 broker；
一般 Store 與 domain ports 不直接暴露 Confirm 方法。同程序程式碼不構成安全 sandbox。
完成 admission 後，domain 呼叫 `PrepareOperatorAction`。native 確認後，
`GetOperatorActionConsent` 只為完整相符的計畫回傳私有 reference。
`ConsumeInActionTransaction` 必須加入 domain reservation 的同一 SQLite 交易。
consume 失敗會使該 scope 失敗；reservation 失敗則兩者 rollback。replay 回原 reservation，
不得再次 dispatch。H→S qualification 驗原 proof 鏈，不雙 consume 同一 consent；
新的 permission restore 仍須自己的 purpose／consent。

`GetAuthorityReadiness(requiredPurposes, scope)` 要求 live、ready 的合格 native issuer、
非空 closed purpose set，以及 project／owner／branch／native references。
它回 issuer epoch、短期 heartbeat expiry 與獨立、不透明的 qualification reference；
qualification reference 不等於 epoch，也不是可用來 mint 的 proof。
`authorityGo=true` 表示本次程序能詢問此決定，不是 consent、domain eligibility，
也不是全部 core acceptance 的交付證明。缺 bridge／pairing、liveness 過期、purpose
未啟用或 scope 不完整時回 false。HTTP 唯讀入口接受重複 `purpose` 與 `projectRef`、
`ownerRef`、`branchRef`、`nativeRef`；不回 mint 材料。

H 在 pause／export／candidate／stop／freeze 前必須 gate；S 人工恢復與依賴 D-2 batch
恢復的換版也要 gate。S0、OwnerInbox、PR2-S、read 與合法自動恢復保持獨立。
三類受限 SQLite fixtures 驗 C0 核心；真 service、batch 與 H→S 整合仍屬 consumer 的發布 gate。

## 驗證邊界

`test/fixtures/operator-native/prepare.mjs` 與 opt-in `TestC0NativeDesktopPairing`
提供 Linux sealed core fixture，直接使用 production native UI／handoff 模組、真 SQLite、
既有 IPC broker 與 OS peer 檢查。owned 用真 inherited stdin；attach 先由真 owner 啟動
同一 daemon，確認 owner 程序退出後，再由不持舊 token 的新 app 配對。測試會開啟真
native Review 並產生非空 challenge，再掃 HOME／workspace／run file／logs 與雙方環境。
自動開 Review 只用於材料掃描，不算真人主動檢視；不按 Confirm，plan 維持 prepared。

這不是完整 production 接線測試：fixture main.cjs 與 test daemon 沒有執行 production
`main.ts` 的 establish／run-file／owned 判斷，或 `daemon.Run` 的完整啟動接線。
真人 lab 必須使用 production main.ts 與 daemon.Run，不能只用 core fixture 代替。
Linux lab 的 basename 必須是 `agent-orchestrator`；在本主機以既有 profile 啟動：
`aa-exec -p ao-dev-electron -- <lab>/node_modules/electron/dist/agent-orchestrator --ozone-platform=x11`。
保留 Chromium sandbox，不修改 AppArmor／sysctl，也不使用 `--no-sandbox`。
固定 plan 可由受限的測試 domain admission 提供，不新增產品 CLI／HTTP mint 入口。
真人還須確認正式 bundle 的 NODE_OPTIONS 注入不生效，以及 Browser view 合成原
快捷鍵不會開 Review。

Linux 真人確認與完整桌面選單／dialog 視覺仍須另附收據，全部 acceptance 通過後才可
啟用交付。macOS／Windows 沒有真實 runtime 收據，維持未驗、fail closed 的已知缺口；
日後取得收據並核准移除平台 gate 前，不得啟用。跨 OS `go test -c` 只證明編譯；
實作授權、runtime readiness 與實際 acceptance 狀態分開記錄。

Linux fixture 須以 `node_modules/electron/dist/agent-orchestrator` 真正執行，不能改名為
`electron` 通過 basename 檢查。主機 AppArmor 必須明確允許該自有 lab executable 的
user namespace；可用 `aa-exec -p <已存在且獲准的profile> -- <fixture>` 明確套用既有
profile，維持 Chromium sandbox。沒有可用 profile 時回報環境限制，不改 basename
或停用 sandbox。正式 ready／CI／merge 須等兩層 delta 審查及 Linux 真人收據完成。
