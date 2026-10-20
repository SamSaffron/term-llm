# Browser Bonsai POC

The genuine term-llm web chat UI, the real `llm.Engine`, and the real local file
tools running inside one browser tab, with Bonsai 2 2B generated on the GPU via
WebGPU. **There is no application backend.** After the static files load, the
only external network requests are the pinned Linux kernel/firmware fetched
from upstream. Model weights and the CPython runtime are same-origin static
assets; weights can be cached locally. Chat, tools, sessions, and workspace state
never leave the page.

`INTERFACE.md` is the frozen Go<->JS bridge contract and output layout. `web/`
(boot, worker, transport, inference) is parent-owned; this directory owns the Go
entrypoint, the in-memory workspace, and the build harness.

## Verified end to end

A real browser/GPU run (evidence: `evidence/model.json`, `evidence/model.png`)
loaded the official Bonsai 2 2B adapter, read `README.md` through the real
`read_file` tool, wrote a 14-byte `proof.txt` through the real `write_file` tool,
streamed the final answer plus both tool calls into the genuine frontend, and
confirmed `proof.txt` via workspace introspection. Reset was verified to discard sessions and drafts; workspace files live only in
the terminated worker's memory. No API request left the browser.

## Developer agent and the Linux shell playground

The chat is backed by the genuine built-in `developer` agent prompt from
`internal/agents` (with `{{git_repo}}` expanded to the bounded `browser-bonsai`
label), plus a short browser-runtime appendix that lists the tools that actually
exist. It runs with the real `tools.ApprovalManager` in `ModeYolo`: every tool
call is auto-approved and there are no approval prompts. Session metadata reports
`agent: developer` and `approval_default_mode`/`approval_effective_mode: yolo`;
inference still comes from Bonsai in the page.

The seeded workspace includes `playground/bonsai.sh` (a shell + awk demo),
`playground/calculate.sh` with a deliberate bug, and `playground/tests.sh` that
fails on it. Ask the agent to "run sh playground/tests.sh, fix calculate.sh, then
run it again" to watch a real red → fix → green loop.

`shell` runs a command in the page's real Linux guest (v86) over the same
workspace: BusyBox `sh`, pipes, `awk`, `sed`, `grep`, coreutils, and CPython
3.12 as `python`/`python3`. The tested standard-library set is `json`, `csv`,
`statistics`, `unittest`, and `sqlite3`; `base64`, `binascii`, `gzip`, and
`zipfile` also work through the bundled zlib. Optional extension modules that
need unbundled shared libraries (`ssl`, `ctypes`, `bz2`, `lzma`, `readline`) are
unavailable; only musl, libsqlite3, and zlib are bundled. It is a real Linux
system, but it is not the host machine and has no network; there is no pip or
package installation, and `node` and Go are not installed. Tool arguments are
`command`, optional `working_dir` (always inside `/workspace`, default
`/workspace`), and optional `timeout_seconds` (default 10, maximum 30). The first
command may wait for the VM's lazy boot and one-time Python runtime install. The
guest returns the full post-execution file map; Go validates it (relative paths
only, at most 256 files / 8 MiB) and applies the snapshot diff to the same
`internal/memfs` workspace the real file tools use, so `read_file` and `glob`
immediately see files the shell wrote. `exit_code !== 0` is reported as a tool
error with the real stdout/stderr and duration.

## Build and run

```sh
# once, if the real frontend assets are absent
make frontend

npm --prefix experiments/browser-bonsai ci
./experiments/browser-bonsai/build.sh
go run ./experiments/browser-bonsai/cmd/serve \
  -dir experiments/browser-bonsai/dist -addr 127.0.0.1:8787
```

Output (`experiments/browser-bonsai/dist`, gitignored): the real
`internal/serveui` shell, `boot.mjs` + worker/transport, `term-llm.wasm` +
`wasm_exec.js`, the inference bundle, the hashed Vite assets, and
`runtime/python-root.tar.gz` with its generated pin/hash metadata and licenses.
The tiny `cmd/serve` is a development static-file server (`application/wasm`
MIME), not an application backend. `build.sh` expects `node`/`npm` and the
parent-owned `web/` sources; it runs `scripts/fetch-python-runtime.sh` if the
pinned CPython asset is absent. That script downloads the pinned Alpine packages,
verifies their SHA-256 digests, and writes `web/python-runtime.mjs` and
`web/runtime/python-runtime.json`.

## Go checks

```sh
go test ./experiments/browser-bonsai ./internal/tools
GOOS=js GOARCH=wasm go build ./experiments/browser-bonsai
make complexity
```

`app_test.go` drives the real engine and real `read_file`/`write_file` tools over
a temporary native workspace with scripted inference: tool round-trip, genuine
frontend request shape (`[{type:"message", content:...}]`), SSE event order,
transcript hydration, and cancellation.

## Architecture

- Go/WASM exposes `termLLMRequest(requestJSON, onEvent)` and
  `termLLMAbort(id)`; a real `http.Handler` serves a small `/v1` subset for the
  real frontend (capabilities, providers/models, sessions, streamed responses,
  cancel, transcript hydration).
- Go calls `termLLMInfer(requestJSON, onDelta)` for inference. It never
  fabricates model output and never overwrites the JS function; cancellation
  calls `termLLMCancelInference()`.
- The real `llm.Engine` runs its own tool loop, executing the real file tools
  against `internal/memfs`, an in-memory `/workspace`, and `shell`, which bridges
  to the page's v86 Linux guest over that same workspace. On first boot the page
  loads a pinned CPython 3.12 overlay from a same-origin static asset into the
  host-backed 9p store; Python runs from there without consuming guest RAM for
  its standard library.
- Go calls `termLLMRunShell(requestJSON)` for commands and
  `termLLMCancelShell()` on timeout/cancel. It never runs host commands; native
  tests inject a mock executor over a temporary directory.

## Limitations (intentional for the POC)

- Ephemeral: reload or reset discards sessions, drafts, and workspace files.
  Model weights may stay in the browser cache.
- Browser-only: `tools.SetWorkspaceFS` refuses native execution; there is no
  durable session database and SQLite is not registered in the WASM build.
- Tools: only `read_file`, `write_file`, `edit_file`, `glob`, and `shell`. No
  host shell, grep/glob CLI integration, git, worktree, spawn, network, or image
  tools; the only command execution is the sandboxed v86 guest. The guest has
  BusyBox and CPython 3.12 (tested: `json`, `csv`, `statistics`, `unittest`,
  `sqlite3`; `base64`/`gzip` work via bundled zlib; optional modules needing
  unbundled libraries such as `ssl`, `ctypes`, `bz2`, `lzma`, `readline` are
  unavailable), but no pip, package installation, `node`, or Go.
- One in-memory workspace and one session at a time; no projects, worktrees,
  Hub, MCP, sharing, live voice, push, or extension surface. Unimplemented API
  routes return explicit 404s.
- Text-only messages; no attachments. Requires WebGPU and enough memory for the
  model; the demo has no remote inference fallback.
- The frontend is the unmodified production build. The shell route is rebased by
  `web/boot.mjs`; do not change `TERM_LLM_UI_PREFIX` for APIs.
- No persistence, query-history paging, or server-side search beyond what the
  UI needs to boot and chat.

## Main build pipeline

The normal build targets and release/deployment steps are unchanged; the
experiment is opt-in via `experiments/browser-bonsai/build.sh`. Shared platform
compatibility seams were added for SQLite registration, process/filelock/PTY
support, the terminal runtime, and tool filesystem access. Native focused tests
pass; these changes have not been integrated or published.

## iPhone startup regression

The original browser build imported the server-side HTML export's Goldmark GFM
extension. Its global linkifier compiled `{1,256}` URL patterns into syntax trees
512 levels deep; Go's recursive regexp compiler overflowed the iPhone native WASM
stack before `main`. The phone reported repeated `wasm-function[8052]`; an
unstripped build with an identical code section mapped that to regexp/syntax's
compiler.

GFM HTML export extensions are now native-only; browser HTML fallback is
CommonMark, and the demo's chat renderer remains the original frontend. The POC
does not expose native HTML transcript export. Native HTML export tests retain
GFM unchanged. A transitive browser dependency test prevents reintroducing this
initializer.

After building, run `npm --prefix experiments/browser-bonsai run test:wasm-startup`.
This boots the real WASM with Node's host stack limited to 64 KB. The original
artifact fails with the same function-8052 stack; the fixed artifact starts. This
is a constrained-stack regression test, not a claim of physical iPhone testing.
