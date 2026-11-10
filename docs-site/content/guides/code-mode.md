---
title: "Code mode"
weight: 15
description: "Bounded JavaScript tool composition and activated skill ES modules."
---

`spawn_script` starts a fresh QuickJS/WASM cell. It can finish immediately or return a
running cell ID; `wait_script` observes new output and `cancel_script` requests termination.
It is included in the builtin **developer and sysadmin** agents alongside their
ordinary direct tools. Other agents remain opt-in. Use direct tools for simple
actions, discovery, skill activation and lifecycle controls; use a cell when
loops, several calls, filtering or an activated skill module are useful.

## Cell contract

```json
{"code":"const r = await tools['catalogue__detail']({id: 3}); text(r.structuredContent.title); store('lastId', 3);","yield_time_ms":10000,"max_output_tokens":10000}
```

`code` is an async **function body**: await and return work; use dynamic
`import('skill:name')`, not rewritten static imports. Every cell has fresh globals.

| Global | Contract |
|---|---|
| `tools[exactName](args)` | Promise of a permitted registered tool's programmatic result. No prototype-derived callable names. |
| `describeTools([names])` | Authoritative input/output schemas, exact names, schema hash and envelope description within the admitted capability ceiling. Use model-facing `tool_search` before a cell for lazy discovery. |
| `text(value)` | Explicit ordered text; strings stay strings, JSON values serialize as JSON. A defined return value emits text after explicit output. |
| `image(blockOrHandle)` | Explicit image emission. Accepts an MCP image block or a current-cell host handle; never fetches a URL or reads an arbitrary path. |
| `load(key)` / `store(key, value)` | Staged session-local JSON state, automatically committed after success. Missing keys return undefined; storing undefined deletes. Cycles, functions, BigInt and nonfinite numbers reject. |
| `sleep(ms)` | Cancellable host wait, at most1000ms per call; counted in cell resources. No detached timers. |

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
Recursive code mode, client passthrough, planner exceptions, activation/discovery,
interactive prompts, spawning/jobs and authority/lifecycle controls are direct-only.
A cell freezes its tool/schema ceiling, then rechecks current policy and catalogue
on every call. Activation/import never grants tools. Schema changes reject with
`schema_changed`, inviting rediscovery in a new cell.

## Execution lifecycle

`spawn_script` returns a `cell_id` and an observation of that cell. If it is still active,
call `wait_script` with the returned ID to collect **only new output** or completion:

```json
{"cell_id":"<actual ID from exec>","yield_time_ms":10000,"max_output_tokens":10000}
```

A yield/wait budget is an observation deadline, **not a cell kill timer**. The
cell continues when the budget expires or a wait request is cancelled. Use
`cancel_script` with that same `cell_id` to request termination. Do not fabricate IDs,
automatically restart cancelled work, or confuse a successful observation with a
successfully completed cell. Only a completed successful cell commits its store.
Session/runtime teardown also cancels owned cells. The process has four VM slots
shared across sessions; a long-running cell holds one until it ends. A full pool
reports busy rather than starting overlapping work. There is no idle expiry.

Explicit text/images keep their emission order. Output budgets truncate visible
text with an omission notice; they do not turn successful work into a false tool
failure. Raw intermediate bodies and the stored state are not automatically
printed. Nested calls remain auditable effects, not extra model-authored calls.
Outstanding unawaited host calls still cause failure.

### State is session-managed, not durable

```javascript
// First spawn_script: complete successfully to commit this write.
store('lastId', 3);
// Later spawn_script in the same session: no state argument is needed.
text(load('lastId'));
```

The store allows 1 MiB total serialized JSON and 256 KiB per value. Failed or
cancelled cells discard staged writes. Every new cell still has a fresh JS heap:
functions, promises and globals do not carry over. Image handles are cell-scoped
and cannot be stored; reacquire screenshots in a later cell. This is memory owned
by the live session runtime, not a database: replacing/evicting the runtime or
restarting the process clears it. A conversation-only clear may retain this store;
an explicit session reset clears it. There is no automatic transcript replay or
cross-session/fork state restoration.

The old `code_mode` name remains an explicit synchronous compatibility tool with
its original caller-carried state contract; it is not included in the new agent
defaults. New workflows should use `spawn_script` / `wait_script` / `cancel_script`.

## Limits

| Resource | Async execution limit |
|---|---|
| Code | 64 KiB |
| Session state / value | 1 MiB / 256 KiB JSON |
| Arguments / raw non-image result per host call | 256 KiB / 4 MiB |
| Bridge requests / executing tools per cell | 64 / 4 |
| Executing tools per process | 16 |
| Active cell per engine / VM instances per process | 1 / 4; busy rejection rather than overlapping uncertain work |
| QuickJS heap / WASM linear memory / native stack | 256 MiB / 512 MiB / 512 KiB |
| Cumulative active JS execution | 2 seconds; host-tool and sleep waiting excluded |
| Yield/wait budget | 10 seconds by default; does not terminate the cell |
| Image handles / decoded image bytes | 64 handles / 16 MiB over the cell lifetime; start a fresh cell for longer screenshot workflows |
| Model-visible text | 10,000 estimated tokens per observation by default; separately bounded retention |
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
