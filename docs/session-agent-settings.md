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

The following lifecycle work remains outside reviewer settings delivery:

- Moving a settled workspace back to a canonical branch needs a separate
  dirty/shared/multi-repo ownership workflow. Settlement must not move files or
  native history, so reviewer settings do not add this action.
- Specialized retired-model resume messages need provider-qualified error
  categories. Network/auth failures do not prove retirement. A failed pinned
  resume never substitutes a model or starts a fresh conversation.
- Legacy relaunch validation must retain qualified pinned fallback when fresh
  discovery fails. New-mutation freshness rules cannot replace that fallback.
- Replacing the bulk O(N²) workspace ownership scan changes teardown ordering.
  This PR does not refactor workspace reclamation or its durable writer veto.
- Adapters without an alternate-path resolver remain fail-closed; this PR does
  not infer workspace ownership or add unverified path allocation to them.
- Wiring SaveAndTeardownAll into daemon shutdown changes daemon-wide ordering
  and needs separate lifecycle validation. It remains unused here.
- Claude prerelease version suffixes remain unverified. This PR does not widen
  the TUI version gate without binary compatibility evidence.

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

## Reviewer settings

A reviewer has a stable `reviewId`, its own fixed U3 account binding, and a
preference revision independent of the worker. A new reviewer accepts its own
model, effort and fast choice:

```sh
ao review trigger worker-1 --agent codex --model reviewer-model --effort high --fast off
ao review ls worker-1 --json
ao review settings <reviewId> --json
ao review set <reviewId> --model default --effort high --fast off --if-revision 2
```

`review set` uses `GET/PATCH /api/v1/reviews/{reviewId}/agent-settings`.
PATCH requires `expectedRevision`, a nonempty `settings` patch and an
`Idempotency-Key` header. Model/effort null resets the provider override; fast
is `default`, `on` or `off`. The CLI reads a revision once when `--if-revision`
is omitted and never retries a revision conflict automatically. Usage errors
exit 2; daemon failures exit 1 and preserve the code and request ID.

A switch request with omitted or empty `agentConfig` selects the reviewer
without changing its saved pin, including when selecting the current harness.
It is not a reset. Use the revision-qualified settings PATCH with explicit
model/effort nulls and `fastMode: default` to reset the selection. A nonempty
legacy `agentConfig: {"fastMode":"default"}` explicitly resets the whole
selection to the baseline; the switch never interprets an absent config as
that choice. The Inspector's agent-default model option sends this explicit
reset for a known Codex or Claude pin, clearing effort and fast overrides too.
If a switch fails, the picker returns to the saved worker config (or the pin
when available) while displaying the error. A switch can save the preference
before restore fails; the Inspector therefore refetches that request's reviews,
workspace and original/updated reviewer pins to display the durable result.

Mode and permissions are worker launch settings, outside the selection pin.
Switch saves these fields even for an already-pinned reviewer, including a
switch from another harness. A changed mode or permission setting restarts the
idle target runtime using the saved config; it never changes an active pass or
relaxes the reviewer's fixed adapter sandbox/tool restrictions. Amp mode changes
therefore survive reopening the Inspector and apply to Run review without a
trigger override. Their save does not change the model/effort/fast pin or its
revision. Worker-level model/effort/fast can still reflect an older selection:
the pin is authoritative, including when the picker can only show worker config
as a fallback because its pin query is unavailable.

An omitted or empty harness clears the worker's reviewer override and follows
the current project default. Conflict and pin checks use that resolved target,
not the previously selected override. Clearing the override still persists when
both choices currently resolve to the same harness, including when the worker
already inherits it. Old worker model/effort/fast shadows are cleared; its pin
remains unchanged unless an explicit selection change was requested.
Native history also remains unchanged unless the switch changes worker mode or
permissions and requires the runtime reset described above. Later project-default
changes can still take effect.

The successful receipt is `phase: pinned`, `applyMode: next-pass`. It does not
claim a provider applied the selection. A running pass or queued/running Chat
turn conflicts, without stopping the reviewer or changing that pass. A pass
atomically records its selection/revision when admitted. Recovery uses that
pass snapshot; later project defaults do not replace it or reselect its account.
When an idle live reviewer needs the new selection, the old runtime is stopped
before the next writer launches with the original native history. An admitted reviewer selection is carried explicitly into Chat start/resume,
including empty model/effort resets, instead of being overwritten by the previous
conversation projection. Legacy reviewers without a recorded selection keep their
existing conversation restore behavior. Selecting a legacy reviewer or saving
its reviewer config does not require or backfill a preference. Its first admitted
pass pins that saved config through the normal initialization path. Switching to
or configuring an idle reviewer leaves another harness's active pass and handle
alone; changing the selected active reviewer still returns a conflict. Fixed
reviewer sandbox/tool restrictions remain in force.

Admission of a new pass with model or effort selected first refreshes an
unqualified account-scoped catalog. If discovery fails and returns a stale
last-known-good catalog for the current input fingerprint, admission may use
its covered model/effort choices (user decision, 2026-10-31). The catalog remains
marked stale; it must carry a real past successful discovery. An unlisted custom
model or unknown alias effort is not covered. A known unsupported effort still
uses the settings unsupported error. Explicit fast admission may also use that LKG evidence; fast-on still requires
the adapter and selected model to advertise support. Settings mutation retains
the fresh-success requirement. Fetched or validated timestamps alone, including
an idle authentication-required cache, are not successful evidence. Failure of
this initial qualification rejects admission before saving a preference/pass
snapshot or launching a reviewer. An owner left by failed initial qualification
can retry or change its initial choice once qualification succeeds, provided it still
has no pass or native launch history. This includes non-account harnesses and
retains any established fixed account binding. Repeating the already-resolved
worker model/mode on an unpinned legacy reviewer is also ordinary admission,
including the Inspector's repeated config payload. The Inspector reads a recorded next-pass
pin for its model display. Its picker saves through switch; Run review sends
only the selected harness/rerun intent, never a copy of worker config. An
explicit CLI model/effort/fast override remains a selection change, even if it
matches an older worker config. Only an actual change to an
executed legacy reviewer requires mutation support and a recorded preference.
A legacy native-history restore keeps its compatibility behavior; restoring
that history does not qualify the next pass. Without a native conversation ID,
restore must qualify the selected snapshot or pin as a new launch. A pin alone
does not prove native history exists: preflight can fail after admission saves
the pin and pass. Rejection preserves the pin, pass and fixed account. This also
applies to a running snapshot whose native ID has not yet been recorded and to
Chat-to-TUI fallback that clears native history. A rejected restore leaves an
existing running pass intact. A selection-changing trigger or switch still
conflicts; retry a normal trigger without overrides after qualification is
available. That path reconciles a reviewer proven dead and cancels the stale
run before admitting its replacement. This does not make an unknown runtime
probe proof of death. A launch with no model,
effort or explicit fast choice uses provider defaults without adopting a cached
catalog selection or claiming provider-effective values.

The Chat composer labels these controls **Next review pass**. It keeps a draft
until the daemon accepts a pin, shows conflicts beside the action and preserves
the draft on failure. External updates refresh the saved preference without
rewriting an open draft. Provider-effective settings remain explicitly unknown;
in particular, a Claude fast `on` preference is never displayed as proof that
the live session offers or applies fast. Pin CDC refreshes only the affected
host/review preference, without downloading conversation history.

Managed changes require a current settings-purpose orchestrator grant, a
same-project worker parent and an explicit `agentSettingsPolicy.reviewers`
allowlist keyed by reviewer harness. `agentSettingsPolicy.harnesses` grants
worker permissions only. Actor identity, the sole live orchestrator, project
policy and reviewer owner/account/revision are checked again inside the commit
transaction. Local UI and identity-free CLI retain the A-Q8 manual exception,
audited as `local_operator_unverified`; they still need CAS and provider
validation. The API accepts neither account changes nor permission changes.
Grants expire after 24 hours; they do not renew in a running provider process.

| Surface | Declared support and limits |
| --- | --- |
| Codex / Claude reviewer launch | Own model/effort/fast, with account-scoped catalog and current input fingerprint. Model/effort admission allows a covering last-known-good catalog after refresh failure; settings mutation requires the last successful discovery to remain fresh under the catalog owner’s calendar-day rule; fetched/validated timestamps alone do not qualify. Native fake-wire contracts also retain fixed reviewer permissions. |
| Existing reviewer next-pass mutation | Stable review owner with recorded preference, fixed U3 binding and complete native/generation identity. Active passes conflict. Legacy unknown preferences are not reconstructed from worker settings. |
| Other reviewer launch adapters | Only fields declared by the adapter config spec and accepted by its catalog. Catalog variants alone do not declare effort or fast support. |
| Other harness reconfigure | No generic hot-reconfigure or account binding is inferred from a model launch flag. Missing settings/runtime/account capability remains unsupported or unavailable. |
| Cursor | Exact variant IDs remain model IDs, never a generic effort or fast choice. |
| Amp | Native mode remains distinct from effort/fast. Older launch preferences using its mode in the model slot retain launch compatibility; they do not qualify typed effort/fast. |

Other harnesses can launch with fields declared by their adapter, but currently
lack the fixed U3 account binding required for account-bound next-pass changes.
Changing an existing pinned reviewer's model/effort/fast through trigger
overrides, switch config or the settings API returns
`AGENT_SETTINGS_UNSUPPORTED`, rather than reporting
a temporary catalog outage. This also applies when a trigger override actually
changes the resolved choice of an unpinned legacy reviewer with execution
history. Repeating its saved choice or retrying an unlaunched owner goes through
normal admission and catalog qualification. A legacy switch/config save still
records its selection for the first admitted pass; it does not mutate a pin or
infer an account binding. Worker-level mode and permissions remain saveable via
switch as described above; they are not account-bound pin mutations. No change
is made to an active pass or the reviewer's fixed permission rules.

The additive reviewer migration leaves every old row unchanged, including
worker revisions, reviewer bindings and historical run selections (`NULL`). A
downgrade refuses to erase recorded preferences or pass snapshots. Rollback of
the feature disables reviewer mutation, preserving existing owners and account
bindings. Live-provider and paid-fast compatibility require separate authority;
fake-wire evidence is not a claim of a real paid turn.
