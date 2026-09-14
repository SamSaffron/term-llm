# TUI sessions in web + one turn owner per session

Status: implemented (uncommitted) after an astra review of v1 and a group review
of the implementation. Deviations from the text below: the staleness check uses a
store-tracked "own revision" (`SQLiteStore.OwnTranscriptRev`, noted after commit)
instead of per-site bookkeeping; `release` marks the terminal attention seen; the
transcript fence shares the admission/renewal grace term; the TUI cancels its run
when a fenced write reports a lost lease. Production LOC landed above the ~175
estimate.

## Goal

1. TUI chat sessions appear in the web sidebar.
2. Exactly one process may run a turn in a session at a time (TUI process vs web
   server, or two web servers on the same DB). Enforced durably in SQLite, and
   the loser's transcript writes are rejected, not merely "cancelled soon".
3. A TUI never sends the model history it did not load (staleness refusal).

Explicit non-goals: cross-process inbox/steering, live mirroring of a TUI stream
into the browser, automatic TUI reload of web-made history.

## Why this is small: the lease and the fence already exist

Verified against current code:

- `serve_response_lifecycle` (`internal/session/sqlite.go:379`) is a durable
  per-response lease: `session_id`, `state='running'`, `owner_instance_id`,
  `fencing_token`, `lease_expires_at`. FK is to `sessions`, not to any
  web-only table, so synthetic TUI response IDs are fine. The web admits one row
  per response (`cmd/serve_response_run_stream.go:782`), renews every 10s sweep
  with a 30s lease (`cmd/serve_response_run_manager.go:531`), finalizes on
  completion (`:893`), and orphans expired rows after a 5s grace
  (`RecoverExpiredResponseRuns`).
- **Transcript fencing is context-driven and already implemented.**
  `checkpointResponseRunFenceTx` (`internal/session/attention.go:303`) reads
  `ResponseRunFenceFromContext(ctx)`; if a fence is present and the lease row is
  no longer running/owned/unexpired, the write fails with
  `ErrResponseRunLeaseLost` inside the same transaction. Every
  `*WithTranscriptRev` message write calls it (`sqlite_messages.go:136,206,245,408,586,671`).
  Anyone who wraps their run context with `session.WithResponseRunFence` gets
  fenced writes for free.
- **Gap 1:** `SQLiteStore.AdmitResponseRun` (`attention.go:101`) never checks
  whether the session already has a running lease held by a *different* owner.
  Web-vs-web exclusivity today is purely in-memory (`trySetActiveRun`).
- **Gap 2:** the TUI never touches the lifecycle table or the fence. It persists
  the user row in `sendMessage` (`internal/tui/chat/streaming.go:614–631`), then
  `startStream` (`:824`) writes `UpdateStatus(StatusActive)` and finalizes status
  in `MainRunExecution.Finalize` (`:1086`); assistant/tool rows are written by
  engine callbacks with unfenced contexts (`:134–226`).
- **Gap 3:** web transcript mutations that are not response runs (undo/redo
  `cmd/serve_undo_redo.go:77`, compaction `cmd/serve_compaction_handler.go`)
  only check in-memory run state, so a foreign TUI lease is invisible to them.
- Downstream is already wired: the status endpoint treats a foreign running row
  as "active" and deliberately does not hand the browser a response ID to attach
  to (`cmd/serve_handlers_status.go:226`); the store change watcher
  (`cmd/serve_events.go:751`) pushes lifecycle/attention/transcript changes to
  browsers; `errServeSessionBusy` → 409 and the browser restores the composer
  (`cmd/serve_ui_stream.go:52`, `frontend/src/stores/run-engine.ts:700`).
- `/v1/sessions` already returns TUI sessions with `mode` and `origin`
  (`SessionSummary`); they are hidden only by `isSidebarSessionVisible`
  (`frontend/src/components/Sidebar.tsx:771`, added in `320e19c7` / #1076 to
  hide *all* TUI-origin sessions, not just subagent children).

## Changes

### 1. Store: foreign-owner query + guard in `AdmitResponseRun` (~35 LOC)

`internal/session/store.go`:

```go
ErrSessionTurnOwned = errors.New("session: turn is owned by another process")

// Added to ServeResponseLifecycleStore (and its logging wrapper):
// ForeignTurnOwner reports the owner of a live running lease on the session
// held by someone other than ownerID ("" when none).
ForeignTurnOwner(ctx context.Context, sessionID, ownerID string) (string, error)
```

`internal/session/attention.go`: implement as one query shared by the guard and
the web gates in step 4:

```sql
SELECT owner_instance_id FROM serve_response_lifecycle
WHERE session_id = ? AND state = 'running' AND owner_instance_id <> ?
  AND lease_expires_at + <orphanGraceMs> > <nowMs>
LIMIT 1
```

Same `julianday` now/grace expression as `RenewResponseRunLease`, so an
abandoned lease stops blocking at the moment the sweeper would orphan it.

In `AdmitResponseRun`, run the query inside the existing transaction **after**
`nextFencingToken` (which takes the write lock, serializing competing
admissions) and before the `INSERT`; return `ErrSessionTurnOwned` on a hit.

Same-owner rows are deliberately not blocked: in-process serialization already
exists (`trySetActiveRun`, TUI `MainRunManager`), and rush replacement admits
under the same owner after the source settles (`cmd/serve_steering_rush.go:222–266`).

Tests (`attention_test.go`, table-driven): A admits, B admits → sentinel; A
finalizes → B ok; A admits twice → ok; `LoggingStore` wrapper preserves
`errors.Is`; `ForeignTurnOwner` returns A's ID for B and "" for A.

### 2. Web: map the conflict to 409 (~5 LOC)

`cmd/serve_response_run_stream.go` `admitErr != nil` branch (~:795): if
`errors.Is(admitErr, session.ErrSessionTurnOwned)`, wrap as
`fmt.Errorf("%w: another process owns this session's turn", errServeSessionBusy)`.
Existing cleanup (cancel, `mgr.delete`, `onDone`) is already correct.

Test: `cmd/serve_responses_protocol_test.go` — pre-admit a foreign lease, POST
`/v1/responses` → 409 `conflict_error`.

### 3. TUI: acquire early, fence writes, release late (~95 LOC)

New `internal/tui/chat/turn_lease.go`:

```go
var tuiTurnOwnerID = sync.OnceValue(func() string { return "tui_" + session.NewID() }) // per process

type turnLease struct {
    store      session.ServeResponseLifecycleStore
    fence      session.ResponseRunFence // ResponseID, OwnerInstanceID, FencingToken
    sessionID  string
    stop       chan struct{}
    released   sync.Once
}

// acquireTurnLease: nil,nil when the store lacks the capability (noop/read-only).
// Refuses with ErrSessionTurnOwned or errTranscriptStale (step 5).
func (m *Model) acquireTurnLease(ctx context.Context, sessionID string) (*turnLease, error)
func (l *turnLease) context(ctx context.Context) context.Context   // session.WithResponseRunFence
func (l *turnLease) startRenewals(runCtx context.Context, cancel context.CancelFunc)
func (l *turnLease) release(outcome session.ResponseRunState)     // stop renewals, FinalizeResponseRun{FinalRev: store.TranscriptRev}
```

Renewal policy mirrors the web sweeper: renew every 10s with a 5s bounded ctx;
on `ErrResponseRunLeaseLost` **or** any failure with < 12s to known expiry,
call `cancel` (fail closed). Release is best effort with a 4s timeout.

Wiring:

- **`sendMessage` (`streaming.go` before `:614` `AddMessage`)**: acquire. On
  `ErrSessionTurnOwned` → `showFooterWarning("Session is busy in another
  process (web); wait for that turn to finish.")`, composer untouched, nothing
  persisted. Store as `m.pendingTurnLease`. (Astra P1: admission must precede the
  first turn-associated write.)
- **`startStream`**: take `m.pendingTurnLease`; if nil (e.g.
  `resumeAfterReload → startStream("")`) acquire here. Every exit before
  `mainRunManager.Start` succeeds, and a `Start` error, must `release(cancelled)`
  (Astra P1: rejected starts never reach `Finalize`). Do **not** bind lease loss
  to the local `cancel` — `startStream` calls it itself on success (`:1146`).
- **Inside `Execute`**: `runCtx = lease.context(runCtx)` before `runWithAdapter`;
  `lease.startRenewals(runCtx, func(){ m.mainRunManager.Cancel(runSessionID) })`.
  All engine-callback persistence now inherits the fence, so a fenced-out TUI
  cannot commit assistant/tool rows even if it wakes up after a pause. Callback
  errors are still swallowed by the UI, which is acceptable: the guarantee is
  "rejected write", not "pretty error".
- **`Finalize(runErr)`**: `lease.release(outcome)` **before** the existing
  `UpdateStatus`, so a successor's status is not overwritten after ownership is
  gone. Outcome: completed / cancelled (`context.Canceled`/`DeadlineExceeded`) /
  failed. For `*llm.SuspendedError` do **not** release here.
- **`suspendForReload` (`process_reload.go:161`)**: perform the `DiscardPartial`
  `ReplaceMessages` with `lease.context(...)`, then `lease.release(cancelled)`,
  then proceed. The new process re-admits in `resumeAfterReload` with its own
  owner ID and hits the staleness check (step 5) if the web wrote meanwhile.
  (Astra P1: `Finalize` ran before the replacement in v1.)
- Legacy (no run manager) path: same, release after `runWithAdapter` returns.

Process death: no finalize → lease expires at 30s, orphaned by any sweeper at
≥35s; another owner may admit as soon as the predicate in step 1 passes (before
the row is rewritten). No PID files, no filelock.

Tests (`internal/tui/chat`, SQLite store): (a) foreign lease pre-admitted →
`sendMessage` shows the warning, transcript rev / message count / status /
user turns unchanged, composer retains text; (b) normal turn → row goes
running → completed with `final_rev` set and `ListAttention(Running)` empty;
(c) `Start` rejection releases the lease; (d) a write with a stale fence is
rejected (`ErrResponseRunLeaseLost`).

### 4. Web: gate non-run transcript mutations on a foreign lease (~25 LOC)

Helper in `cmd/serve_session.go`:

```go
func (s *serveServer) foreignTurnActive(ctx context.Context, sessionID string) bool
// AsServeResponseLifecycleStore → ForeignTurnOwner(ctx, sessionID, s.responseOwnerID()) != ""
```

Call sites, each returning the existing 409 `conflict_error` "cannot mutate the
transcript while work is active":

- `cmd/serve_undo_redo.go:77` next to the in-memory `activeRunID` check.
- `cmd/serve_compaction_handler.go` after the runtime is obtained, before compaction.
- Audit item (do not gate blindly): any other route that writes through
  `TranscriptMutation*`/`ReplaceMessages` on an existing session. Branch creation
  writes a *new* session and is not gated.

This is a preflight, not atomic with the mutation — the same standard the web
already applies to its own in-memory checks. Atomic gating would require every
mutation to carry a fence and is out of scope.

Test: undo with a foreign lease pre-admitted → 409.

### 5. TUI staleness refusal (~15 LOC)

The TUI builds requests from in-memory history (`buildMessagesForStream`,
`streaming.go:867`) and only reloads on suspension/attach. Track
`m.knownTranscriptRev`:

- set from `m.sess.TranscriptRev` after `reloadMessagesFromStore` /
  `refreshSessionFromStore` (`chat.go:821`) and at initial load;
- set from `store.TranscriptRev` inside `turnLease.release` (the lease was held,
  so that rev is this process's own).

In `acquireTurnLease`, after admission and inside the same call: if
`store.TranscriptRev(sessionID) != m.knownTranscriptRev`, release immediately and
return `errTranscriptStale` → footer: "Session was changed by another process
(web). Use /resume to reload before sending." No watcher, no auto-merge.

Test: web-style append after TUI load → TUI send refused, nothing persisted.

### 6. Sidebar: show TUI **chat** sessions (~3 LOC)

`frontend/src/components/Sidebar.tsx:771`:

```ts
function isSidebarSessionVisible(session: Session): boolean {
  // Parentless one-shot/background runs (ask, jobs, goals) stay hidden; the
  // web should not become a process monitor. Interactive terminal chats are
  // first-class and may be continued from the web when their turn is free.
  return session.origin !== 'tui' || session.mode === 'chat';
}
```

`SessionSummary.mode` is already serialized and typed on the frontend
(`frontend/src/domain/types.ts:212`). Update `components.test.tsx` fixtures:
tui/chat visible, tui/ask hidden, web visible.

Composer UX while a foreign turn runs: status marks the session active without
an attachable response ID (`status-reconciler.ts:287`); a send gets the 409 from
step 2. Add a frontend test for the foreign-running status shape to confirm the
composer does not try to attach. Optional (+10 LOC): a small "terminal" badge
for `origin === 'tui'`.

## Budget

| Area | Files | LOC |
|---|---|---|
| Store query + guard + sentinel | `attention.go`, `store.go` | ~35 |
| Web 409 mapping | `serve_response_run_stream.go` | ~5 |
| TUI lease + fence + wiring | `turn_lease.go` (new), `streaming.go`, `process_reload.go` | ~95 |
| Web mutation gates | `serve_session.go`, `serve_undo_redo.go`, `serve_compaction_handler.go` | ~25 |
| Staleness refusal | `turn_lease.go`, `chat.go` | ~15 |
| Sidebar | `Sidebar.tsx` | ~3 |
| **Total production** | | **~175–180** |

Tests (~150 LOC) are not counted.

## Still not covered (stated, not hidden)

- Live mirroring of a TUI stream into the browser; the browser sees a running
  indicator, then the finished transcript via the existing change watcher.
- Non-atomic web mutation gate (step 4) — preflight only.
- TUI does not auto-reload after a web turn; it refuses and asks for `/resume`.
- Cross-process steering/inbox.
- Integrations not audited for admission (hub/relay, goal runtime, ACP, jobs,
  Telegram). They share `startResponseRun` where they go through the web server;
  anything else that writes transcripts without admission is outside the
  guarantee until audited.

## Verification

```sh
go test ./internal/session -run 'ResponseRun|Attention|ForeignTurn'
go test ./cmd -run 'Responses|Busy|Lifecycle|UndoRedo|Compact'
go test ./internal/tui/chat -run 'TurnLease|Stale|Stream'
make build && go vet ./... && go test ./...
npm --prefix frontend run format && npm --prefix frontend run lint && npm --prefix frontend run typecheck && npm --prefix frontend test
```

Manual: open the same session in `term-llm chat --resume` and the web UI.
1. Send from TUI; send from web while streaming → 409 toast, composer kept.
2. Send from web; send from TUI while streaming → footer warning, composer kept.
3. Let the web turn finish; send from TUI → staleness refusal; `/resume`; send OK.
4. `kill -9` the TUI mid-turn → web can send after ~35s; sidebar indicator clears.
5. Web undo while a TUI turn is running → 409.
6. SIGUSR2 reload of the TUI mid-turn → resumes; if the web wrote in the gap,
   the resume is refused as stale rather than sending a stale continuation.
