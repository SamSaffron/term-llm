# Session persistence

## Transcript replacement and compaction

A compacted session has two parts: the **archive** (rows before
`compaction_seq`, kept for scrollback and search) and the **active context**
(rows from `compaction_seq` on, the summary plus everything after it).
`LoadActiveMessages` returns the active context, and runtimes hold only that.

| Operation | Replaces | Boundary | Use when |
|---|---|---|---|
| `ReplaceMessages` | the active context | kept | you hold what `LoadActiveMessages` returned, plus changes. This is the default. |
| `ResetMessages` | the whole transcript | cleared, archive deleted | you own the complete conversation: an explicit fresh-history run, or a transcript held complete in memory (Telegram). |
| `CompactMessages` | appends a new active context | moved to the appended rows | applying a compaction (`ApplyCompaction`). |

Rules the store enforces inside the write transaction, so callers never choose
a mode from session metadata:

- No boundary, or a stale one with no rows at or after it, means the whole
  transcript is active. A stale boundary is cleared, matching
  `LoadActiveMessages`.
- With a live boundary, an empty `ReplaceMessages` is rejected
  (`ErrEmptyActiveContext`): it would leave a stale boundary that turns the
  archive active again. Use `ResetMessages` to clear a transcript on purpose.
- A whole-transcript write recounts `user_turns`. A tail rewrite only adds
  user turns beyond those it replaces, never decreasing the count; retained
  compaction-tail copies never count.

Never pass a whole transcript (for example from `GetMessages`) to
`ReplaceMessages` for a session that may be compacted: the archive would be
copied into the active tail. The store cannot tell a stale snapshot from a new
one, so a writer must hold the session (runtime lock or turn lease) between
loading the active context and replacing it.

Contract tests: `transcript_replace_test.go`.

## Steering persistence

`internal/session` owns steering persistence, operation fencing, recovery state, and the session-side ownership boundary. User-facing steering documentation lives in the [usage guide](https://term-llm.com/guides/usage/) and the [web UI and API guide](https://term-llm.com/guides/web-ui-and-api/).

### Ownership and recovery

Server operations are session-scoped and keyed by a distinct stable request ID. Retrying the same ID returns the same immutable source and batch, including terminal results. Do not construct a fallback `/responses` request from rush-owned entries after a transport error.

SQLite stores accepted FIFO sequence, provenance and exclusive ownership, plus an operation ledger with full recovery payload. Initial input, pending-row removal and committed operation dispositions share one transaction. Stop and initial-input authorization compete through a fenced operation CAS. Database/HTTP errors before durable admission do not cancel the source.

Lock order is session admission boundary → runtime steering mutation guard → engine callback mutex. Admission installs the engine freeze before releasing these short locks for database I/O. No provider cancellation or settlement wait occurs under them. The published transition reservation blocks competing starts and session mutations during the gap.

The source's actual worker/persistence completion and active-session ownership release precede replacement admission. On restart, ambiguous operations are reported as blocked (`settlement_unknown`) rather than automatically replaying external work. Authorized state includes the latest operation, and operation lookup retains terminal metadata and recovery payload until session deletion. This has a retention/privacy cost: session deletion removes the ledger via foreign-key cascade.

The terminal manager owns the same freeze and settlement handoff. Without a session store, recovery is process-local: **Pending steering recovery is available only while this session is open.**

## Database upgrade / downgrade

Migration 56 renames the pending table/index, backfills deterministic historical `(created_at,id)` order, and adds monotonic acceptance sequence/provenance/ownership. Historical order backfill is best effort; it cannot recover true acceptance order from timestamp ties. Migration 57 adds the rush ledger. Migrations preserve pending structured payloads and foreign-key cascades. Fresh and upgraded schemas are checked for equivalence.

Migration 64 adds the nullable inherited agent `model_policy` JSON column to sessions. Existing sessions without a policy are unrestricted by inheritance. Branches and handover sessions copy the inherited chain; new child sessions store their ancestors' rules separately from their own agent definition. As with earlier migrations, do not run old and new binaries concurrently against this database.

Migration 65 adds `session_attention.seen_rev` (default 0) and nullable `seen_source`. Seen acknowledgements and manual unseen changes increment the revision only when the watermark changes. Manual unseen clears `seen_at`; it records no acknowledgement of the latest completion. The immediately previous v64 binary rejects v65 for read-write use; its read-only queries still understand the original columns but cannot track manual watermark reversals. Stop older writers before upgrading and restore a backup to downgrade.

Do not run old and new binaries concurrently against this database. Older binaries are not supported after schema upgrade. Back up the database before upgrading; downgrade by stopping the server and restoring the backup, not by a destructive reverse migration.


Migration 66 acknowledges historical unflagged terminal agent generations before
unconditional parent barriers are enabled. It preserves pending eager notices,
`host_restarted` interruptions, and all nonterminal rows. New generations are
eligible regardless of notification origin or `notify_when_done`; collection,
notification, and human-stop suppression remain generation scoped. The v65
binary rejects v66 for read-write use; the table shape is unchanged for read-only
inspection. Stop older writers before upgrading and restore a backup to downgrade.

## Automatic completion ownership (schema 68)

Automatic alerts are owned by a durable logical human turn, not by each protocol
response. Human admission always supersedes the old destination, including TUI
and API inputs without a push target. Only an authenticated v2 window/document
binding grants a destination. Client handoffs require an exact persisted pending
call/result set; child synthesis requires explicitly awaited ID/generation facts
and durable response parentage. Detached work and timestamps grant no authority.
Committed user steering and rush input transfer ownership in the transcript
transaction, never on queueing, withdrawal, or synthetic input.

Finalization settles the turn, acknowledges incorporated child generations,
allocates final attention, and writes an identity-only outbox candidate in one
transaction. Intermediate handoffs/joins and recoverable host retries do not
publish final attention. Dispatch uses fenced claims/results, a 24-hour expiry,
and rechecks read, supersession, subscription status, and bounded owning-window
presence. Presence defers delivery but never marks work read. Attention ACKs
retire exact older candidates transactionally; marking unseen cannot resurrect
an alert. Delivery cannot provide exactly-once external acceptance.

Migration 68 retires historical pending automatic rows lacking proven ownership;
delivered rows and payloads remain intact. The v67 binary rejects the v68 marker
for read-write use. Read-only old queries retain their original columns but do
not understand logical ownership or fenced claims. Stop older writers before
upgrading; downgrade by restoring a backup, not reversing this migration.
Window/document replay tombstones persist to reject stale incarnations. Turn
and steering/rush ownership records follow session deletion via foreign keys.
