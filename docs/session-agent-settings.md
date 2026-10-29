# Session agent settings API

The daemon owns authorization, provider validation and scheduling. Settings are
durable preferences, not evidence that a provider accepted them. Read
`GET /api/v1/sessions/{sessionId}/agent-settings` for the committed revision,
pending selection and qualified `effective` observation. `unknown` and
`not_provided` must remain visible; never substitute `spawnAgentSettings` or
requested fast for an effective value. In particular, Claude may retain fast
`on` while its live model does not advertise fast: the receipt reports
`not_provided` / `provider_not_offered` until a supporting model returns.

## Mutations and polling

`PATCH /api/v1/sessions/{sessionId}/agent-settings` requires `expectedRevision`,
a nonempty `settings` patch and an `Idempotency-Key` header. Omitted patch
fields stay unchanged; explicit empty model/effort and fast `default` reset to
provider defaults. Poll the returned operation using
`GET .../agent-settings/operations/{operationId}`. HTTP 202 means accepted,
not applied. Retrying the same key and payload reads the same operation;
changing the payload, including interruption consent, returns 409.

For Chat, `applyMode: "after-turn"` queues changes until safe dispatch. Legacy
local menus coalesce any **unstarted local** pending operation, including one
created by a typed local PATCH. The superseded operation becomes `cancelled`
with `AGENT_SETTINGS_SUPERSEDED`; pollers must recognize this terminal outcome.
Managed pending operations and local operations already past `queued` return
409 `AGENT_SETTINGS_CONFLICT`, without projecting a new selection. A caller
must keep its draft and show the conflict rather than assume it was saved.
Managed callers may cancel only their own unstarted operations, using the
operation version CAS.

## TUI restart

Codex and Claude TUI changes require explicit `applyMode: "restart"`:

```json
{
  "expectedRevision": 0,
  "applyMode": "restart",
  "stopTurn": false,
  "settings": {"effort": "high", "fastMode": "off"}
}
```

Without restart consent the daemon returns `AGENT_SETTINGS_RESTART_REQUIRED`
before stopping the source. Without `stopTurn: true` it waits for the active
turn to finish. Pending approvals are never answered automatically, including
with interruption consent. Before the first provider action, cancellation
settles both the settings intent and physical handoff. Once started, callers
must follow the journal instead of cancelling a partially applied operation.

The daemon pins account, native conversation and launch identities; checks
fresh account-scoped discovery and native history; proves the old writer
stopped; then strictly resumes that same conversation. It does not switch
provider, fall back to a fresh thread, or write profile settings. Codex reset
wire comes from fresh `config/read` and `model/list` without opening a thread.
Claude uses session-local flags/settings and conservatively verifies the
2.1.205+ session-local fast/effort contract; other versions are unverified and
refused before stopping, not assumed compatible. This is fake-protocol
coverage, not a claim of real-binary compatibility.

Later cold resumes of a committed TUI operation re-prepare the current durable
preference against a fresh baseline and require the same native history. They
do not replay a completed operation's historical source or let legacy omitted
flags revive an old thread override or tier.

A fully unstarted session can save its preference without launching a process.
Started operations own an exclusive handoff shared with account moves,
Chat/TUI switching, restore and reaper. A launch, commit or rollback failure
keeps the original durable preference. Restoration starts only after every
possible recorded writer is proven stopped. Uncertain shutdown or an
unaddressable Create outcome leaves `recovery_required` and fences input,
restore and reaper; daemon startup never automatically creates another writer.
Explicit Resume can restore the original preference when all owned handles
are known and their liveness is conclusive. Missing launch receipts remain
fenced for inspection. Do not clear the journal to force a retry.

## Errors and authority

- `AGENT_SETTINGS_REQUEST_CANCELLED` (409) and
  `AGENT_SETTINGS_REQUEST_TIMEOUT` (503) distinguish request interruption from
  provider failure. Query the operation before retrying an accepted mutation.
- `AGENT_SETTINGS_NATIVE_HISTORY_UNAVAILABLE` (409) prohibits fresh fallback.
- `AGENT_SETTINGS_CAPABILITIES_UNAVAILABLE` (503) means current capability or
  reset evidence is unavailable; it is not proof of unsupported capability.
- `AGENT_SETTINGS_RECOVERY_REQUIRED` (409 on explicit Resume) means no safe
  restoration was proven. Preserve the journal and original preference.

Managed orchestrator grants follow project policy and the same-project worker
scope, excluding self/orchestrator. Grants have a fixed 24-hour lifetime and
are **not renewed in place**; a live reconnect preserves launch identity but
does not extend expiry. Relaunch through the normal lifecycle to obtain a new
grant. Local UI/CLI without a grant retain manual target scope and are audited
as `local_operator_unverified`, not as verified human actions.

## Delivery boundaries and backout

The dedicated `ao session set/settings/settings-operation` commands, restart
consent UI, clear busy-menu 409 notices and effective-value presentation belong
to U4 PR 5. This PR supplies daemon/API semantics without claiming those UI
flows are delivered. Real provider binaries and paid fast are NOT_RUN without
isolated account authorization.

Backout disables the new TUI mutation entry while preserving journal,
preferences and runtime fences. Do not automatically retry a second process.
The migration refuses downgrade while TUI settings handoff history exists.
Keep the coordinator's startup fence support during backout; do not run an
older coordinator against an unfinished handoff or delete its journal.
