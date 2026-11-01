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
nonce 本身不構成 native 身分：owned 與 attach 都須通過下列 OS peer／程式內容驗證，
owned 另驗私有 token。mint 材料不落地至檔案、環境、run file、log、artifact 或 renderer。

C0 使用既有 Browser runtime IPC listener 的獨立協定；不共用 Browser grant／command。
HTTP 與 mobile API 不提供 prepare、challenge、mint、confirm、consume 入口。
request reference、`actor=user`、`--confirm` 與 `local_operator_unverified` 都不是權威。

OS peer 來源分別為 Linux `SO_PEERCRED` 與 `/proc/<pid>/exe`、macOS
`LOCAL_PEERCRED`／`LOCAL_PEERPID` 與 `PROC_PIDPATHINFO` executable vnode、Windows
`GetNamedPipeClientProcessId`、process image 與相同 user SID。macOS 使用
[Apple proc_pidpath 對應的 kernel 查詢](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/proc_info.c)，不以 argv 判定 executable。

daemon 啟動時將所屬封裝程式的 executable、macOS Electron framework 與 `app.asar`
的 digest 固定於記憶體。peer 必須符合這些程式內容；digest 不是可 mint credential。
Electron v1 fuse 必須關閉 RunAsNode／main inspector，並開啟 ASAR integrity／
OnlyLoadAppFromAsar；universal binary 每個 slice 都要符合。相同 AppImage 重新 mount
可配對；缺資訊、內容改變、未知 fuse 或通用未封裝 dev Electron，兩種模式均 fail closed。

信任起點是 daemon 啟動時的受信任安裝 bundle；這是程式內容 pinning，並非 publisher
signature attestation。保證限 AO API／session grant 隔離。相同 OS 帳號的 exec／讀檔、
重打包或替換程式、程序記憶體／debug、XTEST 與其他 OS 注入仍屬殘餘風險。
native dialog 不證明物理人類身分；fake dialog 測試不構成真人手勢或阻擋 OS 攻擊的證據。

## 固定計畫與效期

`PrepareOperatorAction` 保存已 admission 的固定計畫，不自動彈窗或執行。
操作者從 native **Operator confirmation → Review next fixed action plan** 選單
（`CommandOrControl+Alt+Shift+O`）主動檢視全部欄位：purpose、subject、固定 batch、
scope、revisions、risk codes、digests 與 expiry。頁面同時限制字數與行數，不截斷計畫；
所有 dialog 預設取消。最後一頁另顯示各 purpose 的固定風險說明、digest、期限及 OS 邊界。

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

完成 admission 後，domain 呼叫 `PrepareOperatorAction`。native 確認後，
`GetOperatorActionConsent` 只為完整相符的計畫回傳私有 reference。
`ConsumeInActionTransaction` 必須加入 domain reservation 的同一 SQLite 交易。
consume 失敗會使該 scope 失敗；reservation 失敗則兩者 rollback。replay 回原 reservation，
不得再次 dispatch。H→S qualification 驗原 proof 鏈，不雙 consume 同一 consent；
新的 permission restore 仍須自己的 purpose／consent。

`GetAuthorityReadiness(requiredPurposes, scope)` 要求 live、ready 的合格 native issuer、
非空 closed purpose set，以及 project／owner／branch／native references。
它回 issuer epoch、短期 heartbeat expiry 與 qualification reference。
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
既有 IPC broker 與 OS peer 檢查。owned 用真 inherited stdin；attach daemon 保有未交給新 app
的私有 token。自動測試只驗 pairing、材料暴露與 plan 維持 prepared，不模擬確認手勢。

Linux 真人確認與完整桌面選單／dialog 視覺仍須另附收據，全部 acceptance 通過後才可
啟用交付。macOS／Windows 沒有真實 runtime 收據，維持未驗、fail closed 的已知缺口；
日後取得收據並核准移除平台 gate 前，不得啟用。跨 OS `go test -c` 只證明編譯；
實作授權、runtime readiness 與實際 acceptance 狀態分開記錄。
