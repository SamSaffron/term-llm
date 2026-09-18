---
title: "Reload a running process"
weight: 18
description: "Replace a running term-llm server or chat process with a new build at a safe point, without losing in-flight work."
kicker: "Deploy agents"
next:
  label: Background services
  url: /guides/background-services/
---

Upgrade a running term-llm server or chat process without losing in-flight work by
requesting a safe-point replacement. Send SIGUSR2 directly, use `/reload` in chat,
or target a registered process with the process CLI. Resumable conversations
preserve the current invocation across same-PID exec; completed tools are not
replayed and no synthetic user message is inserted. After 30 seconds, reload
cancels eligible active operations, then waits for their actual settlement.

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
ownership until net/http flushes their response. Clients should reconnect or
retry only rejected, unstarted requests, not blindly replay successful tool calls.

Generated web and MCP bearer tokens survive replacement through private reload
hints, scrubbed from the successor's environment before tool children can inherit
them. Explicit web `--token` and `TERM_LLM_SERVE_TOKEN` settings retain their
normal precedence; the private web token is only the fallback for a generated
token. Same-PID replacement uses the installed pathname, including an invocation
symlink, not a pinned old binary image. Successful exec cannot roll back if the
successor later fails to initialize; readiness waits report failure or timeout
rather than success.

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
acknowledgment, draining active work, cancelling after the grace period, replacing
the executable, starting and verification. Each process finishes independently;
a slow target does not hide another target's result. “Ready” is shown only after
replacement is verified. There is no hidden server deadline. If an explicit CLI
wait limit expires, the error says that the signal was delivered and replacement
may still occur. Redirecting stdout or stderr (or setting `TERM=dumb`) keeps the
existing plain result lines without terminal animation. `--no-wait` also remains
plain.

If a server restart is still waiting after two seconds, its server log reports
outstanding operations grouped by admission source file/line, with counts and the
oldest operation's age. A second snapshot is logged at the cancellation grace
period. These diagnostics do not include prompts, tool arguments, request URLs or
credentials, and do not add output to the TUI or restart progress table. Passive
response subscriptions remain passive when forwarded through the Hub, including
subscriptions opened by POST; their producers retain separate ownership.

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
transcript revisions and fencing remain unchanged. The jobs database additionally
has a single-live-manager owner lock before crash recovery. Private continuation
handoffs do not grant new session-write authority. Web runs retain their existing
lifecycle owner and fencing token.
