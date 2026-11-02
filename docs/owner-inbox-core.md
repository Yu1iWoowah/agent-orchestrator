# H-PR1 OwnerInbox core

This is an inactive foundation. The daemon does not construct `OwnerInboxCore`
or register its participants. Ordinary report create/claim/semantic delivery/ACK
retains its current behavior. No owner backfill, lifecycle cutover, provider
routing, native handoff, or input migration is enabled.

Production A32 requires S-G0's real intake participant, second outbound
transaction and shared I/O lane, the `delivery.unknown.defer` consumer,
qualified native authority/Get UX, and every required Claude/Codex Chat/TUI/script
joint cell. SQLite fixtures do not implement S eligibility or qualify a provider.

## Typed transaction contract

| Port | Durable result |
| --- | --- |
| AcceptReport | Validate worker/project; atomically save original report/outputs, versioned source and receipt. Project+sender+caller key deduplicates retries; changed content conflicts. Unresolved routing stays durable and unacknowledged. |
| ClaimBatch | Check explicit owner/epoch/conversation/branch/generation/policy; fix ordered source versions and summary hash. The summary stays independent of user text; equivalent competing plans resolve to the original batch. |
| BindAcceptAndAckAO | Atomically create canonical queued input or verify existing piggyback input, enlist S intake/hold and optional U1 metadata, map input and ACK reports. Missing S participant refuses before input creation; any error rolls back all effects. |
| GetByActionKey | Return original references after response loss or owner changes. Replay never enlists metadata or creates an input twice. Receipt is AO acceptance, not L2 or an outbound permit. |

A receipt returned to a losing piggyback caller acknowledges the report batch only;
it does not acknowledge or discard that caller’s separate user intent. The real
H/S Send integration must preserve that intent without copying the reports again.

The lock order is shared writer → SQLite transaction → H/S/U1 SQL. Public core
methods refuse ambient provider-projection transactions. Callers must not hold
chat/piggyback/private participant locks. Storage alone constructs the narrow,
already transaction-bound intake and metadata participants. They cannot expose
SQL, commit/rollback, reentry, reconciliation, native UI or provider I/O to H.
The only current participant injection entrypoint is compiled in `_test.go`.

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

Binary backout preserves schema, receipts and holds. Down refuses after first
owner/source/batch/action use. Empty up/down/up and refused Down are tested with
the real migration. Neither metadata readability nor a fixture certifies the
old binary/host owner/attention or G0 compatibility matrix; no version floor is
raised. Full H-PR1 integration and activation remain pending S-G0 and joint tests.

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
