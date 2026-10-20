# Browser Bonsai bridge interface (Go WASM <-> JS)

Status: agreed contract, frozen unless both sides agree to change.
Owner of Go side: this worktree. Owner of `web/` JS side: parent agent.

The Go program is built with `GOOS=js GOARCH=wasm` into
`experiments/browser-bonsai/web/dist/term-llm.wasm` plus the matching
`wasm_exec.js`. Nothing else is required from the browser: there is no HTTP
backend, no Node server, and no network access.

The WASM module owns the real term-llm `llm.Engine`, the real tool registry, an
in-memory workspace, and an in-memory session store. JavaScript owns the page,
the fetch interception, Bonsai/WebGPU inference, and the v86 Linux guest.

## Boot

1. Page loads `wasm_exec.js`, then `web/boot.js` (parent-owned).
2. `boot.js` instantiates the Go runtime and `term-llm.wasm`.
3. Go sets `globalThis.__termLLMRequest`, `globalThis.__termLLMInfer`,
   `globalThis.__termLLMWorkspace`, and finally resolves
   `globalThis.__termLLMReady` (a Promise).
4. `boot.js` awaits `__termLLMReady`, installs the fetch/SSE interception, then
   dynamically imports the real frontend entry (`/dist/app-*.js` from the
   embedded Vite build served as static files).

The module never exits. Go's `main` returns only after installing the exports.

## 1. HTTP request bridge (frozen signature)

Exactly one entry point. JavaScript passes a JSON string so the Go side never
depends on JS object shapes:

```ts
// requestJSON = JSON.stringify({method, path, body?, headers?})
//   method: "GET" | "POST" | ...
//   path:   "/demo-api/v1/responses?x=1" (path + query, prefix included)
//   body:   UTF-8 string, optional (binary uploads unsupported in POC)
//   headers: Record<string,string>, optional
//
// onEvent is optional. It is invoked with:
//   status:      HTTP status (0 until headers are known)
//   headersJSON: JSON.stringify(Record<string,string>), "{}" until known
//   chunk:       UTF-8 response body fragment ("" for lifecycle events)
//   done:        false for data events, true exactly once at the end
//
// Returns a Promise resolving to a plain JS object:
//   { status: number, headers: Record<string,string>, body: string }
declare function termLLMRequest(
  requestJSON: string,
  onEvent?: (status: number, headersJSON: string, chunk: string, done: boolean) => void,
): Promise<{ status: number; headers: Record<string, string>; body: string }>;
```

Semantics:

- The request is handled by a real Go `http.Handler` through an
  `httptest`-style streaming adapter. `termLLMRequest` is also exported under
  the legacy alias `__termLLMRequest`; use the unprefixed name.
- Event ordering: once status and headers are known, the bridge invokes
  `onEvent(status, headersJSON, "", false)` (a header-ready event with an empty
  chunk). Every non-empty body write follows as
  `onEvent(status, headersJSON, chunk, false)`, in order, as soon as the handler
  writes and flushes it. A final `onEvent(status, headersJSON, "", true)` is
  always delivered exactly once. This lets the interceptor create a
  `ReadableStream` on the header-ready event and push SSE chunks into it.
- The promise resolves with the complete status/headers/body after the final
  event (or rejects for malformed request JSON; handler panics become a `500`).
- Concurrency: any number of requests may be in flight; each promise is
  independent. `onEvent` for different requests must not be confused — each
  invocation is bound to its own request.
- Response header names are lowercased; multiple values are joined with `", "`.
  Request headers are applied verbatim.
- Handler `r.Context()` is not yet cancellable from JS; treat requests as
  running to completion. Abort support is a compatible future addition.
- Safe to call before `termLLMReady`; the globals are installed first.

## 2. Inference bridge

```ts
declare function termLLMInfer(
  requestJSON: string,                          // OpenAI chat-completions request
  onDelta?: (chunkJSON: string) => void,
): Promise<string>;                             // OpenAI chat-completions response JSON
```

Semantics:

- `requestJSON` is an OpenAI-compatible `POST /v1/chat/completions` body:
  `{model, messages, tools?, tool_choice?, temperature?, max_tokens?, stream: true}`.
  Tool definitions use the OpenAI function schema, already produced by
  term-llm's provider layer.
- `onDelta(chunkJSON)` receives an OpenAI *stream chunk* object as JSON:
  `{"choices":[{"index":0,"delta":{"content":"..."}}]}` or a tool-call delta
  `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1",
  "function":{"name":"read","arguments":"{\"path\":"}}]}}]}`. The Go provider
  assembles deltas; an implementation may ignore `onDelta` and only return the
  final response.
- The promise resolves with an OpenAI chat-completions **response** JSON:
  `{"id":"...","model":"...","choices":[{"index":0,"message":{"role":"assistant",
  "content":"...","tool_calls":[...]},"finish_reason":"stop"}],"usage":{...}}`.
- Rejection with an `Error` fails the provider stream; the chat turn reports the
  error in the UI. Do not reject for a normal stop.
- Go calls this from a goroutine with `js.FuncOf` callbacks, so the JS function
  may be `async`; it must not block the event loop synchronously.
- Exported with the alias `__termLLMInfer` for tolerance.

## 2b. Ready signal and diagnostics

```ts
globalThis.termLLMReady: Promise<void>;  // resolved after all exports are installed
globalThis.termLLMInfo(): { go: string; workspace: string; model: string };
```

`web/boot.js` should `await termLLMReady` before installing fetch interception
and importing the real frontend entry.

## 3. Workspace introspection (debug/UI only)

```ts
declare function __termLLMWorkspace(): Array<{ path: string; size: number }>;
```

Returns the current virtual workspace file list. Optional for the UI; used by
the demo page footer and tests.

## 4. Linux shell bridge (tool `shell`)

The Go tool `shell` runs commands in the page's real v86 Linux guest. The page
owns the VM (BusyBox userland plus a CPython 3.12 overlay); Go owns the
workspace snapshot/diff and never invokes a host shell.

```ts
// requestJSON = JSON.stringify({command, working_dir, files, timeout_seconds})
//   command:         shell command line
//   working_dir:     always a guest path inside /workspace (default "/workspace")
//   files:           Record<relativePath, textContent> — full text workspace snapshot
//                    (at most 256 files / 8 MiB; non-UTF-8 files are omitted)
//   timeout_seconds: 1..30; real command execution budget. A lazy first boot may
//                    take up to 180s extra, outside this budget.
//
// Resolves to a JSON string:
//   {stdout, stderr, exit_code, files}
//   files is the FULL post-execution workspace map, same relative-path keys.
declare function termLLMRunShell(requestJSON: string): Promise<string>;

// Cancels the in-flight guest command (tool timeout/cancel).
declare function termLLMCancelShell(): void;
```

Semantics:

- The runtime is a real BusyBox Linux system (v86) with `sh`, pipes, `awk`,
  `sed`, `grep`, and coreutils, plus CPython 3.12 as `python`/`python3`. The
  tested standard-library set is `json`, `csv`, `statistics`, `unittest`, and
  `sqlite3`; `base64`, `binascii`, `gzip`, and `zipfile` work through bundled
  zlib. Optional extension modules requiring unbundled libraries (`ssl`,
  `ctypes`, `bz2`, `lzma`, `readline`) are unavailable; only musl, libsqlite3,
  and zlib are bundled. It is
  **not the host machine** and has no network: there is no pip or package
  installation, and `node` and Go are not installed.
- On the first shell boot, the page fetches a same-origin, SHA-256-pinned
  `runtime/python-root.tar.gz`, uploads it to host-backed 9p, extracts it to
  `/mnt/runtime/python`, and removes the archive. This is part of the 180s cold
  boot allowance. Python runs with `-B`/`PYTHONDONTWRITEBYTECODE=1`; the
  installed runtime is not written to. `/workspace` synchronization remains
  UTF-8 text only; the binary runtime upload does not change that protocol.
- The guest mounts the shared workspace at `/workspace`. Go accepts a model
  `working_dir` only when it resolves inside that tree; absolute host paths are
  stripped to their relative form and paths outside the workspace are rejected.
  No host path appears in the request.
- Go rejects the entire returned `files` map before applying anything if a key
  is absolute, contains `..`, uses backslashes, is non-canonical, exceeds 256
  files, or exceeds 8 MiB. Rejected maps leave the workspace untouched.
- Go diffs the returned map against its snapshot: changed/new files are written
  and omitted files are deleted in the same `internal/memfs` workspace the real
  `read_file`, `write_file`, `edit_file`, and `glob` tools use. Native tests use
  a temporary directory workspace; no host path outside it is ever touched.
- The tool never prompts. The browser agent runs with the real
  `tools.ApprovalManager` in `ModeYolo`.
- `exit_code !== 0` marks the tool output as an error while preserving the
  actual `stdout`/`stderr` (each capped at 64 KiB) and the measured duration.

## 5. In-memory workspace

The Go side implements a virtual filesystem so the **real** `internal/tools`
file tools run unmodified against an ephemeral workspace rooted at `/workspace`.
No JS filesystem shim is required.

- Initial files are seeded by Go: `/workspace/README.md` and the
  `/workspace/playground/` starter (`calculate.sh` is deliberately broken,
  `tests.sh` fails, `bonsai.sh` is a shell + awk demo, and `scores.csv` is input
  for a Python csv/json/statistics/sqlite3 exercise).
- Tools enabled in the POC: `read_file`, `write_file`, `edit_file`, `glob`, and
  `shell`; `manage_workspace` is hidden from the model.
- Host shell/bash, grep (needs `rg`), process, git, and worktree tools are
  disabled; command execution happens only inside the sandboxed v86 guest.
- The workspace survives for the page lifetime and lives only in Go memory.
- The agent identity is the genuine built-in `developer` prompt from
  `internal/agents`, running with the real approval manager in `yolo` mode.

## Output layout (build.sh)

`experiments/browser-bonsai/build.sh` writes a self-contained static site:

```
dist/
  index.html              genuine serveui shell, loads boot.mjs
  boot.mjs worker.mjs transport.mjs prose-stream.mjs
  inference.bundle.mjs ort/            parent-owned inference runtime
  term-llm.wasm wasm_exec.js           Go module + matching runtime
  runtime/python-root.tar.gz           pinned CPython 3.12 overlay
  runtime/python-runtime.json          package URLs, hashes, versions, licenses
  python-runtime.mjs                   generated archive hash metadata
  licenses/…                           third-party notices incl. Python/musl
  assets/…                             hashed real frontend (Vite dist)
  icon-512.png manifest.webmanifest
```

`cmd/shell` renders `serveui.RenderIndexHTML` and injects
`window.TERM_LLM_DEMO_ENTRY = "assets/app-<hash>.js"`; nothing else in the page
loads the app entry. `cmd/serve` is a development-only static file server
(application/wasm MIME); it is not an application backend.

## UI prefix and routing (important)

The real frontend uses `TERM_LLM_UI_PREFIX` for both API paths and its document
route: a new chat calls `history.replaceState('/demo-api/')`, and session routes
become `/demo-api/chat/<slug>`. Those are shell routes, not HTTP resources, so
the demo must keep the document on its static mount.

Agreed POC fix (parent-owned boot, no frontend source change): keep
`TERM_LLM_UI_PREFIX = '/demo-api'` and transport prefix `/demo-api`, and wrap
`history.pushState`/`history.replaceState` before importing the app. When the
target path is `/demo-api` or starts with `/demo-api/`, rewrite it to the
captured initial static mount (e.g. `/__browser-bonsai-poc__/`), preserving
`search` and `hash`. API calls never navigate, so they are unaffected.

Static hosting (and the smoke router) should still serve `index.html` for any
extensionless path on the mount, so a reload after a rebased route is harmless.

## 6. Frontend config injected by the build harness

`experiments/browser-bonsai/cmd/shell` renders the real
`internal/serveui` shell into `web/index.html` and copies the built Vite assets
to `web/dist/assets`. It injects:

```js
window.TERM_LLM_UI_PREFIX = "/demo-api";
window.TERM_LLM_SHELL_AUTHORIZED = true;
window.TERM_LLM_UI_TITLE = "term-llm browser bonsai";
window.TERM_LLM_AGENT_NAME = "bonsai";
// no TERM_LLM_CACHE_SCOPE: private persistence stays disabled
```

The fetch interceptor maps `/demo-api/*` to `__termLLMRequest`; the Go router
therefore sees `/demo-api/...` and strips the prefix internally. SSE is passed
through chunk-by-chunk.
