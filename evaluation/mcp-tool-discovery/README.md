# MCP tool-discovery evaluation

This directory contains the reproducible evaluation workflow required for the MCP catalogue/deferred-discovery feature. Generated state and transcripts belong only in a dated scratch directory; none are written into the repository.

## Controlled federation

`internal/tooldiscovery/evalfixture` implements both the original ten independent stdio MCP servers with twenty realistic tools each and a single aggregate stdio MCP server exposing the same 200 tools. The domains are source control, issue tracking, team chat, documents, observability, SQL analytics, cloud infrastructure, browser automation, commerce/CRM, and calendar/mail. Ten-server mode remains the default for the existing catalogue-boundary matrix; `generate-config --aggregate --size 200` is the measured real-world one-server shape.

The federation provides:

- realistic nested input and structured-output schemas;
- read and mutating annotations (annotations remain retrieval hints, not approval policy);
- deterministic disposable state and append-only `calls.jsonl` execution logs;
- catalogue profiles at 6, 12, 18, 24, 25, 32, 42, 64, 100, and 200 tools;
- five-item pagination on the source-control server;
- actual stdio MCP connection, pagination, namespacing, search, activation, and execution paths;
- a deterministic unique `oracle:v1:...` value in every tool's structured result and JSON text-compatible result, with `oracle` required by every output schema.

`tasks.json` defines the original 31 execution-log tasks for the ten-server boundary matrix. `oracle_tasks.json` defines ten single-aggregate-server capability-selection tasks, one per domain. Their ordinary prompts describe the desired operation without leaking the exact provider-prefixed tool name. A task scores the semantically required external call and an exact final-output match against that tool's oracle independently; overall success requires both and rejects wrong external calls.

## Single-server oracle and cache pilot

`run_oracle_evaluation.py` is the reproducible real-world experiment requested for the aggregate server. Its default, explicitly anecdotal pilot predeclares the same three confusable/boundary-spanning oracle tasks, `mode: auto`, threshold 24, ChatGPT `portable` versus `native`, a Qwen `portable` regression, and three repetitions: 18 measured ChatGPT runs plus 9 Qwen runs. Every strategy receives the same seeded task/repetition order.

For ChatGPT native runs, selected MCP children are emitted as Responses namespace objects and coalesced only with other selected children from the same namespace; unselected siblings remain absent. Portable runs retain flattened `server__tool` functions. The protocol audit understands both shapes and continues to require a stable native top-level tool array.

```sh
python3 evaluation/mcp-tool-discovery/run_oracle_evaluation.py
```

Defaults are deliberately pinned to the already-selected models:

- local `qwen36a3bq3:qwen36-a3b-q3-131072:latest` (`think: false`, 131072 context);
- `chatgpt:gpt-5.6-luna-medium` with WebSocket behavior recorded in the environment.

The runner never edits canonical config and never copies OAuth material into scratch. For ChatGPT it creates an ephemeral temporary XDG config and symlinks the existing OAuth file only for the life of the process. It aborts if the canonical config hash changes. Mode configs, MCP config, catalogue, transcripts, raw search-query retrieval results, cold warm-ups, measured runs, environment metadata, `summary.json`, and `report.md` are retained under `~/scratch/YYYY-MM-DD-mcp-tool-discovery-oracle-200/`.

For prompt-cache comparison, ChatGPT receives the same deterministic 64,000-byte shared opening used by the prior pilot. It calibrates to 9,928–9,950 provider-observed initial tokens in the current selected tasks (portable mean 9,938; native mean 9,940). The runner records SHA-256, byte/word counts, and term-llm's explicitly approximate bytes/4 constructed estimate; it does **not** label either estimate an exact tokenizer count. Provider-observed initial-turn input/cache/write counters are reported separately from all-turn totals. One cold request per strategy is stored separately in `warmups.jsonl` before the identical seeded measured order. ChatGPT runs add `--debug-raw` only to the isolated evaluation process; secret-free parser summaries record request item types, previous-response use, top-level tool signatures, and whether a loaded schema leaked into top-level tools, while full raw stderr remains in the dated transcript. The report discloses implicit cache routing/eviction and within-run ChatGPT WebSocket continuation as distinct confounders and does not infer a cache benefit from all-turn cached counters.

Use `--providers qwen` or `--providers chatgpt` for a model-specific diagnostic run, and `--all-tasks` for all ten oracle tasks. `--repetitions` may be increased but cannot be set below three for scored pilots. If an external timeout interrupts the long live matrix, rerun the identical command with `--resume`; the runner verifies the recorded seeded order and strategy matrix, preserves cold warm-ups/raw rows, and executes only missing provider/strategy/ordinal cases.

### 2026-08-12 comparable pilot

The completed secret-free artifact is `~/scratch/2026-08-12-mcp-tool-discovery-oracle-200-native/`. All 27 measured cases made the exact required call, returned the exact oracle, and made no wrong calls. ChatGPT native and portable each passed 9/9; Qwen portable passed 9/9. Native emitted 9 matched client search/output pairs, used zero fallbacks, kept the top-level tool array stable in 9/9 wire audits, and never placed a loaded schema in that array. Portable changed its top-level array after ordinary `tool_search`, as expected. Required retrieval rank was 1 in every run.

The calibrated opening produced mean initial provider-observed input of 9,938 tokens for ChatGPT portable and 9,940 for native. Portable had 9,728 initial cached tokens total (one observed hit); native had zero. Both had zero cache-write tokens. All-turn cached totals were 88,576 portable and 184,320 native, but native's count includes successful `previous_response_id` continuation on each follow-up and loaded-tool context. The run therefore makes **no prompt-cache-key or independent prefix-cache benefit claim**. Median latency was 7.810s portable and 7.092s native in this anecdotal three-repetition/task sample.

## Local Qwen sweep

Prepare a **separate evaluation-only** config template containing credentials/base URLs for the local provider. Do not point the workflow at the normal term-llm config. The template must not contain a `tool_discovery` key because the workflow appends the measured mode and threshold.

```sh
export EVAL_BASE_CONFIG=/path/to/disposable-qwen-config.yaml
export EVAL_PROVIDER=ollama                 # or vllm/openai-compatible provider key
export EVAL_MODEL=qwen3:30b-a3b             # exact local model ID
export EVAL_MODEL_DIGEST=sha256:...          # immutable runtime/model digest
export EVAL_QUANTISATION=Q4_K_M
export EVAL_CONTEXT_LIMIT=32768
export EVAL_PROVIDER_ENDPOINT=http://127.0.0.1:11434
export EVAL_TEMPERATURE=0
export EVAL_TOOL_CALL_SETTINGS='native tool calls; temperature 0'

python3 evaluation/mcp-tool-discovery/run_qwen_evaluation.py
```

The full run:

- builds the current checkout and fixture binary;
- creates `~/scratch/YYYY-MM-DD-mcp-tool-discovery-eval/{config,fixtures,repos,databases,documents,results,transcripts}`;
- records commit/dirty state, exact model/provider metadata, host/GPU, sampling settings, warm-up, repetitions, and seed;
- compares eager, deferred, and auto thresholds 16/20/24/28/32 at every required catalogue size;
- runs every applicable task three times in deterministic randomized order after one warm-up;
- resets mutable fixture state before each run;
- records raw JSONL results, execution-derived ordered success/wrong calls/final-state checks, wall time, provider usage, search calls and queries, production-index Recall@5 for those queries, initial and activated schema token estimates, and full transcripts;
- writes aggregate `results/summary.json` without claiming statistical significance.

The activated-token metric is the unique set loaded by observed `tool_search` calls, reproduced through the production index and 16-tool budget. It is not presented as provider-billed usage or as a cumulative-per-request token count. Provider token fields come only from emitted provider usage events.

## Evaluation status

The checked-in files are the reproducible harness and deterministic corpus, not a claim that the local-Qwen or ten-real-server runs have already happened. No generated result table or transcript is committed here. A review artifact is complete only when a dated scratch run supplies `environment.json`, `runs.jsonl`, `summary.json`, and representative transcripts; report an unrun or unavailable model/server smoke explicitly rather than inferring results from deterministic retrieval.

For a predeclared, comparable **pilot/anecdotal** sweep across the requested
6/24/25/42/200 boundaries, with one repetition of eager, deferred, and auto at
threshold 24, run:

```sh
python3 evaluation/mcp-tool-discovery/run_qwen_evaluation.py --pilot
```

The pilot uses the task IDs recorded in `PILOT_TASK_IDS_BY_SIZE`, keeps settings
identical across modes within each catalogue size, and labels both environment
and summary output as incomplete anecdotal evidence. It does not replace the
full matrix.

For an infrastructure-only smoke (not threshold evidence):

```sh
python3 evaluation/mcp-tool-discovery/run_qwen_evaluation.py --quick
```

The provisional production threshold remains 24 unless the comparable full result table justifies a change under the rules in `spec.md`.

## Real-server schema smoke

Public MCP implementations and credentials drift. Supply an explicit disposable `mcp.json` containing exactly ten pinned package/image versions and a metadata JSON documenting versions, workspace/database roots, safety restrictions, and substitutions. Then run:

```sh
python3 evaluation/mcp-tool-discovery/run_real_server_smoke.py \
  --mcp-config ~/scratch/YYYY-MM-DD-mcp-tool-discovery-eval/config/real-mcp.json \
  --metadata ~/scratch/YYYY-MM-DD-mcp-tool-discovery-eval/config/real-mcp-metadata.json
```

The metadata must cover the GitHub, Playwright, Chrome DevTools, filesystem, Git, PostgreSQL, SQLite, fetch, memory, and time categories. Only disposable/public targets are permitted. The report records actual counts and schema hashes and does not claim the real-server set totals 200.

## Deterministic checks

```sh
go test ./internal/tooldiscovery/... ./internal/mcp

go test -race ./internal/tooldiscovery ./internal/mcp
```

An offline 200-tool transport/retrieval smoke (no model call) can be recorded in the same dated scratch layout:

```sh
SCRATCH="$HOME/scratch/$(date +%F)-mcp-tool-discovery-eval"
mkdir -p "$SCRATCH"/{bin,profiles/200,results}
go build -o "$SCRATCH/bin/mcp-discovery-eval" ./internal/tooldiscovery/evalcmd
"$SCRATCH/bin/mcp-discovery-eval" init --root "$SCRATCH"
"$SCRATCH/bin/mcp-discovery-eval" generate-config \
  --root "$SCRATCH" --binary "$SCRATCH/bin/mcp-discovery-eval" --size 200 \
  --output "$SCRATCH/profiles/200/mcp.json"
"$SCRATCH/bin/mcp-discovery-eval" catalogue \
  --mcp-config "$SCRATCH/profiles/200/mcp.json" \
  --output "$SCRATCH/profiles/200/catalogue.json"
"$SCRATCH/bin/mcp-discovery-eval" retrieval \
  --manifest "$SCRATCH/profiles/200/catalogue.json" \
  --tasks evaluation/mcp-tool-discovery/retrieval.json \
  --output "$SCRATCH/results/retrieval-200.json"
go test ./internal/tooldiscovery -run '^$' \
  -bench 'Benchmark(IndexBuild|Search)10000' -benchmem -count=5 \
  > "$SCRATCH/results/index-benchmarks.txt"
```

The fixture shape and required profile totals are regression-tested in `internal/tooldiscovery/evalfixture/domains_test.go`. Retrieval output uses the production lexical index through `mcp-discovery-eval retrieval`. Offline retrieval and benchmark output validate the deterministic plumbing and performance target only; they do not substitute for model task success, Qwen threshold evidence, or the real-server schema smoke.
