#!/bin/bash
# 手動換主控：kill 舊主控 → 帶初始 prompt 建新主控。由舊主控以 setsid nohup 啟動，作為它本回合最後一個動作。
# 用法：manual-handoff.sh [--agent H] [--account A] [--model M] [--effort E] [--approval-mode P]
# 未給的 harness／帳號／model／effort 不傳給 ao spawn，由專案設定 orchestrator 決定。
# 步驟與每個判斷的理由見 README.md；腳本只看 kill、spawn 的 exit code，失敗即記 STOP 並停。
set -euo pipefail
umask 077

OLD=${AO_SESSION_ID:?須在舊主控 session 內執行}
PROJECT=${AO_PROJECT_ID:?須在舊主控 session 內執行}
DATA=${AO_DATA_DIR:?須在舊主控 session 內執行}

SPAWN_ARGS=(--approval-mode bypass-permissions)
APPROVED=()
while [ $# -gt 0 ]; do
  case $1 in
    --agent | --account | --model | --effort | --approval-mode)
      SPAWN_ARGS+=("$1" "${2:?$1 需要值}")
      [ "$1" = --approval-mode ] || APPROVED+=("${1#--}=$2")
      shift 2 ;;
    *) echo "未知參數：$1" >&2; exit 2 ;;
  esac
done

mkdir -p "$DATA/artifacts/$OLD/handoff"
LOG=$DATA/artifacts/$OLD/handoff/manual-handoff.log
STEP='start'
log() { printf '%s %s\n' "$(date '+%F %T %Z')" "$*" >>"$LOG"; }
stop() { local rc=$?; log "STOP step=$STEP rc=$rc"; exit "$rc"; }
trap stop ERR
AO=(env -u AO_SESSION_ID -u AO_PROJECT_ID ao)

PROMPT=$(cat <<EOF
你是接手 $OLD 的新主控（手動換主控）。依序做下面四步；任何一步失敗，就向使用者回報失敗的步驟與錯誤並停止，不重試、不做後面的步驟。
1. 複製舊主控 artifact：cp -a --update=none "$DATA/artifacts/$OLD/." "$DATA/artifacts/\$AO_SESSION_ID/"。
2. 記錄設定：harness 取 ao session get "\$AO_SESSION_ID" --json 的 session.harness；帳號、model、effort 依你看得到的來源照實記錄。讀不到的記 UNKNOWN。腳本傳入值：${APPROVED[*]:-（無）}，只供記錄。這一步不比對，也不因此停。
3. 用 ao project documents read handoff/orchestrator-handoff.md --project $PROJECT 讀交接文件，向使用者回報接手、現況、步驟 2 記下的四個值與腳本傳入值；再寫含這些值的接手收據 handoff/receipt-$OLD-<你的 session id>.md（ao project documents write … --project $PROJECT --expected-hash missing）。寫入成功即代表你持有共用資料夾 lease，寫入被拒算失敗。
4. 依交接文件，以你自己的身分向存活 worker 送 FYI，告知你已接手。
EOF
)

log "start old=$OLD project=$PROJECT spawn-args=${SPAWN_ARGS[*]}"
STEP='kill'
"${AO[@]}" session kill "$OLD" --project "$PROJECT" >>"$LOG" 2>&1
STEP='spawn'
"${AO[@]}" spawn --project "$PROJECT" --kind orchestrator --name "orchestrator（接手 $OLD）" --prompt "$PROMPT" "${SPAWN_ARGS[@]}" >>"$LOG" 2>&1
log "done"
