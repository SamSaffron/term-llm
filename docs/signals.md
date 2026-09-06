# Signals and planned process replacement

SIGUSR2 requests a planned self-exec; it is not configuration reload or permission
to replay a command. Successful exec keeps the OS PID and generates a fresh
executable-instance UUID. The installed executable is resolved before replacement;
resume state stays in SQLite, with only a random lookup ID in the replacement
environment. Startup removes that hint before launching tools. Argv does not grow
`--resume` flags.

## Current implementation scope

This PR remains work in progress toward process-wide support. Do not describe
nonfatal signal handling as successful restart in an unsupported mode.

| Mode | Current SIGUSR2 behaviour |
|---|---|
| Web / API | Drain admitted work, checkpoint, self-exec, resume eligible responses. Default natural grace is 10 seconds; then cancel through the existing cancellation path and join actual execution before resuming. |
| Jobs, alone or alongside web/API | Pause scheduler/worker admission and account for running jobs and completion notifications. Programs finishing within grace are not replayed. After grace, running programs use ordinary durable cancellation and their actual exit gates replacement; never-started claims remain queued. Arbitrary programs are not automatically replayed. Persistent ordinary LLM jobs checkpoint and resume the same run/session after forced cancellation; failed exec reclaims the checkpoint in the original process. Progressive/native-provider parity and no-persistence handling still need verification. |
| Chat/TUI | Uses the existing `/reload` lifecycle through the UI message loop, with a 10-second grace and normal interruption. Restores foreground session, draft and attachments through SQLite, preserving argv. Foreground interrupted tasks receive an internal continuation, not a fabricated user message. Background-session continuation remains incomplete. |
| Web reverse Hub / WebRTC transports | Requests use the same admission fence as direct HTTP. Passive event streams do not block replacement. Sockets close on exec and clients reconnect; current-source combined-transport browser proof is still required. |
| Web widgets | Reuses StopAll with verified child exit. The manager remains available for lazy restart if exec fails. No success is claimed when child exit is unacknowledged. |
| Standalone Hub | Drain local mutation handlers; after 10 seconds cancel and join them before same-PID self-exec. Remote work remains node-owned. Sandbox dashboard and replacement proof passes. |
| MCP HTTP (`serve mcp`) | Drain full mutation responses and actual tool execution, including detached descendants. After 10 seconds cancel and join before exec. Clients reinitialize after replacement; the generated bearer token is preserved. Active-shell sandbox proof passes. |
| Telegram, stdio MCP, other long-lived commands | Early listener handles SIGUSR2 nonfatally, but complete lifecycle/recovery adapters are not yet implemented. |
| Short-lived maintenance/CRUD/one-shot commands | Deferred without replay. Natural completion does not execute the command again. |

Native inline provider loops, MCP-owned resources and some independently owned
mutation APIs still have explicit restart exclusions. Those are unfinished
ownership integrations, not a universal restart guarantee.

## Other signals

- SIGINT/SIGTERM retain existing shutdown/cancellation behaviour. Web/API/jobs and
  Telegram use the shared serve shutdown context. TUI restores terminal ownership;
  a raw-mode Ctrl+C key remains a UI event distinct from an OS signal.
- SIGHUP has no new configuration-reload contract. Do not send it expecting reload.
  Tool-child HUP cleanup is not a serving reload handler.
- SIGUSR1 retains widget-stop behaviour when widget support installs its handler.
- Standalone Hub uses graceful SIGINT/SIGTERM HTTP shutdown; shutdown takes
  precedence over a pending process replacement.
- The process signal dispatcher is installed before command configuration on Unix.
  Duplicate requests coalesce at their lifecycle owner. Unsupported modes retain
  the original invocation rather than dying or blindly repeating side effects.
- Windows has no Unix SIGUSR2/self-exec support. Linux is the tested discovery and
  pidfd-targeting platform; other Unix process-discovery support is not implemented.

## Grace, cancellation and safety

For response-owning web/API and foreground TUI work:

1. Stop new admission and allow up to **10 seconds** to reach a durable boundary.
2. If grace expires, authorize the internal restart interruption before cancelling.
3. Use normal steering-style cancellation and wait for actual execution to exit.
   A synthetic cancellation result is not proof that Tool.Execute returned.
4. Seal the interrupted transcript checkpoint, then exec and admit continuation.
5. A further bounded settlement wait refuses replacement if cleanup does not finish.
   No SIGKILL of the main process is used to fabricate a clean checkpoint.

User Stop differs from internal restart cancellation: Stop revokes automatic
continuation. Cancelled sources can only be resumed through a prepared and sealed
restart intent; unrelated cancelled, superseded or unsealed sources are rejected.
On recovery the model is told that interrupted external effects may have happened
and must be checked before repetition. Tools are available for remaining work.

The engine reuses its execution/steering freeze and actual-tool lifetime accounting;
there is no ordinary-tool name allowlist. Concurrent/child execution must settle,
even when its caller already received cancellation. Program/native subprocess
cleanup is a separate ownership responsibility, not something inferred from a name.

A failed exec does not count as successful restart. A parked invocation can be
released; an already cancelled invocation remains interrupted. Failure to discard
an intent or verify execution settlement keeps the affected restart quarantined
rather than reopening unsafe execution.

## Explicit handoffs

Web response handoffs bind restart ID, logical service identity, source boot/fence,
session and transcript revision. Acceptance and replacement-run admission are one
SQLite transaction. A grace-expired interruption must first have been prepared
while the source was running, then sealed only after cancellation/persistence
settled. Source-ID Stop follows accepted replacement edges.

TUI command handoffs use one-shot private SQLite metadata with source instance,
service/argv/cwd identity, foreground session revision, and serialized UI state.
They are consumed by a different instance, not selected through a PID match.
A missing environment hint never resurrects old unfinished sessions.

The restart lookup hint is not a credential or authorization. Existing Hub
credential hand-back and auto-generated MCP HTTP bearer tokens use separate
exec-only environment entries, removed at startup before tool children launch.
Handoff claims remain synchronous correctness operations; they are not moved into
the advisory registry publisher. Admission is at-most-once, not a claim of exactly-once
external side effects or guaranteed completion after a second crash.

## Operator commands (Linux)

```sh
term-llm process list
term-llm process list --json
term-llm process restart 123 --timeout 2m
term-llm process restart-all --timeout 2m
```

Only binaries publishing validated owner-private records are discovered. Older
binaries and arbitrary command lines are not scanned and signalled. Records live
under `$XDG_RUNTIME_DIR/term-llm-processes`, falling back to the user's cache dir.
They contain PID, kernel boot/process-start identity, instance UUID, build, mode
and last reported lifecycle phase—not argv or tokens.

Publication runs in a goroutine after a **25 ms** grace period. Short commands
exiting before filesystem work begins do not wait for it. Discovery is eventually
consistent and status is advisory, not a live health probe. List validates ownership,
PID reuse and zombie state; stale records are removed under the publisher lock.
The publisher owns both writing and cleanup so it cannot write after its cleanup.

Restart opens a Linux pidfd and revalidates the snapshot before SIGUSR2. It waits
for a new instance, the installed Go build identity, matching mode and reported
ready state. pidfd readiness—not a transient procfs read failure—is authoritative
for death. There is no unsafe numeric-kill fallback when pidfd is unavailable.

`restart-all` snapshots once, excludes itself, requests concurrently and reports
ordered per-process outcomes. Unsupported/deferred outcomes are errors, not silent
success. `--no-wait` reports only signal delivery. Nothing here installs a binary;
install it atomically first, then request replacement.
