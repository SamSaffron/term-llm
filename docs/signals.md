# Process reload (SIGUSR2)

SIGUSR2 requests safe-point replacement of a long-lived process. Resumable
conversations preserve the current invocation across same-PID exec; completed
tools are not replayed and no synthetic user message is inserted. After 30 seconds,
reload cancels eligible active operations, then waits for their actual settlement.
The archived [redesign review](sigusr2-redesign.md) describes an earlier proposal;
this page is the current contract.

## Current behavior

| Invocation | SIGUSR2 |
| --- | --- |
| `serve mcp` (stateless HTTP) | Drain, then same-PID exec of the installed executable. |
| `serve web`, `api`, `jobs`, `telegram`, including combined modes | Suspend supported conversations; settle other operations, then exec. |
| `serve hub` | Drain finite local/forwarded work; passive transports reconnect after exec. |
| `chat` (including `/reload`) | Suspend the visible conversation; settle background work, preserve composer state, reversibly release the terminal, then exec. |
| One-shot commands and connected legacy stdio `mcp-server` | Nonfatal deferred request; finish the original invocation without replay. No replacement is reported. |
| Windows | No SIGUSR2 listener; process replacement/discovery commands are unsupported. |

A signal received during initialization is coalesced and retained until a
reloadable mode binds. A command that never binds simply finishes normally; this
is not reported as a successful reload. Signal handling starts at command entry,
not before the Go runtime or package initialization has run.

### Safe-point and cancellation contract

1. Close admission to new root operations and request a cooperative boundary.
2. Resumable engines settle at a boundary before the next model request or before
   dispatching a newly returned tool batch. For example, `sleep → Hello → next
   tool` may reload after **Hello**, before that next tool starts.
3. Preserve committed history, undispatched calls, provider state, turn budget and
   usage in an owner-private handoff. Finish persistence and release execution
   ownership before exec. The successor continues the same invocation.
4. After **30 seconds**, cancel registered active steps, including supported
   provider/tool execution, ordinary HTTP/MCP requests and jobs. This does not
   abort the reload request. Cancelled jobs remain terminal, not requeued.
5. Join actual execution and cleanup before replacement. Cancellation alone is
   not settlement: a noncooperative tool or interactive shell can still delay exec.
6. Failed exec or resource preparation reopens admission and resumes parked
   conversations in the old process. `last_error` remains separate from `ready`.

There is no automatic server deadline that abandons restart, and CLI wait limits
only stop waiting. Completed effects are never automatically replayed. An
interrupted tool result is retained rather than rerunning the tool; incomplete
model output is discarded before continuing from committed history.

Continuation applies to web response runs, visible TUI conversations and Telegram
replies. Detached TUI runs and nested helper engines do not export independent
checkpoints: they finish or are cancelled. Inline providers receive a cooperative
next-tool-result flush request; forced cancellation at an unprovable inline
boundary is terminal rather than fabricating safe continuation. Tool batches are
checkpointed before dispatch, not between every sequential call within a batch.

Web continuation preserves the response ID **and epoch**, replay sequence, and
original idempotency claim. A matching POST retry returns the existing response;
a changed request with that key is still rejected. If an individual response
cannot restore (for example, its provider was removed or its lease was lost), it
is retained as a failed response without preventing other runs or the server from
starting. Terminal persistence still uses the original owner/fence and cannot
overwrite a recovered orphan. Invalid private handoff envelopes remain fatal.

MCP's HTTP listener stays open until exec. Failed exec therefore does not require
rebinding the port or reconstructing the server. New connections becoming active
during drain are closed before dispatch; already-active connections retain
ownership until net/http flushes their response. Clients should reconnect/retry
only rejected, unstarted requests, not blindly replay successful tool calls.

Generated web and MCP bearer tokens survive replacement through private reload
hints, scrubbed from the successor's environment before tool children can inherit
them. Explicit web `--token` and `TERM_LLM_SERVE_TOKEN` settings retain their normal
precedence; the private web token is only the fallback for a generated token. Same-PID replacement
uses the installed pathname, including an invocation symlink, not a pinned old
binary image. Successful exec cannot roll back if the successor later fails to
initialize; readiness waits report failure/timeout rather than success.

### Repeated signals and exec

Signals during a drain are coalesced. Immediately around kernel exec, SIGUSR2 is
ignored: unlike a caught handler, this disposition survives exec and prevents a
second signal from killing the replacement before its listener is installed.
Those boot-window signals are coalesced away, not queued for another replacement.
A failed exec restores the old listener. INT/TERM/HUP/USR1 dispositions are not
changed by this mechanism.

## Process discovery and targeting

```sh
term-llm process list
term-llm process list --json
term-llm process restart PID
term-llm process restart-all
```

The CLI is Linux-only and uses procfs plus pidfd targeting; it has no unsafe
numeric-PID fallback. The default is to wait until replacement is verified.
`--timeout DURATION` explicitly limits only the CLI wait; `0` means no limit.
After SIGUSR2 is delivered, exiting the waiter does not cancel the pending restart.
`--no-wait` confirms signal delivery only, explicitly not replacement readiness.
A deferred mode is an error, not a successful restart.

In an interactive terminal, `restart` and `restart-all` immediately show a table
of the selected processes, with PID, mode, elapsed seconds and lifecycle status.
The table refreshes every second and on phase changes: identity checks, signal
acknowledgment, draining active work, cancelling after the grace period, replacing the executable, starting and
verification. Each process finishes independently; a slow target does not hide
another target's result. “Ready” is shown only after replacement is verified.
There is no hidden server deadline. If an explicit CLI wait limit expires, the
error says that the signal was delivered and replacement may still occur.
Redirecting stdout or stderr (or setting `TERM=dumb`) keeps the existing plain
result lines without terminal animation. `--no-wait` also remains plain.

If a server restart is still waiting after two seconds, its server log reports
outstanding operations grouped by admission source file/line, with counts and the
oldest operation's age. A second snapshot is logged at the cancellation grace
period. These diagnostics do not include prompts, tool arguments, request URLs
or credentials, and do not add output to the TUI or restart progress table.
Passive response subscriptions remain passive when forwarded through the Hub,
including subscriptions opened by POST; their producers retain separate ownership.

Each process publishes one owner-private JSON file beneath
`$XDG_RUNTIME_DIR/term-llm-processes` (or the user cache directory if unset).
Records contain PID, OS start identity, per-exec UUID, executable/build identity,
mode, current phase, and last attempt/error. No argv, tokens, prompts, or drafts
are stored there. Publication is advisory and asynchronous, with a short grace
period to avoid registry I/O for very short commands. SIGUSR2 does not depend on
successful registry publication.

Listing validates OS identity and removes stale records. Restart-all snapshots
once and excludes itself. A verified restart requires the same process identity,
a new exec UUID, the intended installed build, matching mode, and `ready` state.

**A process file is not session-write authority.** Existing session leases,
transcript revisions and fencing remain unchanged. The jobs database additionally has a single-live-manager owner lock before crash
recovery. Private continuation handoffs do not grant new session-write authority. Web runs retain their existing lifecycle owner and fencing token.

## How to add a mode without another state machine

`internal/restart.Coordinator` owns admission, boundary requests, grace cancellation, rollback and status.
A mode binds one replacement callback only after all its components are ready.
Combined modes must bind once, not once per component.

```go
ctx, release, err := coordinator.Enter(ctx)
if err != nil {
    return err // no operation effects have started
}
defer release() // after effects, persistence, notification and cleanup
```

Before launching work that may outlive its caller:

```go
childCtx, releaseChild, err := restart.Child(ctx)
if err != nil {
    return err
}
go func() {
    defer releaseChild()
    run(childCtx)
}()
```

Children must acquire ownership **before** the parent releases. A stale parent
context is rejected. Libraries inherit ownership from context; they do not reach
for the process-global coordinator. Calls without reload ownership are unchanged.

`Coordinator.Handler` accounts for HTTP handlers and propagates ownership.
`Coordinator.HTTPConnections` additionally accounts for HTTP/1 response flushing;
handler return alone is insufficient. Passive streams and hijacked/stateful
transports require explicit ownership adapters, not URL/method heuristics. `HTTPTransport` supplies the general web/Hub transport adapter, while producers
use explicit root admission.

### Mode adapters

- Web/API retains detached responses, commits, skills, side questions, automatic
  titles and completion delivery through their real completion. Explicit controls
  remain available during drain. New user operations are rejected before effects.
- Jobs acquires ownership before claiming a run and retains it through terminal
  persistence and notification. Queued jobs stay queued; reload never requeues an
  already-started job.
- Telegram transfers its next update offset, chat history and pending engine
  continuations. Resumed replies reuse the existing message and do not insert
  another user turn; pending replies launch before polling more updates.
- Chat reserves asynchronous commands when emitted, not when eventually run.
  Presentation timers and passive event waiters do not block idle replacement.
  Composer text, attachments and completed side-question state survive. Persisted
  conversations resume from storage; no-session conversations use the state file.
- Hub/reverse forwarding accounts for request completion, while explicit passive
  event subscriptions and reverse connections reconnect after successful exec.
  WebRTC requests use the same operation admission as HTTP requests.
- Idle MCP subprocesses and widgets are stopped and joined only after quiescence.
  Failed exec restores MCP availability; widgets remain available for lazy start.
  An interactive shell is owned work and must exit before the pending restart can proceed.

The handoff environment hint is scrubbed at startup. State files are private to
this user and removed after consumption or rollback. A crash before consumption
can leave a private temporary file; it is not a durable execution journal.

## Verification

Unit/race tests cover root/child ownership, cancellation versus settlement,
coalesced startup signals, timeout/retry, owner unbinding, HTTP response flushing,
real signals during replacement boot, and signal restoration after failed exec.
Registry tests cover stale identities, private directories, publisher cleanup,
real same-PID exec and stale-instance rejection; fixtures are umask-independent.

The optional isolated acceptance proof uses two real builds:

```sh
go build -ldflags '-X github.com/samsaffron/term-llm/cmd.Version=reload-A' -o ./reload-a .
go build -ldflags '-X github.com/samsaffron/term-llm/cmd.Version=reload-B' -o ./reload-b .
python3 scripts/test_sigusr2_reload.py --binary-a ./reload-a --binary-b ./reload-b
python3 scripts/test_sigusr2_modes.py --binary-a ./reload-a --binary-b ./reload-b
python3 scripts/test_sigusr2_chat.py --binary-a ./reload-a --binary-b ./reload-b
# Prove the Hello boundary, 30-second cancellation, and failed-exec continuation:
python3 scripts/test_sigusr2_safe_point.py --binary-a ./reload-a --binary-b ./reload-b
python3 scripts/test_sigusr2_safe_point.py --binary-a ./reload-a --binary-b ./reload-b --cancel
python3 scripts/test_sigusr2_safe_point.py --binary-a ./reload-a --binary-b ./reload-b --fail-exec
# With frontend dependencies installed, also validate replay through the web reducer:
python3 scripts/test_sigusr2_safe_point.py --binary-a ./reload-a --binary-b ./reload-b --check-client
# Also verify generated bearer auth survives exec without leaking to tool children:
python3 scripts/test_sigusr2_safe_point.py --binary-a ./reload-a --binary-b ./reload-b --check-auth --check-client
# Also exercise a symlink-based upgrade:
python3 scripts/test_sigusr2_reload.py --binary-a ./reload-a --binary-b ./reload-b --symlink
```

It creates its own temporary HOME/XDG directories, loopback server, installed
fixture binary and child PID. It verifies an in-flight shell tool executes once,
the complete response drains, A becomes B with the same PID, generated credentials
survive without leaking to children, failed exec leaves the service usable, and
the real process CLI can successfully retry. It never targets a live service.

The combined-mode proof checks a concurrent web shell operation and program job,
terminal job persistence, full response delivery, same-PID A-to-B replacement and
failed-exec retry. The PTY proof checks an active tool, idle replacement, failed
exec terminal recovery and draft restoration. Telegram polling cancellation and
rollback use a fake API in Go tests; these are not live Telegram delivery proofs.
Hub/reverse and WebRTC have route/regression tests, not a claimed real deployment
upgrade proof. Linux is required for the process-discovery acceptance scripts.
