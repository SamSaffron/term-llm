# Codegen benchmark

Execution-based code generation benchmark for term-llm providers. It asks a provider to generate code, compiles/runs the result in an isolated temp directory, records correctness/perf signals, and estimates cost from token usage when pricing is known.

The first target is `claude-bin`; provider swaps are just a flag.

## Quick start

```bash
go run ./benchmarks/codegen \
  -provider claude-bin \
  -concurrency 2 \
  -budget 4h
```

Run fewer tasks while iterating:

```bash
go run ./benchmarks/codegen \
  -provider claude-bin \
  -tasks go_fizzbuzz,go_binary_search
```

Run the optional Zig task when `zig` is installed:

```bash
go run ./benchmarks/codegen \
  -provider claude-bin \
  -tasks zig_sum_positive_perf
```

`-tasks all` runs the default suite and intentionally excludes optional toolchain-dependent tasks like Zig.

Run another provider/model:

```bash
go run ./benchmarks/codegen -provider anthropic:claude-sonnet-4-6
go run ./benchmarks/codegen -provider openai:gpt-5.2
go run ./benchmarks/codegen -provider gemini:gemini-3-pro
go run ./benchmarks/codegen -provider ollama:qwen3-coder
```

## What it measures

Each task produces a JSON record with:

- compile/test pass/fail
- scalar score (`0.0` or `1.0` for the first suite)
- benchmark output where a task has a perf component
- parsed runtime/allocation metrics when the scorer can extract them
- input/output/cache/reasoning token counts when the provider reports them
- estimated USD cost using term-llm's LiteLLM pricing cache when the model can be matched
- generated code, stdout, stderr, and failure detail for postmortems

The summary includes cost and cost-per-pass. That number is intentionally crude but useful: a model that gets 5/5 for $0.03 and one that gets 5/5 for $3.00 are not the same beast.

The prompt explicitly tells every provider to self-validate before returning code: exact signature/export, imports/header, syntax, edge/error cases, concurrency safety, and stated perf constraints. The model still returns only code; the benchmark records whether that private check was worth a damn by running the scorer. Nice because it encourages agentic discipline without turning failures into a model-judged therapy session.

## Tasks

Current suite:

| Task | Language | Signal |
|---|---|---|
| `go_fizzbuzz` | Go | easy correctness |
| `go_binary_search` | Go | edge-case correctness |
| `go_json_format` | Go | stdlib/API correctness and error handling |
| `go_concurrent_counter` | Go | concurrency correctness under `-race` |
| `go_dedupe_perf` | Go | correctness plus `go test -bench -benchmem` output |
| `go_web_chat_1000` | Go | generated chat server exercised over real localhost HTTP by the common Go harness |
| `node_web_chat_1000` | JavaScript/Node | same common localhost HTTP harness against a generated Node stdlib server |
| `ruby_web_chat_1000` | Ruby | same common localhost HTTP harness against a Ruby stdlib adapter around the generated callable |
| `python_web_chat_1000` | Python | same common localhost HTTP harness against a Python stdlib adapter around the generated callable |
| `asm_sum_positive_perf` | x86-64 assembly | System V ABI assembly correctness plus warmed perf loop and max RSS memory |
| `zig_sum_positive_perf` | Zig | optional explicit task; exported C ABI function linked to the same C perf harness as assembly; requires `zig` installed |

The web chat tasks are intentionally heavier than the toy tasks and now use one common performance harness. Each generated solution is launched as a localhost HTTP server on `127.0.0.1:$PORT`; the same Go load driver sends the bad-input check, 100-post warmup, 1000 concurrent `POST /rooms/{room}/messages` requests, a validating `GET`, and the empty-room check. That means Go, Node, Ruby, and Python are measured through the same client, the same concurrency shape, and the same correctness assertions. Ruby/Python still generate a small callable to avoid framework lottery; checked-in stdlib adapters expose that callable over real HTTP for the shared harness.

Assembly and Zig are deliberately not pretending to be web stacks. They test whether the model can emit linkable low-level code that survives a C harness, then records warmed loop runtime and process RSS. Zig is registered as an explicit optional task (`-tasks zig_sum_positive_perf`) because this container/CI may not have `zig` installed. Different beast, same dashboard: quality, cost, speed, memory.

This is deliberately repo-local and boring to run. Add Ruby/Rails, SQL/Postgres, and TypeScript suites the same way: prompt, isolated workspace, deterministic scorer.

## Results

Artifacts are written to `benchmarks/codegen/results/` by default:

```text
benchmarks/codegen/results/YYYYMMDDTHHMMSSZ_provider-model.json
benchmarks/codegen/results/YYYYMMDDTHHMMSSZ_provider-model_dashboard.html
benchmarks/codegen/results/YYYYMMDDTHHMMSSZ_provider-model_dashboard.svg
benchmarks/codegen/results/latest_dashboard.html
benchmarks/codegen/results/latest_dashboard.svg
benchmarks/codegen/results/history.jsonl
```

The dashboard visualizes the three numbers that matter together:

- **quality**: pass/fail and scalar score
- **cost to generate**: estimated USD from token usage/pricing
- **performance**: post-warmup generated runtime metrics when available, otherwise scorer duration as a fallback
- **memory**: RSS/max-RSS or benchmark allocation metrics where available

The SVG is easy to paste into reports; the HTML adds summary cards and the sortable-by-eyeball result table. Bigger bubbles cost more, higher bubbles are faster, green bubbles passed, red bubbles failed. Yes, this is intentionally judgemental.

Use a throwaway output directory for experiments:

```bash
go run ./benchmarks/codegen -out /tmp/codegen-bench
```

## Flags and environment

Every important flag has an env var so the same command is easy to run from jobs.

| Flag | Env var | Default |
|---|---|---|
| `-provider` | `BENCH_PROVIDER` | `claude-bin` |
| `-tasks` | `BENCH_TASKS` | `all` |
| `-runs` | `BENCH_RUNS` | `1` |
| `-concurrency` | `BENCH_CONCURRENCY` | `2` |
| `-budget` | `BENCH_BUDGET` | `4h` |
| `-timeout` | `BENCH_TASK_TIMEOUT` | `5m` |
| `-score-timeout` | `BENCH_SCORE_TIMEOUT` | `60s` |
| `-out` | `BENCH_OUT` | `benchmarks/codegen/results` |

## Running via term-llm jobs

Create a manual program job:

```bash
term-llm jobs create --file benchmarks/codegen/jobs/codegen-benchmark.json
```

Trigger it:

```bash
term-llm jobs trigger codegen-benchmark
```

The checked-in job uses:

```text
BENCH_PROVIDER=claude-bin
BENCH_CONCURRENCY=2
BENCH_BUDGET=4h
```

To benchmark another provider, either run the command directly with `BENCH_PROVIDER=...`, or update the job definition with a different environment value.

## Adding a task

1. Add a `Task` implementation in `tasks.go` or split it into a new file.
2. Register it in `allTasks()`.
3. Make `Score()` compile/run something deterministic.
4. Prefer real execution signals over model-judged grading. If a result cannot fail mechanically, it is probably a vibes benchmark wearing a fake moustache.
