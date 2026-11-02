# H-PR1 OwnerInbox core

This is an inactive foundation. The daemon does not construct `OwnerInboxCore`
or register its participants. Ordinary report create/claim/semantic delivery/ACK
retains its current behavior. No owner backfill, lifecycle cutover, provider
routing, native handoff, or input migration is enabled.

Production A32 requires S-G0's real intake participant, second outbound
transaction and shared I/O lane, the `delivery.unknown.defer` consumer,
qualified native authority/Get UX, and every required Claude/Codex Chat/TUI/script
joint cell. SQLite fixtures do not implement S eligibility or qualify a provider.

## Activation prerequisites: report consumers

H source payloads and outputs exist only in `owner_inbox_sources.report_json`;
they are not projected into `reports` or `report_outputs`. Before adding any
non-test caller to `AcceptReport`, every consumer below must read both legacy
reports and H sources, or receive an equivalent H-owned projection. This applies
even to an intake-only caller while claim/input/ACK remains disabled, including
unbound sources whose receipt records `routing_unavailable`.

- **Replacement handoff inventory and fence:** `ReadReplacementIntake` uses
  `ListReportsByProject` and `ListReportOutputs` to inventory pending/claimed
  reports. It currently omits unacknowledged H sources and their outputs.
  `replacement_report_intake_insert` also watches only `reports`, so H intake
  does not advance `intake_revision`. Replacement can therefore freeze while
  silently omitting an H source. Both the inventory and revision fence must
  include H intake; accepting a late H source must invalidate a stale freeze
  decision in the same way as accepting a legacy report.
- **Attention:** `attention_report_created` and `project_attention.sql` watch
  or query only `reports`. H `needs_input`/`stuck` sources currently produce no
  corresponding attention entry. H intake must feed the attention lifecycle
  and its change/revision signal so those requests cannot disappear from view.
- **Reported PR detection:** `ListReportedPRURLs` reads `pr_created` entries
  from `report_outputs`. H outputs are absent, so their reported PRs are not
  detected. The reported-PR consumer must also include H `pr_created` outputs.
- **Report history and summary:** `GetReport`, `ListReportsByProject`,
  `ListReportsBySession`, `ListReportOutputs` and their report/output APIs omit H
  source content and outputs, including project summary and `ao report` history.
  These read surfaces must include H sources through the shared read/projection
  path, with current AO ACK state taken from the guarded source fact rather than
  its intake snapshot.

These are unimplemented activation gates, not properties certified by the core
fixtures. All must be implemented and tested before the first non-test intake;
S-G0 qualification alone does not restore these consumers.
The legacy pending/schedule/claim readers must continue to exclude H sources;
restoring read visibility must not make them eligible for legacy delivery.

## Typed transaction contract

| Port | Durable result |
| --- | --- |
| AcceptReport | Validate worker/project; atomically save original report/outputs, versioned source and receipt. Project+sender+caller key deduplicates retries; changed content conflicts. Unresolved routing stays durable and unacknowledged. |
| ClaimBatch | Check explicit owner/epoch/conversation/branch/generation/policy; fix ordered source versions and summary hash. The summary stays independent of user text; equivalent competing plans resolve to the original batch. |
| BindAcceptAndAckAO | Atomically create canonical queued input or verify existing piggyback input, enlist S intake/hold and optional U1 metadata, map input and ACK reports. Missing S participant refuses before input creation; any error rolls back all effects. |
| GetByActionKey | Return original references after response loss or owner changes. Replay never enlists metadata or creates an input twice. Receipt is AO acceptance, not L2 or an outbound permit. |

`AcceptReport` identity is scoped by project/sender/caller key. Its payload
fingerprint covers only state, note, message and ordered outputs; `CreatedAt`,
`AvailableAt` and `RepeatCount` do not change that caller intent identity.
A same-key retry returns the original source/receipt without replacing its
timestamps, schedule or repeat count. A new intent requires a new caller key.

A piggyback input that loses to another canonical input receives
`ErrOwnerInboxInputNotBound`. Its existing human input remains intact; no alias,
second S intake or ACK is created. The real H/S Send integration must preserve
that intent without copying the reports again.

H source payloads and outputs live in immutable `owner_inbox_sources.report_json`,
with a separate guarded `acknowledged_at` fact. They are absent from `reports`, so
old binary pending, scheduling, claim and restart recovery SQL cannot select them.
New legacy queries also exclude H IDs. Ordinary reports can still be delivered
and piggybacked while H sources remain unchanged and unacknowledged. Direct bind has no
user text or existing turn; piggyback requires both and verifies a canonical input
already created by the human intake path. Mismatched plans fail before a transaction.

A claimed batch retains its original policy, ordered members and summary.
The currently admitted owner can complete that plan after a policy revision bump;
S receives the original `BatchPolicyRevision` and current `Owner.PolicyRevision`.
It must still enforce current eligibility and the later outbound gate. Stale owner
admission fails; existing same-key receipts remain read-only replayable. A new
alias must pass current owner admission before writing an action or CDC.
The real S response when `BatchPolicyRevision != Owner.PolicyRevision` is pending
S-G0 X-ALIGN confirmation. Fixtures prove H's fixed-plan completion path, not the
cross-contract accept/hold/reject policy; that policy must be aligned before use.

The lock order is shared writer → SQLite transaction → H/S/U1 SQL. Public core
methods refuse ambient provider-projection transactions. Callers must not hold
chat/piggyback/private participant locks. Storage alone constructs the narrow,
already transaction-bound intake and metadata participants. They cannot expose
SQL, commit/rollback, reentry, reconciliation, native UI or provider I/O to H.
The only current participant injection entrypoint is compiled in `_test.go`.
Same-context reentry into core methods, context-aware writer locks and report
writers fails immediately. Other legacy Store writers are not certified reentrant;
participants must use only their transaction-bound port, never public Store APIs.

The owner CAS helper is a disabled-core primitive, not the production
spawn/script/writer-lease/candidate/parked/return bridge. Full admission and
projection transactions remain necessary before callers can use it as authority.
No newest-live selection or automatic owner migration is introduced.

## Schema and backout

Migration `20261102125818565_owner_inbox_core.sql` adds owner/source/batch/member/
acceptance/action tables, immutable receipts and source/ACK guards, and a partial
held-turn index for the consistency query. It preserves S0 turn CHECK, guards,
views, FKs, row order and all v1 replacement objects. Original report intake
triggers remain unchanged. CDC is produced by SQL triggers. It adds no S delivery
ledger or U1 metadata tables. No production caller creates managed report rows.

This still-unmerged migration was rewritten in place after `ba8312f0`, without
changing its version ID. A DB used by that draft can retain the old H schema;
Goose will not rerun the revised migration at the same version.
**Any DB that has ever been run with `ba8312f0` must not be reused**, even if its
H tables are empty. Preserve such a DB unchanged for diagnosis and use a fresh
isolated lab DB for current tests. No in-place upgrade, repair or Down path from
that draft schema is qualified here.

Binary backout preserves schema, receipts and holds. Down refuses after first
owner/source/batch/action use. Empty up/down/up and refused Down are tested with
the real migration. Neither metadata readability nor a fixture certifies the
old binary/host owner/attention or G0 compatibility matrix; no version floor is
raised. Full H-PR1 integration and activation remain pending S-G0 and joint tests.

On downgrade to an old binary, H sources remain stored and protected but invisible
to its report/history/summary, reported-PR, attention and replacement-intake
readers; unbound H sources are not delivered by that binary either. Absence from
those surfaces is not proof of deletion, AO ACK or an empty handoff inventory.
Activation/backout qualification must provide explicit UI/`ao` disclosure of this
protection difference and a qualified way to inspect the retained H facts.
The mixed-version probes certify ordinary-report progress and preserved holds,
not H visibility or replacement completeness on the old binary.

TUI timeout after possible I/O must retain original input/attempt/mapping/AO ACK
as unknown/held. This core has no generic release or outbound sender. S must
reconcile the original attempt; timeout, restart, error text and local IDs cannot
authorize another send. Real TUI timeout/defer acceptance is not yet verified.

## PR #64 suggestions

1. General conversation adoption/rollback still encounter held guards. Existing
   branch-reassign and v1 replacement moves already filter queued. Mixed batch
   and cascade lifecycle policies remain integration work; do not move held
   payloads independently or weaken S0. Core does not call these paths.
2. Missing/rebound saved payload fails closed, including qualified delete.
   Preserve original input/holds/history and report storage incompatibility.
   No automatic repair, force-delete or re-registration is added. A managed,
   evidence-backed repair remains a rollout prerequisite for affected cells.
3. Preflight now checks held and active holds in both directions, using a partial
   held index; ordinary actions still check only their target. No benchmark or
   startup speed claim is made.
4. Payload IDs are checked against the turn's current conversation. Complete
   non-held registered moves may remain consistent; a single rebound payload
   invalidates them. Held position changes remain guarded.
5. New API registration rejects payload rowid `-1`. Historical S0 sentinel facts
   remain guarded and tested. Core writers use INSERT, never REPLACE. Raw turn
   rowid/REPLACE outside these writers remains a rollout audit item.
6. The 512 MiB CI step verifies exactly the expected test with `-list` before
   executing its unchanged race test and data volume.
7. S0 first startup builds two payload indexes and scans scoped FKs before/after
   its exclusive rebuild. Cost depends on database size and disk; capacity-test
   seconds are not startup estimates. H adds a partial index, no second rebuild.

Outstanding H-PR1 work includes owner/candidate/v2/parked/attention and DTO/
lifecycle integration, v1/script owner bridges, production participant registration
and consumer migration. H-prefixed tests identify H fixtures, not S acceptance.

## PR #67 review follow-ups

Existing custody registration receipts are looked up before the new-registration
rowid sentinel check; historical sentinel registrations remain replayable and
guarded. AcceptReport also verifies the replay receipt's accept stage.

H sender IDs are immutable provenance, not foreign keys that pin worker deletion.
The remaining owner/project/acceptance references retain NO ACTION behavior to
prevent cascades from discarding durable history. No production caller creates
these facts. An explicit retention
and lifecycle policy, including owner release and qualified input deletion, must
be implemented and tested before activation; adding cascades here would destroy
the receipt/hold evidence this core promises to preserve.

An existing piggyback turn blocked by a v1 replacement fence still fails closed
with OwnerInbox conflict or the original storage error. A typed, retry-safe
replacement-fence classification belongs with the real H/S Send integration,
where intake ownership and retry disposition can be decided together. This core
does not infer a retry or release from an error string.

AcceptReport's contract outbox obligation is not implemented in this core stage;
it requires the production H coordinator, owner resolution and retry disposition,
and remains a pre-activation requirement. The source receipt is immutable history:
`routing_unavailable` describes intake time, not current routing health. Current
routing/alert projection and reconciliation after owner resolution remain future
integration work; consumers must not treat the old safe reason as live status.
