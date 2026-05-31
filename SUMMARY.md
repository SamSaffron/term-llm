# Jobs v2 — native (`llm`) runner parity with `ask`

Branch: `feat/jobs-llm-parity`

Implements `designs/term-llm/job-runner-parity.md`: bring the jobs v2 **native
`llm` runner** to parity with the `ask` CLI by adding optional config toggles,
while keeping the `program` runner as the escape hatch. Every new field is
optional; omitting it preserves today's exact behavior.

## What changed

### New `jobsV2LLMConfig` fields (`cmd/serve_jobs_v2.go`)

All optional, all `omitempty`:

| field | meaning |
|-------|---------|
| `provider` | provider override, e.g. `"chatgpt"` or `"chatgpt:gpt-5.5-xhigh"` |
| `model` | model override (alternative to `provider:model`) |
| `cwd` | per-run tool root (file roots + shell `exec.Cmd.Dir`) — **never `os.Chdir`** |
| `read_dir` | additional read roots |
| `write_dir` | additional write roots |
| `tools` | tool-set override (`"all"` or csv) |
| `max_turns` | agentic turn cap |
| `max_output_tokens` | output-token cap (0 = provider default) |
| `files` | context files attached to the prompt (like `ask -f`) |
| `search` | enable web search |
| `system_message` | system-prompt override |
| `skills` | skills selection |

These are mapped onto the **same execution path `ask` already uses** — this is
plumbing, not new concepts.

### How options are threaded

Two small, well-named, independently testable helpers were extracted in
`cmd/serve_jobs_v2.go` and are called by `newServeJobsExecutor`:

- `applyJobLLMProviderModel(...)` — applies the job's provider/model using the
  **existing** override machinery (`applyProviderOverridesWithAgent` +
  `applyAgentModelOverride`, the same path `ask`/`spawn_runner.go` use). The
  job's `provider` behaves like `ask --provider` (it may carry a `provider:model`
  form and takes precedence over the serve-level `--provider`); `model` is applied
  last as the most-specific selector.
- `resolveJobLLMSettings(...)` — merges the job config with serve-level defaults
  into the final `SessionSettings` + user prompt. Omitted fields fall back to the
  serve flag, so existing jobs resolve to identical settings. `files` are read and
  attached to the user message via the shared `prompt.AskUserPrompt`. The request
  now also forwards `max_output_tokens`.

### The `cwd` constraint: no `os.Chdir` (the one sharp rule)

The jobs server runs many runs concurrently in **one process**, so a global
`os.Chdir` would race every other run and the server itself. `cwd` is therefore
implemented as **"root this run's file/shell tools at this directory"**:

- `cwd` is appended to the run's read/write tool roots (so `read_file`,
  `write_file`, `edit_file`, `grep`, `glob` are authorized within it), and
- a new `ToolConfig.ShellWorkingDir` (threaded via `SessionSettings.ShellWorkingDir`)
  becomes the shell tool's default `exec.Cmd.Dir` when a call omits `working_dir`.
  An explicit `working_dir` on a shell call still wins.

No `os.Chdir` is introduced anywhere. A test captures `os.Getwd()` before/after a
full run and asserts it is unchanged.

Files touched for this mechanism:
- `internal/tools/config.go` — `ToolConfig.ShellWorkingDir` (+ `Merge`)
- `internal/tools/shell.go` — shell tool honors `ShellWorkingDir` as the default
- `cmd/session.go` — `SessionSettings.ShellWorkingDir` → `SetupToolManager`

### Deep-copy isolation

`cloneConfigForServeJob` was deepened to deep-copy each `ProviderConfig`'s
slice/pointer fields (`Models`, `UseNativeSearch`, `OAuthCreds`), mirroring the
discipline already in `spawn_runner.go`, so per-run provider/model overrides can
never mutate the shared base config. A test asserts this isolation.

## Scope note (v1)

Per the design's §3 fallback: `cwd` roots the file tools via the read/write-dir
**roots** and roots the shell tool via `exec.Cmd.Dir`. Relative paths passed to
the file tools still resolve against the process working directory (the file
tools have no per-call base dir, and a process `chdir` is forbidden here). In
practice agents use absolute paths or the shell for path-relative work, and the
shell — the common case for the motivating worktree scenario (build/test/git) — is
correctly rooted at `cwd`. Rerooting relative file-tool paths without a global
chdir is left as a follow-up.

## Backward compatibility

- Every new field is optional and defaults to current behavior.
- Existing job definitions deserialize unchanged; `omitempty` keeps the on-disk
  format identical for jobs that don't set the new fields.
- No change to the `program` runner, the API surface, or run/event schemas beyond
  additive config.

## Tests

Table-driven, matching `cmd/serve_jobs_v2_test.go` patterns
(`cmd/serve_jobs_v2_parity_test.go`, `internal/tools/shell_test.go`):

- **config round-trip** — new fields round-trip; omitted fields → current
  defaults; `omitempty` verified.
- **provider/model override** — `provider`, `provider:model`, model-only, combine,
  and serve-flag fallback — **without mutating shared config** (deep-copy isolation
  asserted, including provider slice/pointer fields).
- **cwd routing with NO `os.Chdir`** — `cwd` → read/write roots + `ShellWorkingDir`;
  `os.Getwd()` asserted unchanged across resolution.
- **shell `Dir == cwd`** — the shell tool runs in `ShellWorkingDir` when
  `working_dir` is omitted; an explicit `working_dir` still wins.
- **ask options reach the exec opts** — `max_turns`, `max_output_tokens`, `tools`,
  `read_dir`/`write_dir`, `search`, `system_message`, `files`, plus backward-compat
  fallbacks to serve-level defaults.
- **skills threading** — the job's `skills` value drives skill setup and takes
  precedence over the agent's (and falls back to the agent when empty).
- **focused integration test** — a program-equivalent task expressed as an `llm`
  job with `cwd` set (driven through the **real** serve-jobs executor with the
  hermetic `debug` provider and a temp agent enabling `shell`) creates its output
  file rooted in `cwd`, and the process working directory is unchanged.

## Incidental fix (separate commit)

While bringing `go vet ./...` fully green (a required gate), two pre-existing
`lostcancel` leaks in `cmd/progressive.go` and three pre-existing test-file vet
warnings in `internal/llm` were fixed. These pre-date this branch and are isolated
in their own commit; the production fixes in `progressive.go` are behavior-
preserving (the derived contexts' cancel funcs are now released on return instead
of leaking until the deadline).

## Gates

`gofmt -l` (clean) · `go build ./...` · `go vet ./...` · `go test ./...` — all
green. Race detector clean on the jobs/progressive and tools packages.

## Motivating case, now first-class

```json
{
  "name": "worktree-tui-opus",
  "runner_type": "llm",
  "runner_config": {
    "agent_name": "developer",
    "provider": "claude-bin:opus-max",
    "cwd": "/home/agent/source/term-llm-wt/worktree-tui-opus",
    "max_turns": 400,
    "files": ["/home/agent/designs/term-llm/tui-worktree.md"],
    "instructions": "Implement the attached spec. Work to completion."
  }
}
```

…with full session linkage, progressive state, and the event timeline that the
`program` escape hatch can't give us.
