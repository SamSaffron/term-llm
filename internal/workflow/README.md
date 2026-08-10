# Lua workflows

`term-llm workflow` runs an explicit Lua file as a temporary team of agents. It
is deliberately a small orchestration surface, not a workflow database or
scheduler: files live in Git, runs live in the invoking process, and a stopped
process is not resumed.

## Example

```lua
workflow {
  name = "regression-review",
  description = "Ask isolated specialists for executable evidence",
  inputs = { source = "Source tree to review" },
}

local source = input("source")
local workspace = create_workspace {
  source = source,
  root = "/tmp/term-llm-workflows",
  name = "concurrency-review",
}

local results = parallel_settled {
  run_agent {
    label = "race-reviewer",
    system = "Find one concurrency bug and prove it with a regression test.",
    prompt = "Inspect the package in your current directory.",
    tools = { "read_file", "write_file", "shell" },
    read_dirs = { workspace },
    write_dirs = { workspace },
    shell_allow = { "go test *", "gofmt *" },
    cwd = workspace,
    max_turns = 12,
    require = {
      command = "go test -race .",
      exit_code = 1,
      repetitions = 3,
      output_contains = "FAIL",
      artifact_glob = "*_test.go",
    },
  },
}

return results
```

The invocation grants the maximum capabilities the Lua file may use:

```sh
term-llm workflow validate review.lua

term-llm workflow run review.lua \
  --input source="$PWD" \
  --workspace-root /tmp/term-llm-workflows \
  --agent-tool read_file,write_file,shell \
  --agent-read-dir /tmp/term-llm-workflows \
  --agent-write-dir /tmp/term-llm-workflows \
  --agent-shell-allow 'go test *' \
  --agent-shell-allow 'gofmt *' \
  --concurrency 4 \
  --json
```

## Lua surface

- `workflow { name=..., description=..., inputs={...}, phases={...} }` declares
  metadata. `workflow` must be the first statement.
- `input(name[, default])` reads an input supplied by `--input` or
  `--input-json`.
- `agent { prompt=..., agent=..., provider=..., label=... }` creates a lazy,
  one-shot model task.
- `run_agent { ... }` creates a lazy tool-using agent. It accepts `system`,
  `prompt`, `agent`, `provider`, `label`, `tools`, `read_dirs`, `write_dirs`,
  `shell_allow`, `cwd`, `max_turns`, and `require`.
- `create_workspace { source=..., root=..., name=... }` copies a source tree to
  an authorized destination and returns its path.
- `await(task)` executes one lazy task.
- `parallel { task, ... }` executes tasks with bounded concurrency, preserves
  input order, and fails when any task fails.
- `parallel_settled { task, ... }` preserves every outcome as
  `{ ok, result, error }`.
- `join(values, separator)` joins task output for simple reductions.
- The top-level returned Lua value becomes the command result.

A successful `run_agent` result is structured data containing its final text,
exit reason, actual tool calls, completion-command evidence, and hashed
artifacts. The harness runs `require.command` after the agent exits. Evidence
must match the expected exit code and output for every repetition; model prose
cannot satisfy the contract. Valid evidence may satisfy the task even if the
agent itself exhausted its turn budget.

## Capability boundary

Lua has no ambient filesystem, process, package-loading, clock, or randomness
APIs. Dynamic agents may only narrow the ceilings supplied by:

- `--agent-tool`
- `--agent-read-dir`
- `--agent-write-dir`
- `--agent-shell-allow`
- `--workspace-root`

A workflow cannot grant itself a missing tool, path, shell pattern, or workspace
destination. Child agents run non-interactively only after this intersection is
checked.

This policy is capability control, not an OS sandbox. An allowed command such
as `go test *` can compile and execute arbitrary code from the workspace. Use
containers or another operating-system sandbox when running untrusted source or
untrusted workflow files.

## Non-goals

The initial surface intentionally has no CRUD registry, SQLite store, TUI,
scheduler, pause/resume, replay, or crash recovery. `workflow run <path.lua>`
and `workflow validate <path.lua>` operate directly on files so storage and
versioning remain ordinary Git concerns.
