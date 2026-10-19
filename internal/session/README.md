# Steering persistence and ownership

`internal/session` owns steering persistence, operation fencing, recovery state, and the session-side ownership boundary. User-facing steering documentation lives in the [usage guide](https://term-llm.com/guides/usage/) and the [web UI and API guide](https://term-llm.com/guides/web-ui-and-api/).

## Ownership and recovery

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
