# 固定 PAL 的代讀接點（HCG-P1-PR1.02）

本文件記錄離線小接點的成立條件，不宣稱付費 API、帳號／模型可用性或真模型品質已驗證。後續正本收據、來源／引用、預算與掛載均沿同一 `Consumer.DelegateRead` caller；沒有探索型第二 caller。

## 固定輸入與依賴

- PAL 固定 commit `7afc7c1cc96e23992c8f105f960132c657883bb1`，版本 9.8.2；[LICENSE](https://github.com/BeehiveInnovations/pal-mcp-server/blob/7afc7c1cc96e23992c8f105f960132c657883bb1/LICENSE) 與 README 均列 Apache-2.0。安裝保留原授權本文；本 repo 沒有 vendored PAL 或其 binary。
- 本次受測 Python 3.12.3、MCP 1.30.0、OpenAI 1.109.1、google-genai 1.75.0、httpx 0.28.1。全部依賴版本在 `internal/delegate/pal-requirements.lock`，sha256 `799a72fd49b931a9f50152a6342f574efa1eb8eb89ba500e3f3e7f5859357666`；只在隔離 venv 安裝。
- PAL 原 requirements 只有下限，直接解析曾取得 MCP2/OpenAI3/httpx2，而固定 PAL constructor 仍匯入 httpx，實測失敗。改以相容主版本 constraints 解析並固定全樹，沒有修改 PAL。失敗 lock、安裝 log 與相容 lock 都保留在 worker 證據；不能只安裝未釘選的 `requirements.txt`。

## 正式接點與唯一實際嘗試

PAL 的[公開 registry](https://github.com/BeehiveInnovations/pal-mcp-server/blob/7afc7c1cc96e23992c8f105f960132c657883bb1/providers/registry.py) 支援 Custom factory；[ModelProvider](https://github.com/BeehiveInnovations/pal-mcp-server/blob/7afc7c1cc96e23992c8f105f960132c657883bb1/providers/base.py) 公開 `generate_content`。`pal_bridge.py` 註冊固定 Custom 子類別，只重用正式 `ChatTool.execute` 的檔案／prompt／system 組裝，將資料經 typed stdio 交回 HCG。

HCG 產生最終 wire JSON、固定 endpoint／model 與獨立硬 output cap，再呼叫部署端 transport。沒有使用 PAL SDK client、client fallback 或其四次 retry；沒有修改私有屬性、永久 monkey patch、原生 Messages adapter、OpenRouter 或 HTTP 代理 listener。PAL 自身 class 註冊路徑需要環境 URL，故採其正式 factory；不向 PAL 放真 key 或環境 endpoint。

每次 caller 都有獨立 child／registry。helper 最多允許一次 provider 呼叫，Go broker 也只接受一次；失敗拒答，不重試或跨家 fallback。只有 chat 呼叫入口；其他非 essential 工具固定關閉，essential version/listmodels 只留作 PAL 內部 discovery metadata，沒有對外服務原 PAL MCP server。實際啟動／schema discovery 與兩家 caller 的 trace 未出現 AF_INET connect。

Linux bwrap 使用獨立 network／PID namespace，PAL、venv、來源視圖唯讀，只允私有 cache 寫入；清空 child env。非 Linux 缺等價隔離時 unavailable。整體上限 120 秒，caller 取消不等待不合作的 transport；只收束自己的 child。這個小接點不代表完整 O2 或所有模式已驗。

## Profile 與 wire 限制

billing provider 明確為 openai／anthropic，技術類別 Custom 不作帳務來源。endpoint 分別固定第一方的 `/v1/chat/completions`；model 必須明示，拒 auto／latest／default。部署 snapshot 必須明示 context／model max-output 上界，attempt cap 正值且不超上界；未知 cap 拒送，不能由 tool 或來源填 provider／model。

兩家 wire 都明示 `max_completion_tokens`，省略 temperature、top_p、thinking、response_format、tools、images 與 streaming，不依 PAL sampling 分支決定是否限制輸出。[OpenAI 1.109.1 型別契約](https://github.com/openai/openai-python/blob/v1.109.1/src/openai/types/chat/completion_create_params.py) 將該欄定義為包含 reasoning 的 completion 上限；[Anthropic 官方相容契約](https://platform.claude.com/docs/en/cli-sdks-libraries/libraries/openai-sdk) 支援此欄及文字 chat。實際部署的模型能力、上界與價目仍須成為已核不可變 descriptor；此處沒有選定真帳號或自動選最新模型。

最終 wire 固定一個 system 與一個 user；原始來源 basename 不傳入 PAL 視圖。`prompt.txt` 與文件指令的同 caller 測例核對 question 保留、來源只在 user 資料區。Anthropic 的 system/developer 提升合併規則對此固定結構不會提升 user 來源；這不等於已證模型不受語義 prompt injection 影響。

## 可信 receipt seam

Go 在 PAL 投影前核 response model、id／request-id、finish_reason 與數值白名單。prompt／completion／total 都要求明示、非負、合計一致且在已核 cap 內；缺失、衝突或無法解釋者不回答案，保留已送身份與 UNKNOWN，不補零。

OpenAI cache／reasoning 明細只在原始欄位明示且在合計範圍內才保存。Anthropic 相容 details unavailable，保持 nil，不照套 OpenAI 公式。receipt 綁定可信 execution 的 project/session/delegation/attempt、billing provider、精確 profile revision/digest；取消後晚到 receipt 仍交相同 recorder，不回過期成功。

`.02` 用記憶體 recorder seam；正式 ledger／UNKNOWN hold／費用結算在後續子項完成。receipt 失敗不回答案。缺 usage 的合成資料可驗失效行為，但不能當真 API usage／價目已閉合或成本為零。

## 離線證據與停點

`HCG_PAL_TESTS=1` 且明示固定 PAL source／Python venv 時，`internal/delegate` 的聚焦 race 測例驗：兩家正式組裝／獨立 cap／原始 usage／billing 歸因、O1 basename與角色邊界、未知或非法 usage、provider failure 單次且零 fallback、取消不合作 provider、UNKNOWN 收據身份與 provider-specific 明細。測例只用合成 profile/context/transport/recorder，零真 API／真 key。

Q14 判斷只看上述 A 接點能否閉合。正式 provider 接點與這組最小離線 gate 可成立，沒有因此切 B/C；後續完整來源／budget／同 caller 驗收若發現 A 無法滿足契約，仍須停止並回主控。`.02` 成立後才新增 `.01` migration；整個七子項完成且全量通過前不交付功能 PR。真正付費 lab／語料／key／模式批准另授權，缺證據即 blocked，不以 mock 成功當 paid-ready。

## 精確 payload 與 sanitized snapshot（.03）

執行端只接受 AO 具名審核的 `ApprovedScope`，逐檔釘 path／revision／SHA256；root-relative 開檔防越界、拒非 regular、秘密路徑、binary／可疑憑證及 canary，讀取有大小與取消界。這個 deny scanner 不能證任意正文安全；目前驗收只用已核合成資料，真 repo／log 仍需具名 sanitized manifest 與另行授權。來源變更即拒絕，不沿用舊 hash。

PAL 只見 opaque `sNNNN.txt` 唯讀副本，保留原 line mapping，時間固定為 epoch。HCG 依固定 PAL commit 核對 system SHA256、完整 question、來源逐字內容／行號、固定包裝與 final-exchange suffix hash；行號依 `utils/file_utils.py` 至少四欄。使用原生 `MAX_CONVERSATION_TURNS=1` 限制單次對話，沒有接受 continuation／history 的工具參數，也不對外回傳原 PAL continuation。任何包裝或實際注入差異均 blocked，不能用 processed_files 或 contains 代替驗證。

coverage／omissions 由 HCG 從精確 wire 比對產生；來源過大或總範圍過大先回 scope_too_large，PAL 組裝時略過正文則 coverage=none、blocked，不回完整或全範圍 not_found。兩家分別驗過相同 gate；機器組裝成立不代表真模型不受語義注入影響。

## 固定引用回覆（.04）

模型只可提出 `hcg.delegate-read/1` JSON 的 findings／evidence，HCG 拒絕未知欄位與未驗自由文字。每條 finding 指向 evidence IDs，推論另標 hypothesis；HCG 核 source／revision／原始 content hash、1-based range 與原 snapshot bytes 的逐字 quote，再產 quote hash、original relative path、observed_at／machine_checked。模型給 quote hash 時亦須相符；不同換行的 quote 不默認當原 bytes。機器只驗定位／引用，不保證語義支持或沒有漏事實。

固定 AO instruction／question／focus 與 source metadata 分開，最終 wire 仍經精確 gate；原相對路徑只在 AO 映射／回覆，不作 PAL 控制檔名。coverage 及 requested／injected digest 由 HCG 產生，模型自稱 complete 不受信任。not_found 僅適用已完整注入的本次範圍；超出 output_limit 就 blocked 並留 omission，不截斷成看似完整的答案。output_limit 是驗證後 JSON 的 byte 上限（預設 16 KiB，上限 64 KiB）；拒絕回覆的安全 metadata 不作原文 fallback。

## 主機共用預算與收據故障閉環（.05）

所有 project／session 與兩家 provider 共用同一 daemon 管理根下的 dollar journal；獨立 instance 亦遵守同一 file lock／atomic replacement。cap 固定為 OpenAI 300、Anthropic 400、合計 630 美元，最多全主機一個在途；沒有每 project／key／profile 的新 630。每次正式 wire 嘗試先驗不可變 descriptor、已核月期／外部非本 gate 支出與 hold、完整 model input ceiling／硬 output cap／價目，再持久預留；未知任一上界即拒送。報價以 nano-USD 整數向上取整，計入 cache 分桶造成的最壞 rounding 差額。

reserve 後只以短 policy admission 判定 dispatch 授權，不持鎖等待 PAL／provider／canonical recorder。可證未送才 release；可能已送、未知 usage／價格涵蓋、月界／重啟、canonical 入帳失敗均保留 hold，不 TTL 歸零、不自動重送／換家。新月份須明示 host approval 且舊 hold 全閉合；同月外部支出不得降低或重複包含本 gate attempts。這些都是合成聲明與 fixture 驗證，沒有查真帳號或宣稱已觀測外部真支出。

safe pending receipt spool 與活躍 daemon 的記憶體副本只為入同一 canonical ledger 重放，入帳／結算後移除；journal 只留美元／attempt／descriptor／receipt digest，不另存成功 token 帳本。pending 檔寫入也失敗時保留原 worst hold 與記憶體收據、停止新外送；若整機在無法持久寫入時故障，仍以既有 durable hold 為 UNKNOWN，不能假定零或自動恢復額度。晚到合法 usage 即使 caller 已取消仍記原 profile／policy／價目，不回過期答案、不按 current 重新定價。未知或衝突收據不能覆寫 pending 副本。

backend 只經既有 `reporting` 薄公開 package 的固定 typed receipt adapter 接 canonical recorder，沒有新增 token 自報／匯入 HTTP API。非 Linux 缺 PAL 等價隔離維持 unavailable；Windows／darwin 在本機只作編譯驗證。
