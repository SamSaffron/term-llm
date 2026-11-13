---
title: "Code mode"
weight: 15
description: "Bounded JavaScript tool composition and activated skill ES modules."
---

`spawn_script` starts a fresh QuickJS/WASM cell. It can finish immediately or return a
running cell ID; `wait_script` observes new output and `cancel_script` requests termination.
It is included in the builtin **developer and sysadmin** agents and newly bootstrapped
managed `term-llm contain` agents alongside their
ordinary direct tools. Other agents remain opt-in. Use direct tools for simple
actions, discovery, skill activation and lifecycle controls; use a cell when
loops, several calls, filtering or an activated skill module are useful.

## Cell contract

```json
{"code":"const r = await tools['catalogue__detail']({id: 3}); text(r.structuredContent.title); store('lastId', 3);","max_wait":60,"max_output_tokens":10000}
```

`code` is an async **function body**: await and return work; use dynamic
`import('skill:name')`, not rewritten static imports. Every cell has fresh globals.

| Global | Contract |
|---|---|
| `tools[exactName](args)` | Promise of a permitted registered tool's programmatic result. No prototype-derived callable names. |
| `describeTools([names])` | Purpose (up to 1 KiB), authoritative input/output schemas, exact names, schema hash and envelope description within the admitted capability ceiling. Use model-facing `tool_search` before a cell for lazy discovery. |
| `text(value)` | Explicit ordered text; strings stay strings, JSON values serialize as JSON. Separate nonempty emissions are newline-separated in flattened output; existing newlines are preserved. A defined return value emits text after explicit output. |
| `image(blockOrHandle)` | Explicit image emission. Accepts an MCP image block or a current-cell host handle; never fetches a URL or reads an arbitrary path. |
| `load(key)` / `store(key, value)` | Staged session-local JSON state, automatically committed after success. Missing keys return undefined; storing undefined deletes. Cycles, functions, BigInt and nonfinite numbers reject. |
| `settleInOrder(promises)` | Async iterator of `{index,status,value}` or `{index,status,reason}` in settlement order. Does not limit concurrency or cancel calls on break. |
| `sleep(ms)` | Cancellable host wait, at most1000ms per call; counted in cell resources. No detached timers. |

Use `for await (const r of settleInOrder(calls))` for a small batch of independent
calls. Rejections have `status: "rejected"`; fulfilled values still need error-flag
checks. Await remaining calls after breaking; dispatched effects are not undone.

MCP results resolve as `{content: MCPContentBlock[], structuredContent?, isError?}`.
The raw structured envelope is available **before** model-text truncation; text
that resembles JSON is not parsed into business data. `isError:true` is a resolved
MCP envelope, not a transport rejection. Non-image content/metadata remain intact;
resource URIs are not dereferenced. Native tools have an honest fallback
`{text,isError,timedOut,content?}`; inspect their text explicitly, not an invented
structured business object.

Policy, transport, cancellation and size failures reject with stable `code`,
`message`, and (for child tool calls) `call_id` and `outcome`. An oversized result
after dispatch does **not** mean the action had no effects. Never blindly retry an
unknown outcome: observe or reconcile first. Approvals, Guardian, workspace/file
policy, session identity and ordinary accounting still apply to child tools.
Script exposure is declared separately from tool permissions. `Object.keys(tools)`
lists the callable names admitted to this cell; it is not the complete direct-tool
catalogue. Undeclared tools are direct-only. Enabling code mode never enables other
tools or grants permissions.

Agent orchestration (`spawn_agent`, `wait_agent`, `continue_agent`, `cancel_agent`,
`list_agents`), durable jobs (`queue_agent`, `wait_for_jobs`), `update_plan`,
`search_skills` and `run_agent_script` can be composed when enabled and bound by the
host. They retain the same parent ownership, depth, model policy, workspace and
concurrency restrictions as direct calls. Cancelling a script does not itself mean
that independently started children or jobs were cancelled: reconcile the returned
handles, and use explicit lifecycle tools. Host Stop/shutdown rules still apply.

Recursive script controls, finishing/output tools, handovers, `ask_user`,
`manage_workspace`, live settings/session switching, skill activation, UI extension
lifecycle, hub delegation, and goal/progress mutations remain direct-only.
`tool_search` is also direct-only: its special permission and schema activation
belong to a provider run. Use it before starting a cell, or inspect already admitted
schemas with `describeTools`. Incoming authorized MCP tools can be called even if
their schemas are deferred from the provider's initial tool list. Discovery and
MCP annotations do not grant authority.
A cell freezes its tool/schema ceiling, then rechecks current policy and catalogue
on every call. Local tools capture their original cwd, permissions and approval
scope. Later turns cannot retarget that cwd or widen the cell by enabling yolo;
tightened approval modes and explicit additional-workspace revocations still apply.
MCP tools retain their native server permission/lifetime rules; the VM does not
snapshot external service state. Custom/skill scripts retain their admitted source
directory, definition and permission scope, not a frozen copy of executable file
bytes. Edits within that admitted directory are observed as with direct execution;
changing the registered source/binding or removing a tool requires a new cell.
Idle approval requests remain visible and wait on the original cell's cancellation
context. A new response reconnects presentation only, never authority or ownership.
Terminal chat serializes its single foreground dialog; cell cancellation dismisses
only the matching prompt, including queued/detached prompts. Current or newly toggled
yolo cannot auto-answer a cell that was admitted under prompt mode. One-shot `ask`
also binds native approval programs and terminal flush/restoration to the cell's
context; command/runtime teardown still ends its cells. Activation/import never grants tools. Schema changes reject with
`schema_changed`, inviting rediscovery in a new cell.

### Agent composition

Native tools return the native envelope; agent business results are JSON text in
`text`, not a fabricated `agent_id` field on the envelope:

```js
const responses = await Promise.all([
  tools.spawn_agent({agent_name: 'developer', prompt: 'Inspect the parser', wait: 0}),
  tools.spawn_agent({agent_name: 'developer', prompt: 'Inspect the tests', wait: 0}),
]);
const ids = responses.map(r => {
  if (r.isError || r.timedOut) throw Error(r.text);
  const child = JSON.parse(r.text);
  if (!child.agent_id) throw Error(r.text);
  return child.agent_id;
});
const result = await tools.wait_agent({agent_ids: ids, max_wait: 60});
if (result.isError || result.timedOut) throw Error(result.text);
text(result.text);
```

An error in one branch does not undo work already started in another branch. Await
and reconcile all dispatched calls; never automatically replay an uncertain spawn.

### Foreground client tools

Client/device/WebMCP tools are distinct from incoming server MCP tools. Script
access requires an explicitly offered schema and a scoped inline client runner
whose host can revoke the originating foreground binding. A passthrough tool that
can only be answered in a later client request is not automatically script-callable.

The host pins client access to the originating response/client. When that binding
ends, later client calls fail closed; another response, reconnection or
`wait_script` cannot rebind them. Server-owned tools in the same cell may continue.
An observation timeout alone does not expire a still-active foreground binding.
Calls rejected before dispatch have `outcome: "not_dispatched"`; device actions
already dispatched without acknowledgement may have `outcome: "outcome_unknown"`
and must not be replayed. Each foreground lease has a fixed 128-call anti-replay
budget across its cells; exhausting it is not transient and cannot be repaired by
retrying the call in the same binding. Transport support is deliberately fail-closed:
a tool schema alone does not promise a safe client lifetime.

### Script diagnostics

Syntax errors and runtime exceptions include source locations in the error text,
including stack frames when available. Cell locations use `cell.js`, with line
numbers matching the submitted function body (its first line is line 1); imported
skill modules retain their source filenames. Both syntax errors and uncaught
async throws or rejections are reported consistently with a `script_error:` prefix
(and `error_code: "script_error"` on lifecycle observations). Exception text that
happens to mention `image`, `state_limit`, or another resource error name does not
change that classification. Actual host/runtime resource failures retain their
own error codes.

Calling an unknown `tools[exactName]` throws a descriptive `unknown_tool` error
naming the requested tool, rather than an opaque "not a function" error. Use
`Object.keys(tools)` to list the admitted names and `describeTools([name])` to
inspect arguments. If uncaught, this is a script failure; it does not dispatch a
host call or grant access to a missing tool.

## Execution lifecycle

`spawn_script` returns a `cell_id` and an observation of that cell. If it is still active,
call `wait_script` with the returned ID to collect **only new output** or completion:

```json
{"cell_id":"<actual ID from exec>","max_wait":60,"max_output_tokens":10000}
```

`max_wait` is an observation deadline in seconds: **0–600**, default **60**.
An explicit `0` checks immediately without waiting. It is **not a cell kill timer**. The
cell continues when the budget expires or a wait request is cancelled. Use
`cancel_script` with that same `cell_id` to request termination. Do not fabricate IDs,
automatically restart cancelled work, or confuse a successful observation with a
successfully completed cell. Only a completed successful cell commits its store.
Each session may own **eight live cells**, independently of other web sessions.
A ninth spawn fails `session_cell_limit` and lists that session's `live_cell_ids`.
A cancelled cell with an unresolved native/remote operation keeps **one** of those
places until the operation actually returns; unrelated cells can proceed.

Normal assistant final/end-turn, observation timeout, cancelled wait request and
browser disconnect **do not stop cells**. The next model request includes a compact,
regenerated roster of live/recent handles, independent of transcript compaction.
`cancel_script` stops only its target. Explicit human **Stop / Ctrl-C cancels all
live cells of that session**, including cells from earlier turns. In the Web UI,
open **Activity → Scripts** for per-cell Cancel, **Stop all scripts** while idle,
waiting approvals and recent terminal outcomes. Session reset and runtime/process
shutdown cancel owned cells; live cells protect an idle runtime from TTL eviction.
There is no cell idle expiry and no survival/replay across process restart.

Process admission is separate: **256 VM instances**, **2 GiB charged linear-memory
capacity** and **1 GiB/session** (caps, not reservations of maximum heaps). Each VM
reserves a 4 MiB admission floor; growth charges actual backing capacity beyond
that floor. A full pool fails promptly (`vm_capacity` / `memory_capacity`), rather
than advertising a running cell that is indefinitely queued for a VM. Cold-cache
compilation is serialized with cancellable waiting to avoid duplicate compiler
working sets; VM execution remains concurrent. Under aggregate memory pressure,
growth fails the affected cell (`memory_limit`); admission may fail even below 256.
The accounted cap excludes native tools, image/output buffers, Go GC lag, and
transient copies during growth—it is not an operating-system RSS sandbox.

Measured on linux/amd64 with wazero v1.11.0 and this embedded artifact: 32 session
budgets × 8 real awaiting VMs charged 1 GiB total (4 MiB/cell); additional Go heap
was 851,855,496 bytes and RSS rose from 47,520 to 760,552 kB. This is a lightweight
host-wait measurement, not a claim about arbitrary tool workloads. Compared with
the old four-VM × 512 MiB theoretical maximum, the process charged memory ceiling
remains 2 GiB while lightweight multi-session capacity increases substantially.

Nested tools share **4 executing slots/session** and **16/process**, regardless of
cell count. FIFO channel admission and the per-session cap prevent one session
from taking the entire tool pool; there is no strict round-robin or latency SLA.
Many sessions can hold waiting VMs, but simultaneous tool throughput remains 16.
Human approval waits **and prompt-mutex waiters** park the shared executing slots,
retaining their per-cell slot, and re-acquire cancellably before execution. Long-running actual tools keep
their slots until they return, even after cancellation. The UI/model roster
separates `waiting_calls`, `awaiting_approval` and quarantined outcomes.

Explicit text/images keep their emission order. Separate nonempty text emissions
(including a defined return value) are separated by a newline when neither the
previous text's trailing edge nor the next text's leading edge already has one.
Explicit trailing newlines and blank lines are preserved; empty emissions add
nothing. For example, `text('one'); text('two')` flattens to `one\ntwo`, and
`text('one\n'); text('two')` produces the same text. This is a code-mode emission
rule, including legacy synchronous `code_mode`, not a change to arbitrary MCP
content parts.

Observation budgets may split a single emission across observations: concatenating
those text chunks reconstructs the emission without an extra separator in the
middle. Inserted separator bytes count toward the text observation budget and the
retained-output cap. Undelivered text remains queued for `wait_script`; output
beyond the retention cap evicts the oldest unread text, preserving the final tail.
Omitted bytes are reported; images stay in emission order. These
limits do not turn successful work into a false tool failure. Raw intermediate
bodies and the stored state are not automatically printed. Nested calls remain
auditable effects, not extra model-authored calls. Outstanding unawaited host
calls still cause failure.

### State is session-managed, not durable

```javascript
// First spawn_script: complete successfully to commit this write.
store('lastId', 3);
// Later spawn_script in the same session: no state argument is needed.
text(load('lastId'));
```

The store allows 1 MiB total serialized JSON and 256 KiB per value. Each cell
reads an admission-time snapshot and stages an explicit set/delete write set.
Successful completion merges **only written keys** into the current store under
one mutex. Disjoint overlapping cells cannot erase each other's keys. For the
same key the **last successful committer wins**; this is not a serializable
transaction. `state_overwritten_keys` reports written keys whose current version
differs from the admission snapshot (not an audit of transient absent-key history).
Mutating a loaded object without `store(key, value)` is local to the cell. A merged
size-limit failure applies no keys. Failed, cancelled or reset cells commit nothing. Every new cell still has a fresh JS heap:
functions, promises and globals do not carry over. Image handles are cell-scoped
and cannot be stored; reacquire screenshots in a later cell. This is memory owned
by the live session runtime, not a database: replacing/evicting the runtime or
restarting the process clears it. A conversation-only clear may retain this store;
an explicit session reset clears it. There is no automatic transcript replay or
cross-session/fork state restoration.

The old `code_mode` name remains an explicit synchronous compatibility tool with
its original caller-carried state contract; it is not included in the new agent
defaults. Its synchronous lane is mutually exclusive with async cells in the same
engine, including unresolved effects, so enabling compatibility cannot add a ninth
cell or double the shared session tool capacity. New workflows should use `spawn_script` / `wait_script` / `cancel_script`.

## Limits

| Resource | Async execution limit |
|---|---|
| Code | 64 KiB |
| Session state / value | 1 MiB / 256 KiB JSON |
| Arguments / raw non-image result per host call | 256 KiB / 4 MiB |
| Bridge requests / executing tools per cell | 64 lifetime / 4 (including approval waits) |
| Nested requests in flight / executing tools per session | 64 / 4 shared across cells |
| Executing tools per process | 16 |
| Live cells per session / VM backstop per process | 8 / 256; immediate capacity errors |
| Charged VM memory per session / process / admission floor | 1 GiB / 2 GiB / 4 MiB per VM |
| QuickJS heap / WASM linear memory / native stack | 256 MiB / 512 MiB / 512 KiB |
| Cumulative active JS execution | 2 seconds; host-tool and sleep waiting excluded |
| Observation wait (`max_wait`) | 0–600 seconds, default 60; 0 polls immediately; does not terminate the cell |
| Image handles / decoded image bytes | 64 handles / 16 MiB over the cell lifetime; start a fresh cell for longer screenshot workflows |
| Model-visible text | 10,000 estimated tokens per observation by default; 1 MiB retained text per cell |
| Recent terminal receipts | 8/session; drained receipts expire first, otherwise oldest |
| Undelivered image retention | 32 MiB/session; excess is permanently omitted and reported |
| MCP stdio frame / SSE event / ordinary HTTP response body | 16 MiB each; no lifetime cap on SSE connections |

Cancellation stops new admission and interrupts the VM. An uncooperative native
Go function or remote action can outlive it: actual child permits and engine
quarantine remain until the real call exits. New cells fail busy rather than
overlapping uncertain work. These JS limits do not memory-sandbox arbitrary native
tools. Browser (`js/wasm`) hosts explicitly report code mode unavailable without
breaking ordinary tools. Go releases remain CGO-free and embed the pinned WASM;
Node/npm/wasi-sdk are not interpreter runtime dependencies.

## Skill-shipped ES modules

Activation admits an immutable, content-hashed snapshot; it does not execute module
top-level code. Sources use rooted no-escape reads. Only declared files are loaded:
no npm, Node modules, network fetch, std/os, native extensions or bytecode ingestion.

```yaml
---
name: local-computer
description: Isolated computer facade
code-mode:
  module: js/index.mjs
  files: [js/index.mjs, js/helpers.mjs]
  requires:
    tools: [computer__click, computer__type, computer__observe, computer__wait, computer__screenshot]
---
Use the isolated backend's local page. Explicitly emit observations and screenshots.
```

`js/helpers.mjs`:

```javascript
export function metadata(r) {
  if (r.isError || !r.structuredContent) throw Error('computer action failed; reconcile before retry');
  return r.structuredContent;
}
```

`js/index.mjs` (complete small facade):

```javascript
import { metadata } from './helpers.mjs';
export const computer = {
  click: async (x,y) => metadata(await tools['computer__click']({x,y})),
  type: async text => metadata(await tools['computer__type']({text})),
  observe: async (opts={}) => metadata(await tools['computer__observe'](opts)),
  wait: async (opts={}) => metadata(await tools['computer__wait'](opts)),
  screenshot: async () => {
    const r=await tools['computer__screenshot']({});
    return {...metadata(r),image:r.content.find(p=>p.type==='image')};
  }
};
```

Entry must appear in `files`: at most32 sources,1MiB total and optional64KiB
`types` declarations. Absolute, parent-traversal, backslash/drive, duplicate and
case-alias paths, unsupported schemes/extensions and symlink escapes reject.
Imports resolve `skill:<activated-name>` and same-skill `./relative.mjs` dependencies
from the snapshot only. Cross-skill module dependencies are deferred. Rewriting
files does not hot-reload an admitted cell; reactivate to admit a new generation.
Import errors retain source filenames; there is no fetch/retry/re-evaluation of
effectful module code.

`requires.tools` declares dependencies only: it never registers/enables/grants a
tool. All modules in a VM share that cell's admitted authority, **not** per-module
isolation. Actual user-installed/builtin sources are eligible; project/unclassified
sources require `skills.allow_project_code_modules: true` (defaultfalse) or current
yolo. This is separate from `allow_project_script_tools`. Frontmatter cannot claim
host installation trust. An agent without a permitted `spawn_script` (or legacy
`code_mode`) still activates such a skill: its instructions load, nothing is
admitted, and the activation says the module is unavailable. A skill can therefore
ship a module as an optional convenience next to instructions that work without
it. Trust and dependency failures still reject activation. Current trust and tool policy are rechecked at import/call
boundaries. New/resumed engines require reactivation; transcript strings never
restore executable grants. Turn-scoped admission is restored with temporary skill
policy restrictions.

## Real isolated browser example

The checkout includes a copyable skill at
`internal/codemode/testdata/computer-skill/` and the trusted external MCP backend
`internal/codemode/testdata/browser-backend.mjs`. This backend uses Playwright in
its **own process and fresh profile**, not inside QuickJS. Install frontend dev
dependencies and an appropriate Chromium separately; do not attach shared tabs.
Configure the explicit installed executable when HOME/XDG paths are isolated:

```json
{"servers":{"computer":{"command":"node","args":["/checkout/internal/codemode/testdata/browser-backend.mjs","--log-path","/owned/tmp/browser.jsonl"],"env":{"PLAYWRIGHT_CHROMIUM_EXECUTABLE":"/installed/chrome"}}}}
```

This is the `mcp.json` shape. Copy the skill directory into your user skills path,
connect `computer`, and call `activate_skill({name:'computer'})` directly. Then:

```javascript
const {computer}=await import('skill:computer');
await computer.click(160,90); // focus owned test page input
await computer.type('hello');
await computer.click(90,140); // submit, updates #result
await computer.wait({selector:'#result',timeout:2000});
text(await computer.observe({selector:'#result'}));
const shot=await computer.screenshot();
text('before screenshot'); image(shot.image); text('after screenshot');
store('sessionId',shot.sessionId); store('frameId',shot.frameId);
```

After the first cell completes, start a fresh cell, import again, read the stored
`sessionId` with `load`, compare it with the observation, and reacquire/emit a new
screenshot. No in-VM variable survives.
The facade's screenshot returns a handle-bearing image block plus actual dimensions,
viewport CSS coordinate space, crop/scale and frame ID; action call IDs are distinct.
Generic MCP image blocks need not contain this browser-specific metadata.

The backend owns a **single** browser lane: competing direct MCP and nested facade
input serialize at the same resource boundary. Cancellation preserves the lane until
actual completion and requires explicit observe/screenshot reconciliation before
more input. It never retries an effectful request. This is a supported seam for this
one isolated backend, not a universal lock inferred for arbitrary computer services.
Screenshots are real PNG bytes delivered through ordered provider content and served
media URLs only after explicit `image(...)`; file paths/base64 dumps are not artifact
authority, and forged/expired/cross-cell handles reject. The model must make visual
judgments after receiving an observation/screenshot; JS cannot visually reason itself.

The backend also supplies deterministic paginated search/detail/text/error/schema
fixtures for integration tests. Those are fixture data, not evidence of a live
external search service. The CLI/serve can be exercised with a scripted provider
for deterministic protocol tests; only a separate real provider run establishes
live-model usability.

## Deliberate deferrals

Durable restart-safe state, automatic cell replay, a universal desktop SDK,
cross-skill dependencies, npm compatibility in the VM and a global code-mode-only
switch remain deferred. Execution and lifecycle controls use ordinary JSON tool
schemas across providers; they do not require a provider-specific raw-source wire
format.
