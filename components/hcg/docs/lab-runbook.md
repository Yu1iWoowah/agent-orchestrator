# HCG-LAB-PR1：離線交付與後續 lab 操作規程

本 PR 實作付費 lab 的 D1–D3 前置，僅用 `fixture-model`、非秘密合成 marker、fake HTTP／私有 SQLite 驗證；沒有使用真 key、呼叫 provider、付費、啟動真 agent 或操作 stable。merge 不是付費執行授權。契約為已總審的 `paid-lab-plan.md`（SHA256 `b34decb87749d0dbde3689bf7a46af869b432dd3532aee33fdf6153eb3802804`）§9 A-L1–A-L7；後續主控仍須具名放行實跑。

## 唯一 owner 與管理入口

- daemon 先取得原 data-dir lock，HCG metadata 存在卻缺 `ao.db` 時，在 `sqlite.Open` 前拒絕。鎖不支援／未持鎖者沒有可用 lab parent；不新增另一個 owner root、daemon 同步或 receipt DB。
- grant 只經既有 `usage delegation register --profile-json <已核非秘密JSON>` → primary loopback `PUT /api/v1/hcg-delegation/profiles` → service/reporting/Coordinator。不是 MCP/read 參數，不從 env 或 model 取得授權。
- `grant` 包含 runId、持久 ownerRoot、原 canonicalOwner（projectId/sessionId）、UTC startsAt/expiresAt（最多24小時、不跨UTC月）、兩家精確 profile id/revision、明示 bindings、providerCapNanos（兩家各10,000,000,000）、totalCapNanos（20,000,000,000）、attemptsPerProvider（26）。run/root、canonical owner、期限、profile、上限不可變；operator 只能追加已存在且符合原 project 的 session 綁定。
- service 比對 SQLite 真正開啟的 root，要求 owner-only 實體目錄、原 project/session 列；不造假 identity。profile/effective 回應的 `runId`／`ownerRoot` 必須與本次持久 lab 一致。
- 無 grant／錯 root／過期／不在名單的 session/profile、未恢復的 hold/pending 都不能外送。工具仍只有原 question/paths/focus/output_limit，不增加 key、provider、profile、env 或 exec。

建議持久 root 沿總審的 `~/.ao/data/hcg-paid-lab/hcg-paid-cloud-20261110-r1/`；實際值由後續主控填准。HOME/TMPDIR/TMUX_TMPDIR、安裝與 workspace 可在短 scratch；**持久 DB/root 不是要清掉的 scratch**。CLI 必須明示已核 lab 的私有 `AO_RUN_FILE`／`AO_DATA_DIR`，不可借用目前 stable run-file。這只是選定原 HTTP client 的目標，不是以 env 發 grant。

## Parent transport 與版本 store

正式 parent 固定使用 OS 使用者家目錄下 `.ao/credentials/hcg-paid-lab/`，不隨 lab 的私有 HOME 搬進 scratch。版本位置是 `provider/ref-name/version.json`；每層0700、一般檔0600、owner須為實際執行UID，拒 symlink／非regular。JSON 僅有 provider/name/version/workspace/secret；只有本人後續依已核安全操作放置真值，本 PR 沒有建立此 store 或寫 key 的產品入口。

版本不可覆寫為新 key；輪替須新 version 與重新核定的 profile/run。只依原不可變 ref 讀指定版本，不讀 current、不 fallback。binding metadata 必須與 descriptor 的 provider/workspace 相符；OpenAI 固定 `OpenAI-Project`，Anthropic 使用已核 workspace-scoped key 的綁定，沒有任意 header override。

兩家 endpoint 固定為第一方 `https://api.openai.com/v1/chat/completions`、`https://api.anthropic.com/v1/chat/completions`，model／payload 取原已核 immutable descriptor／K0。TLS 正常驗證、不採環境 proxy、不 redirect/retry/fallback，單次有120秒上限並繼承更短 run／caller 期限。認證只進 parent HTTP header；不進 PAL child、MCP、env、來源、log 或回覆。錯誤 body／任意 header 不外傳，只保留安全 request id／status；usage 沿原 K4 parser／canonical recorder，未知不能補0。

## Run 限額與停止

原同一 budget journal/file lock 在 reserve／final dispatch 同時驗月上限300／400／630與本 run 10／10／20、期限、session/profile、每家26次。最壞 input ceiling＋硬 output 預留不因估算折扣降低；外部支出仍須完整、已知、排除此 gate 的保守證明。

`EverDispatched` 是同 attempt 的不可回退事實；refund／released、settled、取消、失敗、未知及重啟都不回收名額。parent 可證未送（例如 resolver 拒絕）可釋放美元 hold，但若 dispatch 授權已持久提交仍占名額；在 dispatch 前拒絕才不占名額。沒有補額、展期、換 arm/session 重置或第二套 token 帳本。

未知 usage、canonical 故障或其他未解 hold 使後續拒送。任一家配額／金額耗盡或期限、O1–O3／K0–K4、安全／帳務 gate 失敗，operator 依總審 §6.1／§8 停整個 lab、回主控；不能換家承接、補樣本或 retry 湊矩陣。產品不新增 case scheduler。

## 固定十題與交錯表（每個角色相同）

B=baseline、O=OpenAI、A=Anthropic。每個 arm/trial 用不同 session/native context；resume 先在自己的 context 做固定無來源暖身「只回 READY，不讀檔、不呼叫工具」，再沿原 snapshot/native 恢復，暖身亦納入量測。不把前組答案／baseline 來源帶入後組，也不強迫 baseline 全讀。

| 題序 | id | mode | 三 arm 順序 | treatment 預期 |
| --- | --- | --- | --- | --- |
| 1 | L-a | fresh | B→O→A | 一次必要代讀 |
| 2 | L-b | resume | O→A→B | 一次必要代讀 |
| 3 | X-a | fresh | A→B→O | 一次必要代讀 |
| 4 | X-b | resume | B→O→A | 一次必要代讀 |
| 5 | C-a | fresh | O→A→B | 一次必要代讀 |
| 6 | C-b | resume | A→B→O | 一次必要代讀 |
| 7 | N-a | fresh | B→O→A | 僅本範圍 not_found |
| 8 | N-b | resume | O→A→B | 僅本範圍 not_found |
| 9 | P-a | fresh | A→B→O | scope_too_large、零API |
| 10 | P-b | resume | B→O→A | scope_too_large、零API |

F4 順序是 Codex worker Chat → Claude 主控 Chat 的隔離等價 session → Claude worker Chat，前格未過則後格 NOT RUN。Codex 計分前每家僅 C-a 的 fresh/resume 兩次 qualification。每家單次路徑是2資格＋3角色×8正常題=26，最多52次；90個計分 trial 與12個 treatment 過大格不加樣本。多 tool invocation 一律全計，標 MULTI_CALL／protocol FAIL，不挑成功或便宜一次。

未開始列 NOT RUN 與 quota_exhausted/budget_exhausted/authorization_expired/prior_stage_failed 等原因，主模型用量 N/A；開始未完成是 INCOMPLETE，保留已觀測用量、unknown、hold。其餘使用 PASS、PASS_EXPECTED_REJECTION、FAIL／UNSUPPORTED。缺任一預定格不得縮分母宣稱完整通過；保留全任務、warmup、fallback 與 native/cache 用量，不能靠字數造節省。

corpus 與逐字 question/truth 依計畫附錄，不讓 truth 進來源 root。manifest digest `2e816b800fadac4796e95702e36b3948bbd0fc51c0fa92213fbd32ffb382d5c6`，truth digest `5b981847a78003db8ffedb11c909d4329d45f89039b6ae0d66d533f361d8f1dc`。hash／表格只證結構與 bytes，不證真模型品質、批准或節省。

## 恢復、保留與公開證據

1. 只停止自己已核 PID 的 lab；不停止 stable/native session。確定前 owner 已退出並取得同一 data lock，以相同 build、**同 root／原 DB** 啟動，先不付費。
2. 原 project/session、native/launch/controller generation、registry、budget/hold/pending 與 receipt provenance 必須完整。缺 DB 不建立替代；錯 owner、缺 identity、缺 journal/pending 或衝突拒絕啟用、保留原 hold。恢復只重播可信原 canonical write，原版價目／attempt，冪等且零 provider 重送。
3. 啟動恢復不會自動取得 grantReady。主控重新核 remaining 期限／金額／名額與 mount 後，才可經原 registration 明示續行；未結 unknown 不因沒有 pending 檔就清零。get/profiles 的 ownerRoot/runId 必須與同一 canonical owner 相符。
4. 持久 DB與必要identity、registry、journal/pending 至少保留30日，且所有hold對帳、獨立audit與主控核收完成後，才另請使用者授權刪除。停止的 session 只保留／封存，不因清 workspace 硬刪原列。
5. 清自己的 scratch/runtime/workspace 前保全本輪證據；**不刪持久 root**。若未來另核整根刪除，先以一致性方式保全 DB 已提交 WAL、原 identity/metadata/registry/journal/pending 完整集合到核准 owner-only AO 位置，以無 key/provider 的隔離恢復驗證數值／去重／hold，並由主控指定副本為唯一接任 canonical，才可刪原根。沒有新備份平台／mover；不複製 credential、登入憑證或 env。

後續實跑 brief 必須列每個已知 daemon 的目前 build/instance/root、核對時間與公開唯讀來源。對其他 daemon（含 stable）只用其**已授權**公開 `GET /api/v1/hcg-delegation/profiles` 或現有 capability/version view；成功、完整的空 history 才支持無 deployment/ref，非空／404／timeout/未知都不能當無付費能力。再以實際運行 build 的原件／公開 view 證付費 factory 未啟用，不以 repo main 或舊 merge 代證；缺證據即零付費，不改 stable 來湊證據。

本 PR 未執行這些 live 核對。真正 key 放置、原生登入、帳務ID／價格／費項、外部spent/hold、網路與安裝、F4批准及真人UI操作清單，仍由主控在後續實跑前核准；不藉離線 fixture 或 merge 宣稱已取得。

## HCG-LAB-LUNA：新 profile 與 r3 停點

使用者已核准 `gpt-6-luna`、`reasoningEffort=medium` 與 new-r3 獨立額度。
這次程式交付仍不授權建立或啟動 r3、不修改 r1/r2、不呼叫付費 API。
OpenAI profile 必須使用新 revision／priceVersion／digest，不能覆寫 r2 profile。
建議 priceVersion 為 `lab-luna-20261111-standard-30m-r1`，period 取新 run 實際 UTC 月。

| profile 欄位 | 固定值與核對 |
| --- | --- |
| model | `gpt-6-luna`（精確匹配，不接受 prefix 或未核 snapshot） |
| reasoningEffort／serviceTier | `medium`／`default`，HTTP 註冊到 immutable snapshot 不可遺失 |
| contextTokens／maxOutputTokens／outputTokens | 1,050,000／128,000／2,048；最大 input 是 922,000，不是 context ceiling |
| prices 短級 ordinary/read/write/output | 100,000,000／10,000,000／125,000,000／500,000,000 nanoUSD per million |
| prices.longContext | thresholdInputTokens=272000；長級 200,000,000／20,000,000／250,000,000／750,000,000 |
| cache write | 一般區域、Standard、預設 30m；只在已核 Luna schema 下省略 write 葉等於 0 |

request 必須明送 `reasoning_effort=medium`、`service_tier=default`。
只在實回 `service_tier=default` 時使用 Standard 價目；缺失、null、非字串、auto 或其他
模式一律 unknown／保留原 hold、名額並停止，不補 default、不 retry。
canonical identity 保留 requested/observed/status 的安全分類，數值投影保留 write；
InputCost 含 ordinary＋write，UncachedInput=I−R 仍含 write，output 已含 reasoning。
272,000 以上整筆使用長級；最壞 reserve 是 264,036,002 nanoUSD（含分項取整餘量）。
這不證明非 Standard 未知實付受到該 reserve 涵蓋；未證模式仍須凍結、對帳。

r2 OpenAI 已用 2 次／US$0.00549、Anthropic 1 次／US$0.0054，合計 US$0.01089，
全部留原 root／DB／journal／receipt，不退名額、不重算、不搬成空 journal。
new-r3 另給每家 US$10／26 次、總 US$20；r2+r3 潛在累計 OpenAI 28 次／US$10.00549、
Anthropic 27 次／US$10.0054，總 US$20.01089。r2 僅保留已花部分，不再外送。
本 PR 不做同帳本 profile/grant 續接或跨 root cap；手動少跑不能代替產品硬限。

後續 run sheet 必須記：新 root／DB／canonical owner、run/profile/price revision 與 digest、
requested/observed tier/status、完整 external spent/hold（含舊 r2 三筆但不能重複加帳），
新 binary/head/hash、唯一 writer、UTC start/expiry（≤24h、不跨月）、stop 與 mount 證據。
主控具名 D 及獨立對帳通過後才可設定付費窗；任何未知支出無保守上界就停。
月 300／400／630 上限仍適用，不能作為跨 root 累計 US$20 的保證。

原 Mini fresh/resume PASS 只屬原模型。Luna 必須先過離線 gate，再各做一次 fresh／
同 native resume 新資格；每次真 dispatch 占新額度，之後完整矩陣為 24 題。
Anthropic r2 資格 FAIL 的停點仍獨立有效，換 OpenAI 不解除雙家停止；重跑另核。
這份文件與離線 fake provider PASS 都不證明真 Luna 可用性、品質或付費資格。

## HCG-REPLY-RECORD：provider 原文與拒絕原因私有紀錄

**此原文區可能含秘密與真key，同UID可讀；不得複製到artifact／報告／對話。**
owner-only 權限不能隔離同 UID 程式。公開回覆、MCP、HTTP、CLI 與 log 絕不含原文；
parent 仍拒交 unsafe 內容給 PAL／主模型。已核准的私存例外不等於允許外傳、渲染、
回灌模型或上傳雲端，也不授權讀 key store 或追蹤其他 credential。

所有 AO 受控 HCG provider attempt 都會記錄，沿 Coordinator constructor 唯一接線；
沒有模型、請求或設定可關閉的開關。啟用前的歷史 attempt 不補記；r2 已清空的原文
不能由本功能恢復，也不得以重新呼叫 provider 或付費 probe 補造。

### 查找

1. 從原回覆取 `usage_ref.attempt_id`（公開欄位不變，沒有新增 diagnostic 欄位）。
   caller 失聯時，改由同一 owner 的 budget journal attempt 或私有 intent 找回。
2. 只在另行授權的本機診斷範圍，到**同一 canonical owner** 的
   `<AO data root>/hcg/delegation/replies/<attempt_id>/` 讀：
   - `body.bin`：HTTP client 取得的 response entity body 原始 bytes（≤1MiB），
     含非 200、非 JSON、parent 因 credential echo 拒交的內容；不重編、不 trim、不遮蔽。
   - `record.json`：只有安全 metadata，見下表。
3. 回報主控只能寫不含原文的安全 code、數值、是否完整與 digest 是否相符等事實。

| 欄位 | 意義 |
| --- | --- |
| `state` | `in_flight`、`not_sent`（附 `not_sent_reason`）、`sent_validation_done`、`sent_validation_not_run`、`record_failed`（附 `record_failure`） |
| `identity` | run／project／session／delegation／attempt、budget attempt key、provider／model／profile id／revision／digest；不記 native／launch／generation |
| `capture` | `outcome`（response／read_failed／no_response）、HTTP status、安全 request id、`complete`、`body_bytes`、`observed_at_least`；完整時 `body_sha256`，partial 時只有 `prefix_sha256`；`parent_refusal` 是 parent 的 stage／code |
| `receipt` | `receipt_sha256` 等於 budget journal 同 attempt 的 `ReceiptDigest`；`canonical` 為 recorded／usage_pending／blocked。用量與金額只在 SQLite canonical receipt |
| `validation` | `accepted`／`not_found`／`rejected`／`not_run`；`failure` 是第一個實際命中的 stage／code 與可靠的 field、index、offset、count、length、limit，未知 offset 為 null |

provider 邊界（http_status、outer_json、response_id、model、choices、request_id、
usage、finish_reason、content、service_tier）、transport、receipt、pal_done、cancel 與
record_write 的失敗都記成 `not_run` 原因，不混成 verifier 的 unverified。digest 與
body 同樣只留私有區，不當匿名資料分享。

### 狀態、凍結與容量

- 同 owner 專用鎖下，dispatch 前原子預留：已存檔案＋殘留原子 temp＋各在途最壞量
  ＋本筆最壞量（2×1MiB＋2×64KiB）不得超過 64MiB；額滿拒新外送，不自動刪舊、不加額度。
  這是診斷資料的邏輯 byte 配額，不保證磁碟永不滿，也不是金錢／token 帳本。
- 正常在途不凍結其他 attempt。可證未送（授權撤回、取消、transport ErrNotSent）
  寫 `not_sent` 並釋放未用預留；已送但取消或 PAL 失敗寫 `sent_validation_not_run`。
- 任何 write、sync、close、rename 失敗：答案不交付（公開仍是 `integration_unavailable`），
  同 owner 後續 admission 凍結；已送 attempt 的有限 capture 與 canonical 帳務照常完成，
  原 hold／名額不變。重啟後看到沒有活躍 owner 的 `in_flight` 一律視為狀態不明而停送，
  不依時間或檔案猜成未送。本 PR 沒有解除凍結的產品指令；處置需另核本機對帳與恢復。

### 保留與刪除

至少保留 30 日，且對帳、獨立 audit／主控核收、hold 與記錄缺口處置完成後，另經使用者
批准才可刪除；產品不做 GC、備份或搬移。搬 owner／刪 root 前，原文、record 與
canonical 資料要一併依上節保全規程處理。
