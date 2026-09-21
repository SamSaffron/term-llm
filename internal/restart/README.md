# Restart coordination

`internal/restart` coordinates safe in-process replacement across application modes. User-facing operator documentation lives at [Reload a running process](https://term-llm.com/guides/process-reload/).

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

The optional isolated acceptance harnesses that drove SIGUSR2 against two real
installed builds have been removed. Nothing else covers what they proved, so the
following behavior currently has **no** automated end-to-end verification:

- An in-flight shell tool executing exactly once across replacement, and the
  complete response draining afterwards.
- A becoming B under the same PID, generated credentials surviving exec without
  leaking to tool children, a failed exec leaving the service usable, and the
  real process CLI retrying successfully.
- Combined web/jobs behavior: a concurrent web shell operation and program job,
  terminal job persistence, and full response delivery.
- PTY chat: an active tool, idle replacement, failed-exec terminal recovery, and
  draft restoration.
- The Hello boundary, the 30-second cancellation fallback, replay through the web
  reducer, and symlink-based upgrades.

Reintroduce an equivalent Linux-only proof in Go before claiming those guarantees
again; a replacement must create its own temporary HOME/XDG directories, loopback
server, installed fixture binary, and child PID, and must never target a live
service. Telegram polling cancellation and rollback use a fake API in Go tests;
these are not live Telegram delivery proofs. Hub/reverse and WebRTC have
route/regression tests, not a claimed real deployment upgrade proof.
