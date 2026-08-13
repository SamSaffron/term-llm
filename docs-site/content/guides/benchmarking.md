---
title: "Benchmarking providers"
weight: 18
description: "Measure client-observed LLM latency, decode throughput, and effective prefill performance."
kicker: "Performance"
source_readme_heading: "Benchmarking providers"
---

`term-llm benchmark` measures provider inference directly at the `llm.Provider` and `llm.Stream` boundary. It bypasses the agent engine, tools, sessions, and search.

```bash
term-llm benchmark --provider cdck_deepseek
```

The command prints the requested token budget, starts immediately, and uses one live status line while it runs. Press `Esc` or `Ctrl+C` to cancel. In CI, redirected output, or a dumb terminal, it runs without the interactive spinner.

## Default balanced profile

The default `balanced` profile exercises both decode and prefill behavior:

| Workload | Input target | Output target |
|---|---:|---:|
| Decode | 2,000 | 128 |
| Prefill | 4,000 | 16 |
| Prefill | 16,000 | 16 |
| Prefill | 64,000 | 16 |

Each scenario uses one warmup and three measured runs. HTTP/API providers also receive a calibration request and a validation request before measurement. A measured cold-cache request may be retried once with a fresh payload when reported cache telemetry shows contamination.

The three default prefill lengths allow the balanced profile to report an effective prefill estimate. When the points do not all fall inside one predefined context range, the report labels the estimate `observed span`.

Other profiles are available with `--mode quick`, `decode`, `prefill`, and `long-context`. Use `--input-tokens` and `--output-tokens` for explicit scenarios.

## Reading the report

The human report leads with a per-target comparison table of median activity TTFT, decode rate, and effective input, then the prefill fit. Scenario cards under that summary add ranges, token accounting, and any incomplete validity counts.

All timing is client-observed. It includes network, queueing, transport, and provider overhead; it is not server-side phase telemetry.

- **Activity TTFT** is time to the first streamed activity, including reasoning activity.
- **Visible TTFT** is time to the first visible text. It can be unavailable when a short completion contains no visible text event.
- **End to end** measures the full request at the provider stream boundary.
- **Decode rate** uses provider-reported output tokens over the observed incremental decode window.
- **TPOT** is observed time per output token.
- **Effective input** divides computed input tokens by activity TTFT. It is a client-observed rate, not server prefill telemetry.
- **Effective prefill fit** estimates the slope across at least three fit-valid prefill lengths using median pairwise slopes.

Values shown first are medians. Parenthesized values are the minimum and maximum across valid runs; p95 appears when the sample count is large enough.

## Validity and cache handling

The report separates three validity counts. They are omitted from a scenario card when every measured run is valid:

- **latency** runs are usable for latency and provider-token summaries;
- **fit** runs are also safe for effective-input and prefill fitting;
- **decode** runs have a comparable incremental decode window.

Cold-cache measurements use provider-reported cache-read telemetry when the adapter exposes it. vLLM reports prefix-cache reads through the OpenAI-compatible `prompt_tokens_details.cached_tokens` field. A reported zero is treated as a cache miss; an adapter that does not report cache reads leaves cache state unknown.

Use `--allow-unknown-cache` only when you intentionally want unknown-cache runs admitted to token fits. Results remain labeled accordingly.

Cache-write tokens count as computed input. Billing treatment is provider-specific and is not inferred by the benchmark.

## Context safety

Requests are skipped when they exceed a known input or configured context limit. Local Ollama benchmarks use configured `num_ctx` when available and otherwise apply a conservative safety floor.

`--assume-context-limit` is an expert-only eligibility override for local Ollama. It does not configure the server, verify its context window, or make truncated requests valid.

Long-context mode requires a known input limit unless explicit input targets are supplied.

## Adapter limitations

- Measurements are cold and sequential; warm-cache and concurrent workloads are not implemented in schema version 1.
- Managed CLI providers require `--include-managed-provider`. Their timing is subprocess-inclusive and is not directly comparable to direct HTTP adapters.
- Non-incremental or bursty adapters cannot provide comparable decode throughput or TPOT. Provider-reported output usage and end-to-end timing are still recorded.
- Some adapters cannot enforce the requested output ceiling. Their report records actual output usage and labels the ceiling as requested rather than enforced.
- Transport chunks are never treated as token boundaries; throughput requires provider-reported token counts.

## Machine-readable output

Use `--json` for one complete report or `--jsonl PATH` to append each raw request record as it completes:

```bash
term-llm benchmark -p cdck_deepseek --json > benchmark.json
term-llm benchmark -p cdck_deepseek --jsonl benchmark-runs.jsonl
```

Progress is kept off stdout so JSON remains valid. Use `--dry-run` to inspect resolved targets and the maximum request budget without dispatching inference.
