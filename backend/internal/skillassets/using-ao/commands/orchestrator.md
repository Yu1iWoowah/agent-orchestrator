# ao orchestrator

Manage orchestrator sessions.

## Syntax

```
ao orchestrator <subcommand> [flags]
```

## Subcommands

---

### ao orchestrator ls

List orchestrator sessions. Aliases: `ls`, `list`.

**Syntax:**
```
ao orchestrator ls [flags]
```

**Flags:**

| Flag | Meaning | Default / Required |
|---|---|---|
| `--json` | Output as JSON | - |

## Examples

```bash
# List all orchestrator sessions
ao orchestrator ls
```

```bash
# List orchestrator sessions as JSON
ao orchestrator ls --json
```

### ao orchestrator replace

Accept a durable handoff for the current project orchestrator. Acceptance returns
before preparation or retirement finishes; it is not proof of completion.

```bash
ao orchestrator replace --project <project-id> --from <source-session-id> \
  --operation-key <canonical-uuid> --json
```

`--project` (`-p`) and `--from` are required. `--operation-key` is optional; the
CLI generates a UUID if omitted. Supply and retain your own key when retrying
after an unknown response: repeating the same key only reads the same operation.
A different key does not override an active replacement. Phase 1 accepts only
Claude Code Chat or TUI sources and a configured Claude Code Chat successor;
unsupported sources are refused before retirement.

The daemon asks the source to write the configured shared handoff document, then
verifies its managed write receipt, freezes it, and publishes the complete
transcript and manifest as immutable evidence. It waits for the source's current
work to settle, without interrupting workers. Source worktrees, dirty files, and
untracked files are retained. The successor must pass a same-native-conversation
stop/resume and fresh acknowledgement before receiving the shared writer lease.

In a local orchestrator Chat, `/handoff` invokes the same daemon operation. It is
an AO composer command, not a message to the provider. The composer retains the
request identity for safe retry and shows an acceptance receipt with a read
command. Send or remove attachments and references first. Worker, cloud, remote,
terminated, and TUI views do not offer this composer command; a local TUI source
may still be submitted through the CLI.

### ao orchestrator replacement get / retry / cancel

```bash
ao orchestrator replacement get <operation-id> --json
ao orchestrator replacement retry <operation-id> --json
ao orchestrator replacement cancel <operation-id> --json
```

`get` reads durable phase, waiting/recovery reasons, safe provisioning-attempt
receipts, and evidence references without advancing the operation. `retry`
continues the same operation and bound successor only when its persisted facts
permit it. A known failure before native launch may retry that same seed; an
uncertain launch never starts a second successor. `cancel` is allowed only before
source shutdown. Neither command is an unconditional restart or rollback.

Pending worker notifications remain visible in `notifying`; acceptance, a spawn
ID, queue-drain acceptance, and provider idleness are not completion receipts.

If `recovery_required` retains a resume intent without a launch receipt, inspect
`resumeEvidence`, `controlAttempts`, `retainedRetry` and `operatorGuidance` in
`get --json`. `retainedRetry.failure` is the cause recorded when that Retry was
accepted, not a later polling failure. Retry only reconciles retained ownership;
absence of a receipt does not prove absence of a live provider. Preserve the
operation/evidence and have an operator verify the native runtime. Do not clear
the intent, edit the database, revive the retired source, or start a fresh
conversation. This interface provides no force-relaunch authority.

On Unix, positive runtime stop proof covers the AO-owned provider process group,
not tools that detached into their own groups, workers, or user-managed services.
Retired AO capabilities remain fenced. On Windows, Job Object cleanup provides
the positive process-tree proof. Host descriptor removal may finish after a
positive stop receipt; that cleanup delay is not a failed provider stop.

### ao orchestrator replacement diagnose / repair

These operator commands are data-only. Diagnose returns raw-row and latest
sealed-snapshot revision/digest pairs without raw bytes or controller secrets.
Repair requires explicit authorization of both pairs:

```bash
ao orchestrator replacement diagnose <operation-id> --json
ao orchestrator replacement repair <operation-id> \
  --expected-revision <row-revision> --expected-digest <row-sha256> \
  --snapshot-revision <sealed-revision> --snapshot-digest <sealed-sha256> --json
```

Only a corrupt row with a trusted snapshot and preservable managed/native facts
can be repaired. A stale CAS, healthy row, unknown fact, or absent snapshot is
refused and keeps the fence. A successful repair reads as `recovery_required`;
it does not operate the provider, release intake, or unfreeze a writer. Only a
positively pre-stop operation may subsequently use Cancel. An uncertain native
launch on a healthy row cannot be cleared through repair.

Do not downgrade to a version that cannot interpret the durable replacement
schema while any operation remains active.

### ao orchestrator replacement intake

```bash
ao orchestrator replacement intake <operation-id> --json
```

Read canonical pending input and worker reports, including attachment references.
Reading does not claim or acknowledge them and does not confirm native delivery.
Inputs arriving after the frozen transcript cutoff remain separate from that
immutable export. Never-dispatched messages transfer with their original
identity and content; source history is retained.

### ao orchestrator replacement prepared

This agent-facing command submits the source's managed write receipt. Use the
operation, attempt, nonce, path, and hash supplied by the daemon's preparation
prompt; do not invent a receipt or manually freeze documents.

```bash
ao orchestrator replacement prepared <operation-id> \
  --attempt <attempt-uuid> --nonce <nonce-uuid> \
  --path <configured-relative-path> --hash <sha256> \
  --write-receipt-id <managed-write-receipt-id> --json
```

It requires the owning source's `AO_SESSION_ID` and `AO_BROWSER_CAPABILITY`.
Submission alone does not mark preparation verified or activate a writer.
