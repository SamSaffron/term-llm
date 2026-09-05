# Process signals

This matrix describes application handlers, not a promise that every operating
system exposes every signal. **Universal safe restart is work in progress, not
implemented by the current PR.** Unix executables now register one process-wide
SIGUSR2 listener at command entry, before argument parsing or configuration.
Without an installed mode owner, signals are coalesced into a logged `deferred`
status and the original invocation continues; normal command completion logs
`finished`. No arguments, stdin, mutations, or tool calls are replayed. These
logs are not the planned authoritative generation registry or a readiness API.
Other unhandled signals retain Go/OS defaults and inherited dispositions.

## Handler matrix

| Mode | SIGINT / SIGTERM | SIGHUP | SIGUSR1 | SIGUSR2 |
| --- | --- | --- | --- | --- |
| Standalone `serve web` | Cancels the serve context and enters shutdown | No reload handler | Stops widget subprocesses when widgets are enabled; their manager remains available | Cooperative self-exec, subject to the restrictions below |
| Web combined with API, jobs or Telegram | Same shared serve shutdown | No reload handler | Same widget handling when enabled | Logged rejection; no restart |
| `serve api`, `serve jobs`, `serve telegram` without web | Same shared serve shutdown | No reload handler | No application handler | Logged rejection; no restart |
| `serve mcp` | Cancels the MCP context and enters shutdown | No reload handler | No application handler | Logged rejection; no restart |
| `serve hub` | No application shutdown signal handler; Go/OS default disposition | No reload handler | No application handler | Logged rejection; no restart |
| Chat/TUI, ask, exec, loop, edit, image/music/embed, benchmark | Existing per-command cancellation/terminal handling, unchanged | Unchanged | Unchanged | Nonfatal deferred; no resume implementation yet |
| Stdio `mcp-server`, MCP clients, maintenance/CRUD, completion, external editors and subprocess wrappers | Existing per-command handling, unchanged | Unchanged | Unchanged | Nonfatal deferred; finishes original invocation without replay |

Deferred semantics are a safety floor, **not** completed long-lived-mode restart
support. Jobs, Telegram, Hub, TUI, proxy/MCP and combined serve still require
mode-specific checkpoint/admission/ownership integration. Even a successful
command completion is not evidence that detached descendants have finished.

USR1/USR2 handling is compiled for AIX, Darwin, DragonFly BSD, FreeBSD, illumos,
Linux, NetBSD, OpenBSD and Solaris. Other platforms compile no-op installers.
The same-PID browser integration has been exercised on Linux. Windows does not
provide these Unix controls. Chat/TUI, ask and exec have the process-wide
nonfatal listener but do not yet have resumable lifecycle owners.

SIGINT/SIGTERM handling is unchanged: the shared serve context registers
`os.Interrupt` and `syscall.SIGTERM`. HTTP shutdown uses a ten-second context;
MCP shutdown uses five seconds. Shutdown is **not** a promise that every external
side effect has completed. Shell SIGHUP/SIGTERM calls target child shell sessions
or process groups, not server configuration reloads. SIGWINCH/SIGCONT terminal
handlers do not implement restarts.

## Supported first slice

SIGUSR2 is deliberately narrower than all the activities a web server can own:

- Standalone, direct-HTTP `serve web`, with writable SQLite sessions and atomic
  response/transcript fencing. No `--no-session`, mixed platforms, Hub routing,
  WebRTC, widgets, or independently owned automatic title providers.
- Set `serve.auto_title: false` in the service's configuration and start with
  `--disable-widgets`. Those defaults are **not** silently changed by the signal.
- Stateful, streaming Responses runs with an explicit turn budget (the web
  request handler supplies one). Native inline-loop providers, MCP runtimes,
  model swaps, steering rushes and skill-run setup are rejected.
- Completed calls to the concrete built-in `read_file`, `write_file`, `edit_file`,
  `glob`, `grep` and `update_plan` tools can precede a restart boundary. Merely
  naming a custom tool `write_file` does not make it safe.
- Shells, custom/child tools, native provider activities, side questions,
  independently owned mutation APIs, and cancelled/failed invocations make the
  current boot ineligible. A shell returning is not proof that it left no
  descendants. Rejection does not kill it or disable ordinary chat operation.

The unsupported-activity record is intentionally conservative and persists for
that boot. Returning from a tool or closing its UI is not used to infer external
quiescence. Enabling more modes/tools requires an ownership and drain protocol,
not just another name in an allow-list. Opening read-only views or acknowledging
attention does not invalidate eligibility; unauthorized requests cannot do so.

## Deployment limitation: Hub, WebRTC and widgets

**This slice does not self-reload a deployed web using Hub reverse routing,
WebRTC and widgets.** Such a process logs a rejection and keeps running. The
local browser evidence below is not evidence for that deployment.

The reverse connector now has a joinable `Stop(ctx)` owner. Cancellation closes
its socket to wake reads/writes; completion joins the reconnect loop, ping loop
and forwarded requests. A deadline reports a failed join and never authorizes
exec. Tests include a transport that observes cancellation but cannot yet return;
Stop must not claim completion. This joins the forwarding client, **not** the
server-side mutation: an HTTP client returning does not prove its backend handler
finished. Existing reconnect tests now join their connectors before cleanup.

WebRTC `peer.Close` still only cancels its context; it does not acknowledge
completion of transport handlers.
Widget `CloseContext` is a one-way, best-effort shutdown that may return on its
deadline and escalates subprocess termination; it is not a reversible drain.
Simply invoking these shutdown methods before exec would not establish safe
quiescence or restore the old service after exec failure.

Supporting the deployed combination needs joinable transport owners, mutation
admission across transport teardown, restartable widget ownership (with an
explicit policy for external effects), and failed-exec rollback. Browser HTTPS
fallback/reconnection must be exercised through a sandbox Hub and WebRTC relay,
including ambiguous mutation delivery: automatic transport fallback is not a
proof of exactly-once request admission. These changes and their combined
browser proof remain required before this PR can claim universal restart.

## Lifecycle

1. **Coalesce and gate.** Signals received by the installed handler before web
   startup/recovery is ready are rejected, not queued. A signal then starts one
   drain generation. Duplicate signals
   during it do not queue another restart. New mutations receive HTTP 503 with
   `Retry-After`; existing handlers are accounted for. Response streams transfer
   ownership to their tracked engine. Read-only event streams remain connected
   until exec. Stop remains available and preempts draining.
2. **Reach a model boundary.** An engine finishes its current provider/tool turn
   and persists the results before parking, without cancelling its tools. It can
   also park before its first provider request once the real input is durable.
   Initial/turn persistence failures cannot acknowledge a safe boundary. A
   naturally completed answer is not automatically continued.
3. **Abort rather than force.** The drain waits up to 20 seconds. Stuck tools,
   approvals and providers are not killed to meet that deadline. Timeout releases
   parked invocations and reopens admission. SIGINT/SIGTERM preempts the drain and
   follows the existing shutdown path instead of restarting.
4. **Prepare a durable batch.** Once every tracked engine is parked or settled
   and other handlers are idle, a bounded five-second checkpoint phase verifies
   the source leases, transcript revisions and absence of pending steering. All
   selected sessions share a random restart UUID. Preparation itself neither
   fences nor cancels the source engines.
5. **Self-exec.** The process executes the executable pathname captured at serve
   startup, using the original argv. It does not run a build, deploy, supervisor,
   shell, or second server process. Successful exec retains the PID and replaces
   memory. The installed pathname must remain present and executable; updating
   the captured file atomically is supported. Repointing a different symlink is
   not an executable-selection mechanism. No sockets are inherited.
6. **Acquire and consume.** The replacement captures and unsets
   `TERM_LLM_RESTART_ID` and `TERM_LLM_RESTART_SERVICE_ID` before commands or tools
   run. It binds the same listener **before** consuming any intent. A stable
   service UUID is bound to the serving argv and startup directory, and each
   boot has a fresh UUID owner. PID reuse is not ownership. SQLite checks the
   service, distinct boot, live source lease, transcript revision and latest
   response fence. Consuming each intent, fencing its source and admitting the
   replacement response happen in one writer transaction.
7. **Continue the same chat.** Recovery injects an internal **developer** message,
   not a fabricated user bubble. The replacement loads the persisted transcript
   and re-resolves the original selected tools from its own registry, retaining
   the remaining explicit turn budget. Completed actions are not dispatched
   again. The model can call tools for the unfinished task. This is a new model
   invocation, not restoration of a Go stack or replay of unknown pending calls.
   The browser's existing reconnect/session reconciliation finds the replacement
   run and preserves local drafts. Stop addressed to a source response follows
   only its explicit restart-continuation chain, never an unrelated newer turn.

Handoff IDs are lookup hints, not authentication. They are neither HTTP inputs
nor a general crash-recovery queue. Without the startup hint, no intent is
implicitly resumed. Consumed, expired, cancelled, advanced or differently owned
intents cannot be admitted again. Lookup expires after five minutes; the live
source lease normally imposes a tighter startup window. If a session cannot be
safely admitted after exec, it remains available for inspection/manual
continuation; failure does not terminate other already-admitted recoveries.

## Failure behavior

If executable selection, checkpointing or `exec` fails, the current process
remains alive. A failed exec discards its unconsumed intents and releases the
original, still tool-capable invocations. If discarding the intent also fails,
the service enters a safe paused state: mutation admission stays closed and
parked work is **not** silently released. Reads and Stop remain available; an
operator must address persistence health or perform an ordinary shutdown.

Scrubbed Hub credentials needed by a replacement are restored only in the
private environment passed to self-exec, using the existing reload helpers.
Restart hints and those credentials are never deliberately installed in the
ambient environment inherited by tool children. Do not export restart hints,
copy them between services, or use them as a manual resume API.

A successful OS exec cannot roll back a replacement that subsequently fails to
start (for example, invalid new configuration). Install a compatible binary and
check health; a failed startup does not authorize replaying an old intent.

## Example

For a **test or explicitly authorized service**, configure:

```yaml
serve:
  auto_title: false
```

Start a supported web instance, for example:

```sh
term-llm serve web --disable-widgets --tools read_file,write_file,edit_file,grep,glob
```

After atomically installing an updated executable at the captured pathname,
send SIGUSR2 to the **verified PID of that service**:

```sh
kill -USR2 "$verified_web_service_pid"
```

Do not use a broad `pkill` pattern. A logged rejection or timeout is not a
successful reload. Check health/version and the continued session; do not resend
user input to simulate recovery. This feature grants no permission to restart
production or to deploy a binary.

## Verification

Focused regression checks:

```sh
go test -race ./internal/llm ./internal/session ./cmd \
  -run 'Test(ModelBoundary|ExecHandoff|WebExec)' -count=1
```

The isolated Linux browser fixture uses a local scripted OpenAI-compatible
endpoint and real built-in file tools: atomic A→B install, SIGUSR2, unchanged
PID, distinct durable boot owners, one pre-restart tool result, working
post-restart tools, same-chat recovery, unsent draft preservation and the next
user turn. It also exercises simultaneous sessions, a quiet reload with no
replayed continuation, and a real shell child verifying that restart hints were
scrubbed. Private fixture homes, transcripts, logs and screenshots are not
repository assets and must not be published.
