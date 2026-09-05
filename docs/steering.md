# Steering and Steer now

While a resumable conversation is running, **Enter queues steering** for its next safe boundary. Ordinary steering does not cancel model generation or tools. The composer changes from **Type a message…** to **Steer conversation…** without changing your draft.

Accepted guidance remains visible until the engine consumes it. Up from an empty composer selects a pending row; Delete or Backspace removes that row in the terminal. Down past the last row returns to the composer. Selection never changes FIFO delivery order.

## Steer now is a new run, not Stop

When supported, the terminal pending footer offers **Esc to steer right away**, or **Esc to steer all N right away**. The web interface places **Steer now** beside **Remove** on the pending message. With multiple accepted messages, one **Steer all now** button appears beside the last accepted message’s Remove button. There is no steering button in the main composer controls, no keyboard-hint text, and no extra action footer. When Rush is unavailable, its button is hidden. Rush captures *all* accepted, unconsumed guidance, interrupts the source, waits for its persistence and execution to settle, and starts one replacement run with separate user rows and the original message identities. Unsent text and attachments remain in the composer. A message consumed before admission is not sent twice.

Completion menus, dialogs, composition and editor Escape handling take precedence. Escape during a handoff is reserved for that handoff; use the explicit Stop control (or terminal `/stop` / `/cancel`) to stop automatic continuation. The interface reports **Interrupting…** followed by **Starting steered run…**; HTTP acceptance is not proof of completion.

Cancellation does not roll back side effects. Independent background jobs can continue. The replacement receives a developer-context notice warning it to inspect partial effects before repeating work.

## Availability and safety

Rush is provider-independent: it requests the same cancellation as Stop, waits for the source run to settle, then uses the normal conversation continuation path. HTTP, WebSocket, CLI/ACP and wrapped providers keep their existing resume-ID and history-replay behavior; there is no provider allowlist or separate Rush resume API. The run must be stateful and able to accept queued guidance, and server Rush requires durable storage.

Running a foreground tool does not disable Rush. Cancellation is requested first; the replacement waits for actual `Tool.Execute` completion, including tools invoked through native provider bridges. A synthesized cancellation result is not evidence that an abandoned tool stopped. No goroutine is force-killed. Timeout or uncertain persistence/execution blocks automatic continuation and retains the operation payload. Cancellation does not undo shell commands or stop independent background jobs.

Notifications retain their original user-role/message identity and explicit `job_notification` origin. A notification-only queue does not enable user-triggered Rush. An admitted mixed queue transfers the entire FIFO batch; later notifications use the existing durable fallback.

## Ownership and recovery

Server operations are session-scoped and keyed by a distinct stable request ID. Retrying the same ID returns the same immutable source and batch, including terminal results. Do not construct a fallback `/responses` request from rush-owned entries after a transport error.

SQLite stores accepted FIFO sequence, provenance and exclusive ownership, plus an operation ledger with full recovery payload. Initial input, pending-row removal and committed operation dispositions share one transaction. Stop and initial-input authorization compete through a fenced operation CAS. Database/HTTP errors before durable admission do not cancel the source.

Lock order is session admission boundary → runtime steering mutation guard → engine callback mutex. Admission installs the engine freeze before releasing these short locks for database I/O. No provider cancellation or settlement wait occurs under them. The published transition reservation blocks competing starts and session mutations during the gap.

The source's actual worker/persistence completion and active-session ownership release precede replacement admission. On restart, ambiguous operations are reported as blocked (`settlement_unknown`) rather than automatically replaying external work. Authorized state includes the latest operation, and operation lookup retains terminal metadata and recovery payload until session deletion. This has a retention/privacy cost: session deletion removes the ledger via foreign-key cascade.

The terminal manager owns the same freeze and settlement handoff. Without a session store, recovery is process-local: **Pending steering recovery is available only while this session is open.**

## API and rollout

Canonical clients send `X-Term-LLM-Steering-Protocol: 1`; canonical steering routes imply it. Discovery advertises `steering_v1`. Unsupported explicit versions return 400. Projection responses vary by this header; HTTP and WebRTC use the same handler/projection. User text and attachment contents are never vocabulary-rewritten.

- `POST /v1/sessions/:id/steering`: structured ordinary guidance, stable `client_message_id`/`steering_id`, `expected_response_id`, and positive `expected_run_epoch`.
- `DELETE /v1/sessions/:id/steering/:id`: remove pending guidance, not owned or committed input.
- `POST /v1/sessions/:id/steering/rush`: `{request_id, expected_response_id, expected_run_epoch}`; no draft text.
- `GET /v1/sessions/:id/steering/rush/:request_id`: authoritative operation and recovery payload.
- `POST /v1/sessions/:id/steering/rush/:request_id/cancel`: stop handoff/replacement, retaining unconsumed payload.

The release introducing `steering_v1` retains `/interrupt`, the old pending-delete route and unversioned legacy projection for **one release**. New clients normalize old server/history fields once at ingress and hide Rush without canonical capability. Historical `response.interjection` decoding must remain after live aliases are removed; sequence IDs and durable cursors are unchanged. The external Grok `xai-interjection-core` / `x.ai/interject` protocol is not ours to rename.

**Cleanup task for the following release:** remove the live `/interrupt` and legacy pending-delete adapters and unversioned egress projection after checking cached-client adoption; retain historical decoding and migration SQL. Run `bash scripts/check_steering_naming.sh` to enforce the scoped exception list.

## Database upgrade / downgrade

Migration 56 renames the pending table/index, backfills deterministic historical `(created_at,id)` order, and adds monotonic acceptance sequence/provenance/ownership. Historical order backfill is best effort; it cannot recover true acceptance order from timestamp ties. Migration 57 adds the rush ledger. Migrations preserve pending structured payloads and foreign-key cascades. Fresh and upgraded schemas are checked for equivalence.

Do not run old and new binaries concurrently against this database. Older binaries are not supported after schema upgrade. Back up the database before upgrading; downgrade by stopping the server and restoring the backup, not by a destructive reverse migration.

The payload baseline's historical `app-interject.js` entry describes a previous measured asset set; it is not an active chunk name. The current Vite build emits versioned `app.js` and named chunks, including lazy steering actions and queue presentation. Do not rename baseline entries by guessing or relax its budget to mask growth.

## Review and verification notes

The implementation review covered engine settlement, durable ownership, server admission and Stop races, TUI handoff, and browser recovery. Follow-up fixes keep the reservation through initial-input commit, restore recoverable input on failed handoffs, serialize TUI Stop against replacement publication, fence pending deletion by source identity, and resume authoritative operation polling after reconnect. Suggestions to make repeated Escape stop a handoff, automatically resume goals, or release an uncertain settlement barrier were not adopted: those conflict with the safety and interaction contract above.

Verification includes root `make build`, isolated `go test ./...` and `go vet ./...`, race tests for `internal/llm`, `internal/session`, `internal/tui/chat` and `cmd`, and frontend formatting, lint, types, unit tests and production build. Mocked HTTP tests exercise source cancellation followed by exactly one ordered replacement with original client IDs. A context-ignoring tool regression verifies synthetic cancellation cannot certify actual execution settlement. Browser coverage exercises queue selection/deletion, Rush with an unsent draft, repeated Escape and explicit Stop on desktop and mobile; the broader lifecycle suites retain their configured Hub/platform skips.

Production bundle budgets remain unchanged. Provider-independent admission and wrapped-provider cancellation/continuation are regression-tested; cancellation still is not a rollback mechanism.
