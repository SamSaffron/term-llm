---
title: "Terminal-host lifecycle"
weight: 10
description: "Publish visible chat state safely to Herdr, cmux, tmux, Zellij, and other terminal hosts."
kicker: "Integrations"
aliases:
  - /guides/herdr/
next:
  label: Shell integration
  url: /guides/shell-integration/
---

`term-llm chat` can publish the lifecycle of the **currently visible** chat to
one or more containing terminal hosts. The integration is deliberately split
into three layers:

1. a generic manager owns ordering, fanout, deduplication, coalescing, timeouts,
   and bounded shutdown;
2. thin first-party adapters encode stable Herdr and cmux CLI commands; and
3. an opt-in JSON command sink lets tmux, Zellij, Agent Deck, Claude Squad,
   dmux, or a future host integrate without host-specific term-llm code.

No custom process and no OSC sequence runs by default. With the default `auto`
adapter policy, only environment-detected first-party adapters run.

## State semantics

The Bubble Tea `chatProgramModel` is the authority because it knows which chat
model is visible. That authority is created only when the invoking stdin and
stdout are genuinely interactive. A captured or tool-spawned `chat --auto-send`
child therefore cannot claim or release its containing interactive chat, while
a user who launches `--auto-send` directly in a real terminal still gets normal
host lifecycle reporting. The host publishes an initial state and publishes
again only when that visible state changes.

| State | Meaning |
| --- | --- |
| `blocked` | Waiting for approval, an `ask_user` answer, handover-preview confirmation, or a paused external UI. Blocked takes precedence over concurrent work. |
| `working` | Streaming; running a direct shell command or external process; changing a worktree; answering a side question; preparing branch context or a session switch; running a skill; mutating a transcript; or sharing a session. |
| `idle` | No blocked or working condition is active. |
| `release` event | term-llm is relinquishing lifecycle authority; this is not another live state. |

An outgoing in-process session remains authoritative until Bubble Tea installs
the replacement model. Delayed work from a no-longer-visible model cannot
publish over the current model.

## Configuration and controls

The conservative defaults are:

```yaml
lifecycle:
  enabled: true
  adapters: [auto] # auto-detect Herdr and cmux only
  osc: off         # off, auto, or on
  commands: []     # no external process unless explicitly configured
```

Use an explicit first-party allowlist instead of auto detection when desired:

```yaml
lifecycle:
  adapters: [herdr, cmux]
```

`auto` cannot be combined with explicit adapter names. Configured command sinks
are explicit opt-ins and run independently of the first-party allowlist.

Controls, from broadest to narrowest:

| Control | Effect |
| --- | --- |
| `lifecycle.enabled: false` | Disable all adapters, command sinks, and lifecycle OSC. |
| `TERM_LLM_LIFECYCLE=0` | Process-level global opt-out. |
| `lifecycle.adapters` | Select `auto`, `herdr`, `cmux`, both first-party adapters, or an empty list. |
| `TERM_LLM_HERDR=0` | Disable Herdr before any Herdr binary lookup. |
| `TERM_LLM_CMUX=0` | Disable cmux before any cmux binary lookup. |
| `lifecycle.commands` | Explicitly configure external JSON consumers. |
| `lifecycle.osc` | `off`, detected-terminal `auto`, or forced `on`. |

The older explicit `chat.terminal_progress: true` setting remains a compatibility
alias for `lifecycle.osc: auto` in its historical scope: smart terminal-title
mode on a detected Ghostty-compatible terminal. `basic` and `off` title modes do
not enable this deprecated path. Set it to `false` when controlling OSC through
the new lifecycle setting.

Inspect reality without publishing a fake live state or invoking a sink:

```bash
term-llm lifecycle status
term-llm lifecycle status --json
```

The report explains global policy and each adapter's detected/enabled status and
reason. There is intentionally no `probe`: a probe would have to claim a live
state and could mislead a host or another observer.

## Multi-host fanout and shutdown

Discovery considers every first-party adapter in deterministic order. If cmux
is nested in a Herdr pane, term-llm publishes to both. Each adapter has its own
worker and latest-value queue. A slow adapter can coalesce its stale pending
states without delaying a fast adapter; a failing adapter does not stop another.
Each worker calls its adapter synchronously with a bounded context, so even an
adapter that ignores cancellation cannot have a later state or release in
flight at the same time.

Every event from one manager uses a shared, strictly increasing sequence. Each
adapter receives the same sequence for the same event. A normal exit cancels
obsolete state work and attempts one release per claimed adapter in parallel.
The total close wait uses one bounded aggregate budget rather than multiplying
by the number of adapters. Release calls get up to 750 ms each within a one-second
aggregate close budget. Commands are best effort and have bounded contexts.

Adapter failures stay non-fatal and silent during normal TUI operation. For a
safe diagnostic that reports only a quoted adapter name and a fixed error class,
run with:

```bash
TERM_LLM_LIFECYCLE_DEBUG=1 term-llm chat
```

Repeated failures in the same adapter/error class are emitted once per manager.
Diagnostics never include argv, inherited environment, raw error text, cwd, or
session/event data. Use `term-llm lifecycle status` first for policy and
side-effect-free discovery explanations.

`/reload` re-execs the same process image without deliberately releasing host
claims first; the successor immediately republishes the visible state. If exec
fails, normal bounded cleanup releases the claims. If Bubble Tea does not stop
within the interrupt watchdog, term-llm writes a final OSC clear through the
bounded `/dev/tty` cleanup path and then spends its aggregate close budget on
adapter release before forcing exit. `SIGKILL`, a kernel crash, or power loss
cannot run cleanup, so hosts must still expire stale state and ignore a state or
release whose sequence is not newer than the last sequence accepted for that
producer/claim.

## Herdr: native lifecycle claim

Herdr is the first-party native lifecycle adapter. Use Herdr 0.6.9 or newer for
the `pane report-agent` / `pane release-agent` contract.

Detection requires:

- `HERDR_ENV=1`;
- `HERDR_PANE_ID`; and
- `HERDR_BIN_PATH`, or a `herdr` executable found on `PATH`.

The adapter invokes the release-matched Herdr CLI directly, never a socket or a
shell. It reports source `custom:term-llm`, agent `term-llm`, the state, shared
sequence, display message, and persisted session ID. It separately publishes the
visible session's preferred short title through `pane report-metadata`, scoped to
the term-llm lifecycle source. The value is sent as both Herdr's pane title and
its display-agent label because Herdr's default agent rows render the latter; this
makes `/title` and generated titles visible without custom Herdr sidebar rows. A
titleless new session clears both fields. On shutdown the adapter calls
`pane release-agent`. The session-ID field is forwarded for future compatibility
but is currently inert for Herdr custom-source restore authority.

Herdr currently treats `custom:term-llm` as a custom source. Native one-step
restore is not available: Herdr versions investigated for this integration do
not retain/expose the forwarded term-llm session ID as restore authority. Use:

```bash
term-llm chat --resume
term-llm chat --resume=<session-id>
```

## cmux: sidebar status, not agent authority

cmux is macOS-only. The adapter detects the documented
`CMUX_WORKSPACE_ID` and `CMUX_SURFACE_ID` and resolves the `cmux` binary. It does
not synthesize `--socket` from `CMUX_SOCKET_PATH`: the CLI consumes its inherited
canonical environment itself and enforces cmux's `CMUX_ALLOW_SOCKET_OVERRIDE`
policy. The adapter uses only the stable public CLI surface:

- `set-status` with `Working`, `Needs input`, or `Idle`; and
- `clear-status` on release.

The status key includes the surface ID so simultaneous chats in one workspace
do not overwrite each other. term-llm does **not** use notifications, manual
`workspace status set`, internal allowlisted `set_agent_lifecycle`, or persistent
resume mutation.

This is a cmux sidebar-status bridge. It is not cmux agent-journal,
hibernation, or native restore authority. The adapter was implemented against
the public cmux CLI documentation; this change does not claim a live macOS/cmux
smoke test.

## Generic JSON command sink

A sink is an executable followed by literal argv entries:

```yaml
lifecycle:
  commands:
    - name: my-host
      command: [/absolute/path/to/my-host-bridge, --literal-argument]
      timeout: 2s
```

For every state and release event, term-llm starts the executable once, writes
one JSON object plus a newline to stdin, connects stdout/stderr directly to the
null device, and waits only inside a bounded context. It calls the executable
directly with argv. It never uses a shell and never splits, expands, or
interpolates event strings into the command line.

The bridge receives session ID, cwd, PID, and resume argv. Treat those values as
untrusted data: parse JSON, validate what your host accepts, and pass values as
literal argv or API fields. Do not concatenate them into shell source.

### Version 1 event schema

Field order and JSON names are stable for `schema_version: 1`:

| Field | Type | Notes |
| --- | --- | --- |
| `schema_version` | integer | `1` |
| `producer` | string | `term-llm` |
| `kind` | string | `state` or `release` |
| `sequence` | integer | Shared, strictly increasing in one process. |
| `timestamp` | string | UTC RFC3339Nano. |
| `state` | string | `idle`, `working`, `blocked`; empty for release. |
| `message` | string | Bounded display detail; empty for idle/release. |
| `session_id` | string | Persisted session ID when available. |
| `pid` | integer | term-llm process ID. |
| `cwd` | string | Visible session effective working directory, with the term-llm process working directory as fallback. |
| `resume_argv` | string array | Canonical executable argv when a session ID is available; omitted otherwise. |

Example:

```json
{"schema_version":1,"producer":"term-llm","kind":"state","sequence":1700000000000000001,"timestamp":"2026-08-28T18:05:06.123456789Z","state":"blocked","message":"Waiting for approval","session_id":"session-123","pid":4242,"cwd":"/work/project","resume_argv":["/usr/local/bin/term-llm","chat","--resume=session-123"]}
```

Strings are trimmed, controls removed, whitespace normalized where appropriate,
and bounded before encoding. Resume argv is data, not a command for term-llm to
execute. A bridge should persist the last accepted sequence for its active
producer/claim and ignore older or equal state and release events; cancellation
and process scheduling can otherwise let stale external work arrive late.

## Complete tmux bridge

This sample needs `jq` and tmux. Save it as `~/.local/bin/term-llm-tmux-lifecycle`
and make it executable. It records a pane option; the pane-border format renders
the value. It does not attempt native session restore.

```bash
#!/usr/bin/env bash
set -euo pipefail

event=$(cat)
kind=$(jq -r '.kind' <<<"$event")
pane=${TMUX_PANE:?TERM_LLM tmux bridge requires TMUX_PANE}

if [[ "$kind" == release ]]; then
  tmux set-option -pu -t "$pane" @term_llm_lifecycle 2>/dev/null || true
  exit 0
fi

state=$(jq -r '.state' <<<"$event")
message=$(jq -r '.message' <<<"$event")
case "$state" in
  working) label='Working' ;;
  blocked) label='Needs input' ;;
  idle)    label='Idle' ;;
  *) exit 0 ;;
esac
[[ -n "$message" ]] && label="$label: $message"
tmux set-option -p -t "$pane" @term_llm_lifecycle "$label"
```

```bash
chmod 0755 ~/.local/bin/term-llm-tmux-lifecycle
```

Add a visible pane-border segment to `~/.tmux.conf`:

```tmux
set -g pane-border-status top
set -g pane-border-format ' #{pane_index} #{pane_current_command} #{?@term_llm_lifecycle,· #{@term_llm_lifecycle},} '
```

Configure term-llm:

```yaml
lifecycle:
  commands:
    - name: tmux
      command: ["/home/you/.local/bin/term-llm-tmux-lifecycle"]
      timeout: 2s
```

The repository test suite runs a self-contained real tmux end-to-end test when
`tmux` is installed; it skips cleanly otherwise. That test covers the generic
exec/JSON path, not every tmux theme or border configuration.

## Complete Zellij bridge

This sample needs `jq` and a bridge process launched inside the target Zellij
pane. Save it as `~/.local/bin/term-llm-zellij-lifecycle`:

```bash
#!/usr/bin/env bash
set -euo pipefail

event=$(cat)
kind=$(jq -r '.kind' <<<"$event")
if [[ "$kind" == release ]]; then
  zellij action undo-rename-pane >/dev/null 2>&1 || true
  exit 0
fi

state=$(jq -r '.state' <<<"$event")
case "$state" in
  working) label='term-llm · Working' ;;
  blocked) label='term-llm · Needs input' ;;
  idle)    label='term-llm · Idle' ;;
  *) exit 0 ;;
esac
zellij action rename-pane "$label" >/dev/null
```

Then:

```bash
chmod 0755 ~/.local/bin/term-llm-zellij-lifecycle
```

```yaml
lifecycle:
  commands:
    - name: zellij
      command: ["/home/you/.local/bin/term-llm-zellij-lifecycle"]
      timeout: 2s
```

This changes only the pane label and restores Zellij's prior automatic label on
release. It does not register a native agent or provide native restore. The
rename/release sequence was manually verified against Zellij 0.45.0; unlike the
tmux bridge, it is not part of the automated test suite because the test would
need to keep an attached Zellij client and focused pane alive.

## OSC fallback

OSC is separate from host adapters and is opt-in:

- `off` emits nothing;
- `auto` emits only when a Ghostty-compatible terminal is detected; and
- `on` forces the supported OSC 9;4 protocol.

`working` maps to indeterminate (`OSC 9;4;3`), `blocked` to paused at full
visibility (`OSC 9;4;4;100`), and `idle`/release to clear (`OSC 9;4;0`). The
explicit paused value follows the Ghostty/ConEmu grammar instead of inheriting an
unspecified prior percentage. Active indicators are refreshed because Ghostty
expires stale progress. Inside tmux, escape bytes use tmux DCS passthrough; tmux
3.3+ may require `set -g allow-passthrough on`.

Live sequences are returned as Bubble Tea `tea.Raw` commands. They are never
written from lifecycle worker goroutines and never embedded in `View`. The old
Ghostty title-progress provider no longer emits a second indicator, so there is
one progress owner. Direct terminal writes are reserved for bounded restore
after renderer ownership ends, plus the forced-exit watchdog's `/dev/tty` clear
when Bubble Tea has failed to stop. Post-Run restore may intentionally repeat a
clear because a concurrent quit can win before Bubble Tea writes the earlier
`tea.Raw` clear.

## Other host postures

| Host | Posture |
| --- | --- |
| **Herdr** | First-party native lifecycle claim; custom-source native restore remains unavailable. |
| **cmux** | First-party sidebar status only; no journal/hibernation authority. |
| **Superset** | Documentation/configuration posture only; no invented adapter. |
| **tmux / Zellij** | Generic command-sink status bridges; samples above. |
| **Agent Deck** | Configure a generic sink bridge to whatever documented status surface your deployment exposes. No bespoke core adapter or native restore claim. |
| **Claude Squad** | Use a generic sink to update an external status surface. term-llm does not claim Claude Squad process/session authority. |
| **dmux** | Use a generic sink bridge if your dmux version exposes a stable command/API. No bespoke core adapter or native restore claim. |

### Superset investigation

As of this investigation (2026-08-29), Superset documents custom terminal-agent
launch configuration, including **Command (No Prompt)**, **Command (With
Prompt)**, **Prompt Command Suffix**, and **Resume Args**. That is useful for
registering term-llm as a custom agent and for configuring
`term-llm chat --resume=<session-id>`.

No stable public protocol was found for an external process to publish
term-llm's live idle/working/blocked state into Superset's native lifecycle or
to delegate native restore authority. Superset's built-in unexpected-death
resume depends on an agent session ID known to Superset. Therefore this change
documents command/resume configuration but intentionally does not invent a
Superset adapter. Revisit only when Superset publishes a stable lifecycle API.

## Privacy and security

Lifecycle data can reveal that an agent is blocked, a short activity message,
the current directory, process ID, persisted session ID, and a resume command.
First-party adapters receive only fields their stable CLI supports. Every
configured generic sink receives the full event.

- Configure only executables you trust; a sink runs with the user's account and
  inherited environment.
- Use absolute executable paths where practical.
- Keep bridge scripts non-writable by untrusted users.
- Never log the inherited environment or turn JSON values into shell source.
- Remember that terminal/sidebar status may appear in recordings, screenshots,
  window-manager metadata, and host logs.
- A resume argv is a portable hint, not proof that a host can safely or natively
  restore the session.
