# Session agent settings API

The daemon owns authorization, provider validation and scheduling. Settings are
durable preferences, not evidence that a provider accepted them. Read
`GET /api/v1/sessions/{sessionId}/agent-settings` for the committed revision,
pending selection and qualified `effective` observation. `unknown` and
`not_provided` must remain visible; never substitute `spawnAgentSettings` or
requested fast for an effective value. In particular, Claude may retain fast
`on` while its live model does not advertise fast: the receipt reports
`not_provided` / `provider_not_offered` until a supporting model returns.

## CLI and desktop

```sh
ao session settings worker-1 --json
ao session set worker-1 --model default --effort high --fast off --if-revision 4
ao session set worker-1 --fast on --restart --stop-turn --wait --wait-timeout 2m
ao session settings-operation operation-id --session worker-1 --json
```

These commands are HTTP clients. They do not open the database, change provider
profiles or send terminal commands. Omit a field to retain it; `--model default`
and `--effort default` (including `""`) explicitly reset it. Fast is `on`, `off`
or `default`. Account moves remain a separate `ao account use` action. Typed
resets require a daemon implementing this API; an old daemon's error is shown,
never silently converted into a legacy spawn option.

Without `--if-revision`, `set` reads the revision once before PATCH. It never
automatically retries a conflicting mutation. Missing parameters or invalid
flag combinations exit 2. Daemon errors and failed, cancelled or recovery-required
results exit 1, preserving the error code and request ID. Inspecting an existing
failed operation with `settings-operation` succeeds as a read. `--session` is
required outside AO; inside a session it defaults to `AO_SESSION_ID`.

`--wait` polls the accepted operation. A timeout or interruption does not cancel
it; the error includes its ID and a reconnect command. Polling can reconnect to
a restarted daemon via the current run file, without repeating PATCH. A 202
result means queued, never proof that a provider applied the choice.

The header keeps the committed selection separate from the pending target. The
composer's existing menus select next-turn settings, with a separate pending
notice. Inspector's Agent settings includes requested and effective values,
account label, spawn provenance (labelled as initial resolution), restart needs
and recent operation history. Only server-qualified orchestrator actors appear
as orchestrator activities in the timeline. Local operators remain unverified.
Claude fast `on` with no live fast option displays **Not provided**.

The editor captures its revision when opened. External changes refresh the
read-only sections without replacing its draft or the message composer. A 409
stays beside the action with its code/request ID. Managed pending operations or
already-started local operations must finish before another choice; there is no
optimistic claim that a rejected choice was saved. A deliberate retry after an
ambiguous connection failure uses the original payload and idempotency key.
TUI changes show a restart review; interruption consent is a separate, initially
unchecked option. Unstarted pending operations can be cancelled with version CAS.

An accepted request can still fail after queuing. When pending clears with a
failed result, header, composer and Inspector keep a visible failure notice with
its category, stable error code, operation and request IDs. **Review and retry**
opens the original changes against the latest committed revision, preserving
other controllers' untouched fields. It submits a new idempotent request only
after confirmation; replaying the completed operation's old key would only read
that failed result. TUI stop-turn consent is requested again. **Keep current
settings** dismisses the notice locally across those views without changing
preferences or journal history. A later failure appears again; an external
result never resets an already-open retry draft.
The composer also keeps an unaccepted PATCH separate from both committed and
pending settings. While sending, it keeps the confirmed label and briefly
disables that picker, showing only a submitting notice. A rejection preserves
the selection and error/request ID beside the picker. Explicit retry reapplies
only the original changed fields against the latest snapshot, preserving other
controllers' later changes; dismissing keeps current settings. Only a daemon
response may project an accepted choice, with the separate pending notice.
Settings CDC may contain only the session ID. After its authoritative read
changes revision, pending operation or latest result, the transport refreshes
that session's conversation, model and config-option projections. Ordinary
metadata with unchanged settings does not reload a ready Chat. These reads
preserve the mounted conversation and unsent drafts.
Settings writes and cancellation also refresh those projections after their
settings read settles. This does not depend on a later CDC observing a change:
an immediate applied response, or a pending response already applied by the
refetch, may have filled the settings cache before the event arrives. Local host
aliases use the same projection keys; other hosts are left unchanged.
Remote refreshes fetch only the live conversation page and retain loaded history.
A remote read failure leaves the accepted settings mutation successful and uses
the existing conversation refresh error state.

English and Traditional Chinese have explicit UI copy. Other locale catalogs
currently carry explicit English fallbacks for these new keys; they are not
claimed as completed translations. Effective observation reasons are available
in keyboard-accessible disclosures. Local host aliases share one cache/readiness
scope; remote hosts with equal session IDs remain separate.

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
Claude uses session-local flags/settings and verifies a complete
major.minor.patch version >= 2.1.205, including later minor/major releases.
Older or unparseable versions are refused before stopping. This is fake-protocol
coverage, not a claim of real-binary compatibility.

Later cold resumes of a committed TUI operation re-prepare the current durable
preference against a fresh baseline and require the same native history. When
capability discovery or a provider probe is unavailable, they use the last
applied operation's pinned native model/effort/tier instead. Recovery uses that
same applied wire, or the qualified source preimage for an interrupted first
operation. Neither path borrows another account/conversation's evidence or
starts a fresh thread. Fresh discovery remains mandatory for new mutations;
a cold resume never depends on a completed operation's historical source.

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

Kill can terminate a recovery session after stopping every recorded handle. It
settles both journals as cancelled with `AGENT_SETTINGS_SESSION_TERMINATED`.
If a launch receipt is missing, it preserves the workspace and the unknown
outcome: logical termination does not prove physical shutdown. A durable launch
veto then returns `AGENT_SETTINGS_WRITER_UNCONFIRMED` on any later relaunch,
including a forced fresh launch. Daemon startup also settles an already
terminated recovery without launching or repeatedly rebuilding its active
fence. Only conclusive Kill shutdown records a verified termination; abandoning
an unknown writer does not authorize a retry.

`ao session cleanup` and project teardown skip a vetoed session before runtime
or workspace cleanup and report `unconfirmed native writer; workspace retained`.
A live session sharing that path can still be stopped by Kill, RequestKill,
replacement retirement or project teardown. Retention gates file mutation, not
termination: these paths stop the known runtime and record termination, then skip
stash, scripts and workspace removal and log the retention reason. They recheck
under the project workspace gate immediately before any removal. Preparation /
rollback and multi-repo removal use the same ownership scan. Cancel and expiry
hold the project workspace gate through preparation removal, branch deletion and
cleanup bookkeeping. They wait for an in-flight Restore, then recheck ownership;
a newly-live session keeps its files and the preparation's cleanup handle.
Startup cleanup shares the same gate; callers already holding it use the explicit
under-gate helper.

Spawn and restore resolve the adapter's managed path without materializing it.
When an orchestrator's canonical directory has a writer obligation, a new spawn
uses `<prefix>-orchestrator-<session-id>` and a fresh branch instead. Existing
candidates are skipped with bounded numeric suffixes (`-2` through `-100`), so a
filesystem orphan is not silently reused. Candidate checks use `Lstat`: any
existing item, including a dangling symlink, is occupied. Fresh worktree adds
exclusively claim their new directory before Git runs. Failed-add rollback may
remove only that attempt's directory while its identity still matches; existing
or replaced files, symlinks and directories stay untouched. A multi-repo child
may use an empty, non-symlink directory produced inside this call's newly claimed
root checkout (such as a submodule gitlink); a recovered root does not grant that
ownership. On a child claim failure, ordinary spawns reclaim their newly created
worktrees and unchanged creation branches. Retained partial inventory is recorded
before spawn rollback, including failed fresh-path orchestrator spawns. Replaced
items are excluded from cleanup handles. The adapter
validates that alternate managed path and refuses to reattach an existing
allocation; selection, admission and creation share the project workspace gate.
The old worktree and branch remain untouched. Restore/relaunch of a session
recorded on the retained path still returns
`AGENT_SETTINGS_WRITER_UNCONFIRMED`; it never silently moves an existing native
conversation into a new directory. Other paths without obligations are
unaffected. An adapter that cannot project a safe alternate remains fail-closed;
unknown ownership is not treated as permission to launch.
A predecessor sharing that path also cannot reclaim it. Cleanup checks the
active handoff before the veto so terminal intent cannot create a reclamation
window before journal settlement. If writer status cannot be read, cleanup
retains the workspace. Repeated Kill/RequestKill returns 409
`AGENT_SETTINGS_WRITER_UNCONFIRMED` with an explicit still-vetoed message rather
than silently acknowledging cleanup.

For a live TUI restart operation in recovery, Inspector offers **Archive session**
through the same archive flow used elsewhere in the app. Confirmation explains
that this archives the session and removes a clean worktree while keeping its
branch. An unconfirmed writer retains its workspace and launch veto. Failure
preserves the error for an explicit retry and rolls back the optimistic session
list change. This is an archive, not a stop-only action; it never launches a
replacement. Chat recovery does not offer this TUI-specific exit.

For a terminated session with a retained writer obligation, Inspector offers
**Check retained writer**. Its HTTP boundary is
`POST .../agent-settings/operations/{operationId}/settle` with
`{"expectedVersion": 7, "stopIfAlive": false}`. Only local unverified operators
may use it; an agent session identity or grant is refused. Optional stop consent
permits stopping an exactly identified live generation, not an unknown process.
The coordinator serializes with lifecycle and workspace actions and re-probes
the source, target and rollback generations. Only conclusive absence of all of
them permits the version CAS to clear the veto. Unknown, generation mismatch,
timeout or failed stop keeps the veto. The cancelled settings operation and its
original attribution remain unchanged; a separate transactionally written
operator receipt records the stop proof and request ID. No preference, file,
branch or native conversation moves. Restoring afterwards is a separate action.
An adapter that loses a noncanonical runtime handle may be unable to establish
the required generation proof. Settlement then remains unavailable and the
workspace and launch veto stay in place; there is no manual proof override.

## Errors and authority

- `AGENT_SETTINGS_REQUEST_CANCELLED` (409) and
  `AGENT_SETTINGS_REQUEST_TIMEOUT` (503) distinguish request interruption from
  provider failure. Query the operation before retrying an accepted mutation.
- `AGENT_SETTINGS_NATIVE_HISTORY_UNAVAILABLE` (409) prohibits fresh fallback.
- `AGENT_SETTINGS_CAPABILITIES_UNAVAILABLE` (503) means current capability or
  reset evidence is unavailable; it is not proof of unsupported capability.
- `AGENT_SETTINGS_RECOVERY_REQUIRED` (409 on explicit Resume) means recovery
  could not be confirmed. Inspect the operation before retrying; never clear
  the journal or bypass an unresolved writer fence.
- `AGENT_SETTINGS_WRITER_UNCONFIRMED` (409) preserves a terminated operation's
  unresolved physical-writer obligation; logical termination permits no launch.

Managed orchestrator grants follow project policy and the same-project worker
scope, excluding self/orchestrator. Grants have a fixed 24-hour lifetime and
are **not renewed in place**; a live reconnect preserves launch identity but
does not extend expiry. Relaunch through the normal lifecycle to obtain a new
grant. Local UI/CLI without a grant retain manual target scope and are audited
as `local_operator_unverified`, not as verified human actions.

## Delivery boundaries and backout

CLI, header, Inspector and composer integration are U4 PR 5. Renderer/Electron
fake-data coverage is distinct from the coordinator's SQLite/runtime tests and
does not establish real-provider compatibility. Real provider binaries and paid
fast are NOT_RUN without isolated account authorization.

Backout disables the new TUI mutation entry while preserving journal,
preferences and runtime fences. Do not automatically retry a second process.
The migration refuses downgrade while TUI settings handoff history exists.
The operator receipt migration also refuses downgrade once a receipt exists.
Keep the coordinator's startup fence support during backout; do not run an
older coordinator against an unfinished handoff or delete its journal.

## Known limitations

Pinned restoration can replay a model that the current catalog has retired.
The provider may reject the native resume; its error is not specialized to
identify a retired model. AO does not substitute a model or a fresh thread.
The Claude version gate accepts stable major.minor.patch output; prerelease
suffixes remain unverified and are rejected.

Follow-up in PR 6: optionally moving back to a canonical branch after settlement
needs a separate dirty/shared/multi-repo ownership workflow; settlement itself
must not move native history or files. Specialized retired-model resume messages
need provider-qualified error categories; a failed pinned resume must not infer
retirement from a network or permission error, substitute a model, or start a
fresh conversation. Legacy relaunch capability validation also remains separate
from this UI delivery: applying new-mutation freshness rules there would regress
the qualified pinned fallback for stale discovery. Fresh mutations retain their
strict account-scoped validation. The per-session ownership scan's bulk O(N²)
cost, adapters without an alternate-path resolver, and wiring the currently
unused SaveAndTeardownAll helper into daemon shutdown are not broadened here.

TUI settings restart admission uses the orchestrator replacement gate before
creating an operation or interrupting a turn. An active replacement returns 409
`ORCHESTRATOR_REPLACEMENT_IN_PROGRESS`; a retired source returns 409
`ORCHESTRATOR_REPLACEMENT_SOURCE_RETIRED`.

Creation rollback carries the claimed directory identity through Git registration
and status probes. A claimed worktree is rechecked at mutation time, moved into a
private `0700` `.rollback-*` holder and checked again before deletion. An identity
mismatch preserves the data and clears the owned cleanup handle. Private holders
with unverified contents are retained outside the automatic `.discarded` sweep;
normal dirty-worktree refusals still restore the original directory. Claimed
rollback never falls back to Git's unqualified path-based removal.

Creation claims capture directory identity from an open handle before Git runs.
This also keeps the snapshot stable on Windows, where `Lstat` may defer loading
the file ID until a later `SameFile` call. The handle is closed before rename;
identity comparisons still reject replacements and preserve unverified data.

Ordinary multi-repo child failure also carries its creation claim through the
private-holder removal pipeline, including the final check after Git prune.
It never calls the unqualified force-removal fallback. FreshPath with FreshBranch
still conservatively retains a dirty partial root, branch and copied assets;
the failed spawn records that inventory. This pre-existing preservation policy
is not a guarantee that every alternate-path failure leaves no worktree behind.
