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
