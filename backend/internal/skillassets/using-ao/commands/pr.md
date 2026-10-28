# ao pr

透過本機 daemon 合併 PR 或處理 review threads。

## ao pr merge

```text
ao pr merge <pr-number> --pr-url <url> --expected-head <sha>
```

| 旗標 | 說明 | 必填 |
|---|---|---|
| `--pr-url` | 與 PR 編號相符的完整 HTTPS PR/MR URL | 是 |
| `--expected-head` | 你認可的已審查 commit SHA，40 或 64 位十六進位字元 | 是 |

```bash
ao pr merge 42 --pr-url https://github.com/owner/repo/pull/42 \
  --expected-head 0123456789abcdef0123456789abcdef01234567
```

GitLab URL 可包含巢狀群組，例如
`https://gitlab.example.com/group/subgroup/repo/-/merge_requests/42`。

CLI 只將 `prUrl` 與 `expectedHeadSha` 傳給
`POST /api/v1/prs/{id}/merge`，不會呼叫 GitHub、`gh` 或自動查詢最新 head。
daemon 核對已追蹤及 provider 最新的 head，再以指定 SHA 作為 provider 合併條件，
避免合併到審查之後才推送的 commit。

缺少或不合法的參數回傳 exit 2，且不送出合併請求。daemon 錯誤回傳 exit 1，
保留錯誤訊息、code 與 request ID。若收到 `PR_HEAD_CHANGED`，先審查新增的
commit，再以認可的 SHA 重試；不要直接接受最新 head。

## ao pr resolve-comments

```text
ao pr resolve-comments <pr-number> [comment-id...]
```

指定 comment 或 thread ID；省略 ID 時處理所有尚未解決的 review threads。
