# ao project

Manage projects: register repos, inspect, configure per-project settings, and remove.

## Syntax

```
ao project <subcommand> [args] [flags]
```

## Subcommands

---

### ao project add

Register a local git repo as a project so sessions can be spawned in it. The path must be an existing git repository on disk. With `--as-workspace`, the path may be a parent folder containing direct child git repositories; AO initializes/adopts the parent as the root repo and gitignores children.

**Syntax:**
```
ao project add [flags]
```

**Flags:**

| Flag | Meaning | Default / Required |
|---|---|---|
| `--as-workspace` | Register a parent folder as a workspace project (root-as-repo plus direct child repos) | - |
| `--id string` | Project id | Derived by the daemon from the path |
| `--name string` | Display name | - |
| `--orchestrator-agent string` | Default orchestrator session agent | - |
| `--path string` | Absolute path to the local git repo | Required |
| `--worker-agent string` | Default worker session agent | - |

**Examples:**

```bash
# Register a repo as a project
ao project add --path /path/to/agent-orchestrator --name "agent-orchestrator"
```

```bash
# Register a workspace (parent folder containing multiple repos)
ao project add --path /path/to/side-quests --as-workspace --name "side-quests"
```

---

### ao project ls

List registered projects. Aliases: `ls`, `list`.

**Syntax:**
```
ao project ls [flags]
```

**Flags:**

| Flag | Meaning | Default / Required |
|---|---|---|
| `--json` | Output projects as JSON | - |

**Examples:**

```bash
# List all registered projects
ao project ls
```

---

### ao project get

Fetch one registered project.

**Syntax:**
```
ao project get <id> [flags]
```

**Flags:**

| Flag | Meaning | Default / Required |
|---|---|---|
| `--json` | Output project as JSON | - |

**Examples:**

```bash
# Get details for the agent-orchestrator project
ao project get agent-orchestrator
```

---

### ao project rm

Remove a registered project. Aliases: `rm`, `remove`, `delete`.

**Syntax:**
```
ao project rm <id> [flags]
```

**Flags:**

| Flag | Meaning | Default / Required |
|---|---|---|
| `--json` | Output removal result as JSON | - |
| `-y, --yes` | Skip confirmation prompt | - |

**Examples:**

```bash
# Remove a project (with confirmation)
ao project rm agent-orchestrator
```

```bash
# Remove without prompt
ao project rm agent-orchestrator -y
```

---

### ao project set-config

Replace a project's saved configuration. AO reads these settings when it spawns a session. Use flags for common fields or `--config-json` for the full object. `--clear` removes all configuration.

**Warning:** This command replaces the whole config, even when you use focused flags. The CLI silently ignores unknown keys in `--config-json`, so a typo can leave the replacement empty. Its config mirror also omits `effort`; `ao project get --json` cannot show it, and a CLI configuration replacement drops existing effort values. Use Project Settings in the desktop app or the daemon API when you need to preserve or change effort. See the [full-config warning](https://docs.orchestrator.inc/configuration/projects/#update-from-the-cli) before updating project settings from the CLI.

**Syntax:**
```
ao project set-config <id> [flags]
```

**Flags:**

| Flag | Meaning | Default / Required |
|---|---|---|
| `--agent-rules string` | Project-specific standing instructions appended to worker session prompts | - |
| `--agent-rules-file string` | Repo-relative file containing project-specific worker standing instructions | - |
| `--canonical-repo-url string` | Explicit upstream HTTPS repository URL used for PR claims (same provider, host, and port as `origin`) | - |
| `--clear` | Clear all config | - |
| `--config-json string` | Full config as a JSON object (overrides field flags) | - |
| `--default-branch string` | Base branch new session worktrees are created from | - |
| `--env stringArray` | Env var `KEY=VALUE` forwarded into sessions (repeatable) | - |
| `--json` | Output the updated project as JSON | - |
| `--model string` | Agent model override (e.g. `claude-opus-4-5`) | - |
| `--orchestrator-agent string` | Harness override for orchestrator sessions | - |
| `--orchestrator-rules string` | Project-specific standing instructions appended to orchestrator session prompts | - |
| `--permission string` | Permission mode: `default`, `accept-edits`, `auto`, `bypass-permissions` | - |
| `--post-create stringArray` | Command to run after workspace creation (repeatable) | - |
| `--session-prefix string` | Displayed session-id prefix | - |
| `--symlink stringArray` | Repo-relative path to symlink into workspaces (repeatable) | - |
| `--tracker-assignee string` | Issue assignee required for tracker intake eligibility | - |
| `--tracker-intake` | Opt this project into GitHub or GitLab issue intake; inert unless the daemon also has `AO_TRACKER_INTAKE=on` | Disabled |
| `--tracker-repo string` | Provider-native issue repository (`owner/repo` or `group/subgroup/repo`); defaults to the Git origin | - |
| `--reviewer stringArray` | Reviewer harness for worker PRs (repeatable) | - |
| `--worker-agent string` | Harness override for worker sessions | - |

Use `--config-json` for fields without a dedicated flag, including
`trackerIntake.provider`, `autoReview`, `containerReap.disabled`,
`agentConfig.mode`, `worker.agentConfig.mode`, and
`orchestrator.agentConfig.mode`. The full-config replacement warning above
still applies.

**Examples:**

```bash
# Set default branch and model for a project
ao project set-config agent-orchestrator --default-branch main --model claude-opus-4-5
```

```bash
# Replace the complete config; include every setting you want to keep
ao project set-config agent-orchestrator --config-json \
  '{"defaultBranch":"main","agentConfig":{"model":"claude-opus-4-5"}}'
```

```bash
# Set an env var and a post-create command
ao project set-config agent-orchestrator --env "NODE_ENV=development" --post-create "npm install"
```

```bash
# Set worker and orchestrator standing rules
ao project set-config agent-orchestrator --agent-rules "Run focused tests before reporting done." --orchestrator-rules "Delegate implementation work to worker sessions."
```

```bash
# Load worker rules from a repo-relative file
ao project set-config agent-orchestrator --agent-rules-file docs/ao-worker-rules.md
```

```bash
# Enable assignee-scoped issue intake (requires AO_TRACKER_INTAKE=on)
ao project set-config agent-orchestrator --tracker-intake \
  --tracker-repo owner/repo --tracker-assignee alice
```

---

### ao project documents

讀取共用 orchestrator 文件，不依賴來源 session 是否仍存活。`--project` 依序使用明確 id、AO project/session 環境與目前 repository；唯讀命令不取得寫入權。

```bash
ao project documents info --project demo --json
ao project documents read handoff/orchestrator-handoff.md --project demo --json
ao project documents download architecture/diagram.svg --project demo > diagram.svg
```

預設共用目錄為 `<AO_DATA_DIR>/projects/<safe-project-key>/orchestrator/shared`。`info` 回傳實際路徑、handoff 相對路徑、lease 與檔案清單。`safe-project-key` 為 daemon 的跨平台安全編碼；請使用 `info` 的結果，不自行拼接實體路徑。

Project config 的 `orchestratorHandoff.sharedDir` 相對於該 project 的 `orchestrator` 根目錄；`documentPath` 預設為 `handoff/orchestrator-handoff.md`。初始化後變更 shared root 需要明確遷移。

Living link 使用 `ao://projects/{project-id}/shared/{encoded-relative-path}`，在本機 app 開啟唯讀文件；來源 session 終止後仍可讀。遠端／Cloud 文件檢視尚未支援。

只有目前 orchestrator 能受管寫入。命令須在它自己的 AO session 執行，使用注入的 `AO_SESSION_ID` 與 `AO_BROWSER_CAPABILITY`；不可把能力交給 worker、列印憑證，或手動取得其他 session 的 lease。Worker 與一般檢視者保持唯讀。

先讀取目前 hash，再以該前置條件提交本機 UTF-8 草稿：

```bash
ao project documents write handoff/orchestrator-handoff.md \
  --project demo --file /tmp/handoff-draft.md --expected-hash <current-sha256>
# 只有建立新檔才使用 --expected-hash missing。
```

受管文字寫入上限為 2 MiB；`read --json` 回傳 hash，binary 或較大檔案使用串流 `download`。過期 hash、凍結 lease、舊 controller、symlink 與越界路徑都會被拒絕。不要猜 hash 重試覆寫。所有受管 shared 文件修改均走此入口；直接 host shell 修改不在 AO lease 保證內。

一次性導入是明確的 current-orchestrator 操作：

```bash
ao project documents adopt <source-session-id> --project demo
```

只複製同 project、合法 legacy artifact 根下的 `handoff/`、`rules/`、`backlog/` 與 `architecture/`。保留來源與備份、核對相對依賴，不覆寫不同內容的目的檔；部分導入在驗證完成前不可用。完成後重試不會重新匯入 legacy 的後續改動。規則只納管來源文件，不自動套用 project config 或既有 worker。未取得任務授權時，不可對真實資料執行導入。
