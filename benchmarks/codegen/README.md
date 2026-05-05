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
- input/output/cache/reasoning token counts when the provider reports them
- estimated USD cost using term-llm's LiteLLM pricing cache when the model can be matched
- generated code, stdout, stderr, and failure detail for postmortems

The summary includes cost and cost-per-pass. That number is intentionally crude but useful: a model that gets 5/5 for $0.03 and one that gets 5/5 for $3.00 are not the same beast.

## Tasks

Current suite:

| Task | Language | Signal |
|---|---|---|
| `go_fizzbuzz` | Go | easy correctness |
| `go_binary_search` | Go | edge-case correctness |
| `go_json_format` | Go | stdlib/API correctness and error handling |
| `go_concurrent_counter` | Go | concurrency correctness under `-race` |
| `go_dedupe_perf` | Go | correctness plus `go test -bench -benchmem` output |

This is deliberately repo-local and boring to run. Add Ruby/Rails, SQL/Postgres, Python, and TypeScript suites the same way: prompt, isolated workspace, deterministic scorer.

## Results

Artifacts are written to `benchmarks/codegen/results/` by default:

```text
benchmarks/codegen/results/YYYYMMDDTHHMMSSZ_provider-model.json
benchmarks/codegen/results/history.jsonl
```

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
| `-score-timeout` | `BENCH_SCORE_TIMEOUT` | `20s` |
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
