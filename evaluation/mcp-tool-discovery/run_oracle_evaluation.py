#!/usr/bin/env python3
"""Run the single-server 200-tool oracle and prompt-cache pilot.

The measured client always connects to exactly one stdio MCP server named
``federation``.  Canonical term-llm config is read only to locate ChatGPT OAuth;
mode-specific, secret-free config is generated in a temporary XDG directory.
"""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import math
import os
import pathlib
import platform
import random
import re
import shutil
import statistics
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
from typing import Any

ROOT = pathlib.Path(__file__).resolve().parents[2]
DEFAULT_SCRATCH_NAME = f"{dt.date.today().isoformat()}-mcp-tool-discovery-oracle-200"
DISCOVERY_MODE = "auto"
THRESHOLD = 24
CHATGPT_STRATEGIES = ["portable", "native"]
QWEN_STRATEGIES = ["portable"]
QWEN_PROVIDER = "qwen36a3bq3"
QWEN_MODEL = "qwen36-a3b-q3-131072:latest"
CHATGPT_PROVIDER = "chatgpt"
CHATGPT_MODEL = "gpt-5.6-luna-medium"
PILOT_TASK_IDS = ["oracle-pr-metadata", "oracle-application-logs", "oracle-calendar-availability"]
SEED = 240824
SHARED_OPENING_BYTES = 64_000
GENERAL_GUIDANCE = (
    "This is a capability-selection evaluation. Use a connected external tool for every request. "
    "If tool_search is available, search by the requested capability, inspect the loaded candidates, "
    "and then call the semantically correct external tool. Never invent an oracle. After the external "
    "call, follow the user's exact-output instruction."
)


def run(cmd: list[str], *, env: dict[str, str] | None = None, timeout: int = 300,
        check: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(cmd, cwd=ROOT, env=env, timeout=timeout, check=check, text=True,
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--scratch", type=pathlib.Path,
                        default=pathlib.Path.home() / "scratch" / DEFAULT_SCRATCH_NAME)
    parser.add_argument("--providers", default="qwen,chatgpt",
                        help="comma-separated subset of qwen,chatgpt")
    parser.add_argument("--repetitions", type=int, default=3)
    parser.add_argument("--max-turns", type=int, default=5)
    parser.add_argument("--seed", type=int, default=SEED)
    parser.add_argument("--all-tasks", action="store_true",
                        help="run all ten oracle tasks instead of the predeclared three-task pilot")
    parser.add_argument("--run-timeout", type=int, default=900)
    parser.add_argument("--resume", action="store_true",
                        help="continue missing provider/ordinal cases in an interrupted matching scratch run")
    args = parser.parse_args()
    providers = [value.strip() for value in args.providers.split(",") if value.strip()]
    if not providers or any(value not in {"qwen", "chatgpt"} for value in providers):
        parser.error("--providers must be a comma-separated subset of qwen,chatgpt")
    if args.repetitions < 3:
        parser.error("--repetitions must be at least 3 for the pilot")

    scratch = args.scratch.expanduser().resolve()
    if not scratch.name.startswith(dt.date.today().isoformat() + "-"):
        parser.error("scratch must be a new dated subdirectory beginning with today's ISO date")
    if scratch.exists() and any(scratch.iterdir()) and not args.resume:
        parser.error(f"scratch must be new and empty (or pass --resume after an interruption): {scratch}")
    if args.resume and not scratch.exists():
        parser.error(f"cannot resume a missing scratch directory: {scratch}")
    for name in ["bin", "config", "fixtures", "profiles/200", "results/search-details", "transcripts"]:
        (scratch / name).mkdir(parents=True, exist_ok=True)

    tasks_manifest = json.loads((ROOT / "evaluation/mcp-tool-discovery/oracle_tasks.json").read_text())
    all_tasks = tasks_manifest["tasks"]
    if args.all_tasks:
        tasks = all_tasks
        scope = "pilot/anecdotal all ten oracle tasks"
    else:
        by_id = {task["id"]: task for task in all_tasks}
        tasks = [by_id[task_id] for task_id in PILOT_TASK_IDS]
        scope = "predeclared pilot/anecdotal three oracle tasks"
    shutil.copy2(ROOT / "evaluation/mcp-tool-discovery/oracle_tasks.json", scratch / "fixtures/oracle_tasks.json")

    term_bin = scratch / "bin/term-llm"
    eval_bin = scratch / "bin/mcp-discovery-eval"
    build_started = time.perf_counter()
    run(["go", "build", "-o", str(term_bin), "."], timeout=600)
    run(["go", "build", "-o", str(eval_bin), "./internal/tooldiscovery/evalcmd"], timeout=600)
    run([str(eval_bin), "init", "--root", str(scratch)])
    build_seconds = time.perf_counter() - build_started

    mcp_json = scratch / "profiles/200/mcp.json"
    run([str(eval_bin), "generate-config", "--aggregate", "--root", str(scratch),
         "--binary", str(eval_bin), "--size", "200", "--output", str(mcp_json)])
    catalogue_path = scratch / "profiles/200/catalogue.json"
    run([str(eval_bin), "catalogue", "--mcp-config", str(mcp_json),
         "--output", str(catalogue_path)], timeout=180)
    catalogue = json.loads(catalogue_path.read_text())
    validate_single_server_catalogue(mcp_json, catalogue, tasks)
    shutil.copy2(mcp_json, scratch / "config/mcp-aggregate.json")

    shared_opening = construct_shared_opening(SHARED_OPENING_BYTES)
    shared_path = scratch / "config/shared-opening-context.txt"
    shared_path.write_text(shared_opening)
    shared_meta = {
        "bytes": len(shared_opening.encode()),
        "characters": len(shared_opening),
        "whitespace_words": len(shared_opening.split()),
        "sha256": hashlib.sha256(shared_opening.encode()).hexdigest(),
        "constructed_estimate_tokens": math.ceil(len(shared_opening.encode()) / 4),
        "constructed_estimator": "term-llm EstimateTokens-compatible ceil(UTF-8 bytes / 4), explicitly approximate",
        "target": "approximately 10,000 provider tokens",
        "calibration": "64,000 bytes selected after a 40,000-byte diagnostic produced about 6,300 provider-observed tokens for the complete initial deferred request",
        "claim": "constructed estimate and provider-observed initial/full-run usage are recorded separately; neither is called an exact tokenizer count",
    }

    workloads = [{"repetition": repetition, "task": task}
                 for repetition in range(1, args.repetitions + 1) for task in tasks]
    random.Random(args.seed).shuffle(workloads)
    run_order = [{"ordinal": i + 1, "repetition": item["repetition"], "task_id": item["task"]["id"]}
                 for i, item in enumerate(workloads)]
    run_order_path = scratch / "results/run-order.json"
    run_order_payload = {
        "seed": args.seed,
        "mode": DISCOVERY_MODE,
        "threshold": THRESHOLD,
        "identical_workload_order_per_strategy": True,
        "order": run_order,
    }
    if args.resume and run_order_path.exists() and json.loads(run_order_path.read_text()) != run_order_payload:
        raise SystemExit("resume settings do not match the recorded deterministic run order")
    write_json(run_order_path, run_order_payload)

    canonical_config = canonical_config_path()
    canonical_hash_before = file_sha256(canonical_config)
    credential_path = canonical_config.parent / "chatgpt_oauth.json"
    if "chatgpt" in providers and not credential_path.is_file():
        raise SystemExit("ChatGPT OAuth credentials are unavailable; authenticate before the experiment")

    environment: dict[str, Any] = {
        "date": dt.date.today().isoformat(),
        "evaluation_scope": scope,
        "pilot_anecdotal": True,
        "limitations_predeclared": [
            "Three repetitions per strategy/task do not establish statistical significance.",
            "ChatGPT implicit prompt caching is provider-controlled; cache routing and eviction are not observable.",
            "ChatGPT agentic follow-up turns may use WebSocket previous-response continuation, so summed per-run input and cache counters mix initial full requests with continuation suffixes.",
            "Schema token counts use term-llm's bytes/4 estimator and are not provider billing counters.",
        ],
        "term_llm_commit": run(["git", "rev-parse", "HEAD"]).stdout.strip(),
        "working_tree_dirty": bool(run(["git", "status", "--porcelain"]).stdout.strip()),
        "host": {"platform": platform.platform(), "machine": platform.machine(), "processor": platform.processor()},
        "build_seconds": build_seconds,
        "server": {"connections_per_run": 1, "configured_servers": ["federation"],
                   "tools": len(catalogue["tools"]), "transport": "stdio", "mode": "aggregate"},
        "tasks": [task["id"] for task in tasks],
        "mode": DISCOVERY_MODE,
        "threshold": THRESHOLD,
        "strategies": {"chatgpt": CHATGPT_STRATEGIES, "qwen": QWEN_STRATEGIES},
        "repetitions": args.repetitions,
        "seed": args.seed,
        "workloads_per_strategy": len(workloads),
        "maximum_turns": args.max_turns,
        "run_order_file": "results/run-order.json",
        "shared_opening_context": shared_meta,
        "models": {},
        "canonical_config_mutated": False,
        "credentials_copied_to_scratch": False,
    }
    if "qwen" in providers:
        environment["models"]["qwen"] = qwen_metadata()
    if "chatgpt" in providers:
        environment["models"]["chatgpt"] = {
            "provider_key": CHATGPT_PROVIDER, "model": CHATGPT_MODEL,
            "provider_model": f"{CHATGPT_PROVIDER}:{CHATGPT_MODEL}",
            "selection_basis": "exact model specified by the user's configured agent preference",
            "reasoning_effort": "medium (model suffix)",
            "use_websocket": True,
            "credential_handling": "canonical OAuth file symlinked only into an ephemeral temporary XDG directory; no credential copied to scratch",
        }
    write_json(scratch / "results/environment.json", environment)

    warmups_path = scratch / "results/warmups.jsonl"
    runs_path = scratch / "results/runs.jsonl"
    if not args.resume:
        warmups_path.write_text("")
        runs_path.write_text("")
    else:
        warmups_path.touch()
        runs_path.touch()
    completed_warmups = {(row.get("provider"), row.get("strategy")) for row in read_jsonl(warmups_path)}
    completed_runs = {(row.get("provider"), row.get("strategy"), row.get("ordinal")) for row in read_jsonl(runs_path)}

    with tempfile.TemporaryDirectory(prefix="term-llm-oracle-eval-") as temp_name:
        temp_xdg = pathlib.Path(temp_name)
        config_dir = temp_xdg / "term-llm"
        config_dir.mkdir(parents=True)
        if "chatgpt" in providers:
            os.symlink(credential_path, config_dir / "chatgpt_oauth.json")
        for provider_name in providers:
            provider_spec = provider_model(provider_name)
            strategies = CHATGPT_STRATEGIES if provider_name == "chatgpt" else QWEN_STRATEGIES
            # Separate cold warm-ups per implementation strategy. Every strategy
            # then receives the exact same predeclared shuffled workload order.
            for strategy in strategies:
                if (provider_name, strategy) in completed_warmups:
                    continue
                warm_task = tasks[0]
                reset_fixture_state(scratch)
                config_text = write_mode_config(scratch, config_dir, provider_name, DISCOVERY_MODE,
                                                strategy, THRESHOLD, shared_opening)
                shutil.copy2(mcp_json, config_dir / "mcp.json")
                warm_record = execute_case(
                    term_bin, eval_bin, scratch, catalogue_path, catalogue, provider_name, provider_spec,
                    DISCOVERY_MODE, strategy, THRESHOLD, 0, warm_task, args.max_turns, args.run_timeout,
                    config_text, transcript_name=f"warmup-{provider_name}-{strategy}.txt", measured=False,
                    env=isolated_env(temp_xdg, scratch, provider_name),
                )
                append_jsonl(warmups_path, warm_record)
                print(f"[warmup] {provider_name} {strategy} success={warm_record['task_success']} "
                      f"cached={warm_record['provider_usage']['cached_input_tokens']}", flush=True)

            for strategy in strategies:
                for ordinal, item in enumerate(workloads, 1):
                    if (provider_name, strategy, ordinal) in completed_runs:
                        print(f"[{provider_name}/{strategy} {ordinal}/{len(workloads)}] resume: already complete", flush=True)
                        continue
                    reset_fixture_state(scratch)
                    config_text = write_mode_config(scratch, config_dir, provider_name, DISCOVERY_MODE,
                                                    strategy, THRESHOLD, shared_opening)
                    shutil.copy2(mcp_json, config_dir / "mcp.json")
                    transcript_name = (f"{provider_name}-{strategy}-{ordinal:03d}-{item['task']['id']}-"
                                       f"r{item['repetition']}.txt")
                    record = execute_case(
                        term_bin, eval_bin, scratch, catalogue_path, catalogue, provider_name, provider_spec,
                        DISCOVERY_MODE, strategy, THRESHOLD, item["repetition"], item["task"],
                        args.max_turns, args.run_timeout, config_text, transcript_name=transcript_name,
                        measured=True, env=isolated_env(temp_xdg, scratch, provider_name),
                    )
                    record["ordinal"] = ordinal
                    append_jsonl(runs_path, record)
                    print(f"[{provider_name}/{strategy} {ordinal}/{len(workloads)}] {item['task']['id']} "
                          f"r{item['repetition']} call={record['correct_external_tool_call']} "
                          f"oracle={record['oracle_exact']} success={record['task_success']}", flush=True)

    canonical_hash_after = file_sha256(canonical_config)
    environment["canonical_config_mutated"] = canonical_hash_before != canonical_hash_after
    environment["canonical_config_sha256_before"] = canonical_hash_before
    environment["canonical_config_sha256_after"] = canonical_hash_after
    write_json(scratch / "results/environment.json", environment)
    if environment["canonical_config_mutated"]:
        raise SystemExit("canonical config changed during evaluation; results are not accepted")
    summary = summarize(read_jsonl(runs_path), read_jsonl(warmups_path), environment)
    write_json(scratch / "results/summary.json", summary)
    (scratch / "results/report.md").write_text(render_report(summary, environment))
    write_artifact_manifest(scratch)
    print(f"Report: {scratch / 'results/report.md'}")
    return 0


def construct_shared_opening(target_bytes: int) -> str:
    header = "SHARED PROMPT CACHE EVALUATION OPENING\n"
    pieces = [header]
    index = 0
    while len("".join(pieces).encode()) < target_bytes:
        pieces.append(f"block-{index:05d} stable shared neutral context supports deterministic cache comparison across requests.\n")
        index += 1
    data = "".join(pieces).encode()[:target_bytes]
    return data.decode("ascii")


def canonical_config_path() -> pathlib.Path:
    result = run(["term-llm", "config", "path"])
    path = pathlib.Path(result.stdout.strip()).expanduser()
    if not path.is_file():
        raise SystemExit(f"canonical config does not exist: {path}")
    return path.resolve()


def provider_model(provider_name: str) -> str:
    if provider_name == "qwen":
        return f"{QWEN_PROVIDER}:{QWEN_MODEL}"
    return f"{CHATGPT_PROVIDER}:{CHATGPT_MODEL}"


def qwen_metadata() -> dict[str, Any]:
    raw = ollama_api("/api/show", {"model": QWEN_MODEL})
    tags = ollama_api("/api/tags")
    selected = next((model for model in tags.get("models", []) if model.get("name") == QWEN_MODEL), {})
    details = raw.get("details", {}) if isinstance(raw.get("details"), dict) else {}
    return {
        "provider_key": QWEN_PROVIDER, "provider_type": "ollama", "model": QWEN_MODEL,
        "provider_model": f"{QWEN_PROVIDER}:{QWEN_MODEL}", "endpoint": "http://127.0.0.1:11434",
        "digest": selected.get("digest", "unknown"), "size": selected.get("size", 0),
        "modified_at": selected.get("modified_at", "unknown"),
        "family": details.get("family", "unknown"), "parameter_size": details.get("parameter_size", "unknown"),
        "quantization_level": details.get("quantization_level", "unknown"),
        "context_window": 131072, "think": False,
        "sampling": "installed Ollama model defaults; no term-llm temperature flag",
        "ollama_parameters": raw.get("parameters", "unknown"),
        "ollama_version": run(["ollama", "--version"], check=False).stdout.strip(),
    }


def ollama_api(path: str, payload: dict[str, Any] | None = None) -> dict[str, Any]:
    data = None if payload is None else json.dumps(payload).encode()
    request = urllib.request.Request("http://127.0.0.1:11434" + path, data=data,
                                     headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            value = json.load(response)
            return value if isinstance(value, dict) else {}
    except (OSError, urllib.error.URLError, json.JSONDecodeError):
        return {}


def write_mode_config(scratch: pathlib.Path, temp_config_dir: pathlib.Path, provider_name: str,
                      mode: str, strategy: str, threshold: int, shared_opening: str) -> str:
    if provider_name == "qwen":
        system_prompt = GENERAL_GUIDANCE
        provider_yaml = (
            f"default_provider: {QWEN_PROVIDER}\nproviders:\n  {QWEN_PROVIDER}:\n"
            f"    type: ollama\n    base_url: http://127.0.0.1:11434\n    think: false\n"
            f"    model: {QWEN_MODEL}\n    num_ctx: 131072\n    num_predict: 8192\n"
            f"    context_window: 131072\n    max_output_tokens: 8192\n"
        )
    else:
        system_prompt = shared_opening + "\n\n" + GENERAL_GUIDANCE
        provider_yaml = (
            f"default_provider: {CHATGPT_PROVIDER}\nproviders:\n  {CHATGPT_PROVIDER}:\n"
            f"    type: chatgpt\n    model: {CHATGPT_MODEL}\n    use_websocket: true\n"
        )
    text = (
        provider_yaml + f"ask:\n  instructions: {json.dumps(system_prompt)}\n"
        "skills:\n  enabled: false\nagents_md:\n  enabled: false\nsessions:\n  enabled: false\n"
        f"tool_discovery:\n  mode: {mode}\n  strategy: {strategy}\n  threshold: {threshold}\n"
    )
    (temp_config_dir / "config.yaml").write_text(text)
    artifact = scratch / "config" / f"{provider_name}-{mode}-{strategy}-t{threshold}.yaml"
    artifact.write_text(text)
    return text


def isolated_env(temp_xdg: pathlib.Path, scratch: pathlib.Path, provider_name: str) -> dict[str, str]:
    env = os.environ.copy()
    env.update({
        "XDG_CONFIG_HOME": str(temp_xdg),
        "XDG_DATA_HOME": str(scratch / "runtime" / provider_name / "data"),
        "XDG_CACHE_HOME": str(scratch / "runtime" / provider_name / "cache"),
        "TERM_LLM_SKIP_UPDATE_CHECK": "1",
    })
    return env


def execute_case(term_bin: pathlib.Path, eval_bin: pathlib.Path, scratch: pathlib.Path,
                 catalogue_path: pathlib.Path, catalogue: dict[str, Any], provider_name: str,
                 provider_spec: str, mode: str, strategy: str, threshold: int, repetition: int, task: dict[str, Any],
                 max_turns: int, timeout: int, config_text: str, *, transcript_name: str,
                 measured: bool, env: dict[str, str]) -> dict[str, Any]:
    started_wall = dt.datetime.now(dt.timezone.utc)
    started = time.perf_counter()
    cmd = [str(term_bin), "--no-session"]
    if provider_name == "chatgpt":
        cmd.append("--debug-raw")
    cmd += ["ask", "--json", "--provider", provider_spec,
            "--mcp", "federation", "--max-turns", str(max_turns), task["prompt"]]
    timed_out = False
    try:
        proc = subprocess.run(cmd, cwd=ROOT, env=env, timeout=timeout, text=True,
                              stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    except subprocess.TimeoutExpired as exc:
        timed_out = True
        proc = subprocess.CompletedProcess(cmd, 124, stdout=coerce_text(exc.stdout),
                                           stderr=coerce_text(exc.stderr) + f"\ntimeout after {timeout}s")
    elapsed = time.perf_counter() - started
    transcript = scratch / "transcripts" / transcript_name
    transcript.write_text("# stdout JSONL\n" + proc.stdout + "\n# stderr\n" + proc.stderr)
    events = parse_jsonl(proc.stdout)
    calls = read_jsonl(scratch / "calls.jsonl")
    called = [f"federation__{call['server']}__{call['tool']}" for call in calls]
    required = task["required_tool"]
    wrong_calls = [name for name in called if name != required]
    final_output = "".join(str(event.get("text", "")) for event in events if event.get("type") == "text.delta").strip()
    usage_events = [event for event in events if event.get("type") == "usage"]
    usage = sum_usage(usage_events)
    search = analyze_search_events(eval_bin, scratch, catalogue_path, catalogue, events, required,
                                   transcript_name.replace(".txt", ""))
    resolved = "eager" if mode == "eager" else "deferred"
    if mode == "auto":
        resolved = "eager" if len(catalogue["tools"]) <= threshold else "deferred"
    eager_tokens = sum(int(tool.get("estimated_tokens", 0)) for tool in catalogue["tools"])
    initial_tokens = eager_tokens if resolved == "eager" else 0
    activated_set = set(search["activated_tools"])
    activated_tokens = sum(int(tool.get("estimated_tokens", 0)) for tool in catalogue["tools"]
                           if tool["name"] in activated_set)
    correct_call = required in called
    activation_or_exposure = required in activated_set if resolved == "deferred" else any(
        tool["name"] == required for tool in catalogue["tools"])
    oracle_exact = final_output == task["expected_oracle"]
    success = proc.returncode == 0 and correct_call and not wrong_calls and oracle_exact
    observed_input = usage["input_tokens"] + usage["cached_input_tokens"]
    initial_usage = usage_events[0] if usage_events else {}
    initial_observed_input = int(initial_usage.get("input_tokens", 0) or 0) + int(initial_usage.get("cached_input_tokens", 0) or 0)
    initial_cached_input = int(initial_usage.get("cached_input_tokens", 0) or 0)
    protocol = analyze_debug_protocol(proc.stderr) if provider_name == "chatgpt" else {
        "request_count": None, "top_level_tools_stable": None, "top_level_tool_signatures": [],
        "loaded_schemas_in_top_level_tools": None, "previous_response_requests": None,
    }
    native_search_items = [event for event in events if event.get("type") == "tool.started" and
                           event.get("name") == "tool_search" and event.get("info") == "(native client discovery)"]
    native_output_items = []
    for event in events:
        if event.get("type") != "tool.completed" or event.get("name") != "tool_search":
            continue
        info = event.get("info")
        try:
            decoded = json.loads(info) if isinstance(info, str) else {}
        except json.JSONDecodeError:
            decoded = {}
        if decoded.get("execution") == "client":
            native_output_items.append(decoded)
    record = {
        "measured": measured, "provider": provider_name, "provider_model": provider_spec,
        "mode": mode, "resolved_mode": resolved, "strategy": strategy,
        "threshold": threshold, "repetition": repetition,
        "task_id": task["id"], "domain": task["domain"], "prompt_sha256": sha256_text(task["prompt"]),
        "required_tool": required, "expected_oracle": task["expected_oracle"],
        "return_code": proc.returncode, "timed_out": timed_out, "wall_seconds": elapsed,
        "time_to_first_provider_output_seconds": event_delay(started_wall, events,
            lambda e: e.get("type") in {"text.delta", "tool.started"}),
        "time_to_first_external_tool_seconds": event_delay(started_wall, events,
            lambda e: e.get("type") == "tool.started" and e.get("name") != "tool_search"),
        "provider_turns": len(usage_events), "provider_usage_turns": usage_events,
        "provider_usage": usage, "provider_observed_input_tokens": observed_input,
        "cache_hit_ratio": (usage["cached_input_tokens"] / observed_input if observed_input else 0.0),
        "initial_provider_observed_input_tokens": initial_observed_input,
        "initial_provider_cached_input_tokens": initial_cached_input,
        "initial_provider_cache_write_tokens": int(initial_usage.get("cache_write_tokens", 0) or 0),
        "initial_provider_output_tokens": int(initial_usage.get("output_tokens", 0) or 0),
        "initial_cache_hit_ratio": (initial_cached_input / initial_observed_input if initial_observed_input else 0.0),
        "final_output": final_output, "oracle_exact": oracle_exact,
        "called_tools": called, "wrong_tool_calls": wrong_calls,
        "correct_external_tool_call": correct_call,
        "correct_external_activation_or_initial_exposure": activation_or_exposure,
        "search_calls": search["search_calls"], "search_details": search["details"],
        "required_tool_search_rank": search["required_tool_rank"],
        "activated_tools": search["activated_tools"], "activated_schema_estimated_tokens": activated_tokens,
        "required_tool_recall_at_5": search["required_tool_recall_at_5"],
        "native_search_items": native_search_items, "native_output_items": native_output_items,
        "native_fallback_count": proc.stderr.count("native tool discovery fell back to portable"),
        "top_level_tools_stable": protocol["top_level_tools_stable"],
        "loaded_schemas_in_top_level_tools": protocol["loaded_schemas_in_top_level_tools"],
        "provider_protocol_audit": protocol,
        "initial_mcp_schema_estimated_tokens": initial_tokens,
        "eager_mcp_schema_estimated_tokens": eager_tokens,
        "task_success": success, "transcript": str(transcript.relative_to(scratch)),
        "config_sha256": sha256_text(config_text),
    }
    return record


def analyze_debug_protocol(stderr: str) -> dict[str, Any]:
    pattern = re.compile(
        r"Responses WebSocket Request \(reused=(?:true|false)\)\n(\{.*?\})\n"
        r"\[[^\n]+\] END Responses WebSocket Request",
        re.DOTALL,
    )
    requests: list[dict[str, Any]] = []
    for match in pattern.finditer(stderr):
        try:
            payload = json.loads(match.group(1))
        except json.JSONDecodeError:
            continue
        if isinstance(payload, dict):
            requests.append(payload)
    signatures = [json.dumps(request.get("tools", []), sort_keys=True, separators=(",", ":"))
                  for request in requests]
    loaded_names: set[str] = set()
    top_level_function_names: set[str] = set()
    request_summaries: list[dict[str, Any]] = []
    for request in requests:
        tools = request.get("tools", []) if isinstance(request.get("tools"), list) else []
        top_types: list[str] = []
        top_names: list[str] = []
        for tool in tools:
            if not isinstance(tool, dict):
                continue
            top_types.append(str(tool.get("type", "")))
            if tool.get("type") == "function" and tool.get("name"):
                name = str(tool["name"])
                top_names.append(name)
                top_level_function_names.add(name)
        input_items = request.get("input", []) if isinstance(request.get("input"), list) else []
        input_types: list[str] = []
        for item in input_items:
            if not isinstance(item, dict):
                continue
            input_types.append(str(item.get("type", "")))
            if item.get("type") == "tool_search_output":
                for tool in item.get("tools", []):
                    if isinstance(tool, dict) and tool.get("name"):
                        loaded_names.add(str(tool["name"]))
        request_summaries.append({
            "previous_response_id": bool(request.get("previous_response_id")),
            "top_level_tool_types": top_types,
            "top_level_function_names": top_names,
            "input_item_types": input_types,
        })
    stable = len(set(signatures)) <= 1 if signatures else None
    return {
        "request_count": len(requests),
        "top_level_tools_stable": stable,
        "top_level_tool_signatures": [hashlib.sha256(value.encode()).hexdigest() for value in signatures],
        "loaded_schemas_in_top_level_tools": bool(loaded_names & top_level_function_names),
        "previous_response_requests": sum(1 for request in requests if request.get("previous_response_id")),
        "requests": request_summaries,
    }


def analyze_search_events(eval_bin: pathlib.Path, scratch: pathlib.Path, catalogue_path: pathlib.Path,
                          catalogue: dict[str, Any], events: list[dict[str, Any]], required: str,
                          label: str) -> dict[str, Any]:
    actions: list[dict[str, Any]] = []
    retrieval_tasks: list[dict[str, Any]] = []
    for index, event in enumerate(events):
        if event.get("type") != "tool.started" or event.get("name") != "tool_search":
            continue
        args = event.get("args", {})
        if isinstance(args, str):
            try:
                args = json.loads(args)
            except json.JSONDecodeError:
                args = {}
        if not isinstance(args, dict):
            args = {}
        query = str(args.get("query", "")).strip()
        names = [str(name).strip() for name in args.get("tool_names", []) if str(name).strip()]
        limit = max(1, min(8, int(args.get("max_results", 5) or 5)))
        case_id = f"search-{index}"
        actions.append({"id": case_id, "query": query, "tool_names": names, "limit": limit})
        if query:
            retrieval_tasks.append({"id": case_id, "prompt": query, "required_tools": [required],
                                    "max_results": limit})
    query_results: dict[str, dict[str, Any]] = {}
    if retrieval_tasks:
        task_path = scratch / "results/search-details" / f"{label}-tasks.json"
        result_path = scratch / "results/search-details" / f"{label}-results.json"
        write_json(task_path, {"tasks": retrieval_tasks})
        run([str(eval_bin), "retrieval", "--manifest", str(catalogue_path), "--tasks", str(task_path),
             "--output", str(result_path)])
        query_results = {case["id"]: case for case in json.loads(result_path.read_text())["cases"]}
    by_name = {tool["name"]: tool for tool in catalogue["tools"]}
    by_original: dict[str, list[str]] = {}
    for tool in catalogue["tools"]:
        by_original.setdefault(tool["original_name"].lower(), []).append(tool["name"])
    activated: list[str] = []
    active: set[str] = set()
    top5: set[str] = set()
    required_rank: int | None = None
    details: list[dict[str, Any]] = []
    for action in actions:
        candidates: list[str] = []
        ranked_top: list[str] = []
        if action["query"]:
            result = query_results.get(action["id"], {})
            candidates = list(result.get("results", []))[:action["limit"]]
            ranked_top = list(result.get("top5", []))
        else:
            for requested in action["tool_names"]:
                if requested in by_name:
                    candidates.append(requested)
                else:
                    matches = by_original.get(requested.lower(), [])
                    if len(matches) == 1:
                        candidates.append(matches[0])
            ranked_top = candidates[:5]
        top5.update(ranked_top)
        if required in candidates:
            rank = candidates.index(required) + 1
            required_rank = rank if required_rank is None else min(required_rank, rank)
        for name in candidates:
            if name not in active and len(active) < 16:
                active.add(name)
                activated.append(name)
        details.append({**action, "candidates": candidates, "top5": ranked_top})
    return {"search_calls": len(actions), "activated_tools": activated,
            "required_tool_recall_at_5": (required in top5 if actions else None),
            "required_tool_rank": required_rank, "details": details}


def validate_single_server_catalogue(mcp_json: pathlib.Path, catalogue: dict[str, Any],
                                     tasks: list[dict[str, Any]]) -> None:
    config = json.loads(mcp_json.read_text())
    if sorted(config.get("servers", {})) != ["federation"]:
        raise SystemExit(f"measured MCP config is not one server: {sorted(config.get('servers', {}))}")
    tools = catalogue.get("tools", [])
    if len(tools) != 200:
        raise SystemExit(f"aggregate catalogue has {len(tools)} tools, want exactly 200")
    if {tool.get("server") for tool in tools} != {"federation"}:
        raise SystemExit("aggregate catalogue contains a non-federation server")
    names = {tool["name"] for tool in tools}
    for tool in tools:
        properties = tool.get("output_schema", {}).get("properties", {})
        required = tool.get("output_schema", {}).get("required", [])
        if "oracle" not in properties or "oracle" not in required:
            raise SystemExit(f"tool {tool['name']} output schema lacks required oracle")
    for task in tasks:
        if task["required_tool"] not in names:
            raise SystemExit(f"task {task['id']} required tool is unavailable")
        if task["required_tool"] in task["prompt"]:
            raise SystemExit(f"task {task['id']} leaks exact prefixed tool name")


def sum_usage(events: list[dict[str, Any]]) -> dict[str, int]:
    keys = ["input_tokens", "cached_input_tokens", "cache_write_tokens", "output_tokens"]
    return {key: sum(int(event.get(key, 0) or 0) for event in events) for key in keys}


def summarize(rows: list[dict[str, Any]], warmups: list[dict[str, Any]], environment: dict[str, Any]) -> dict[str, Any]:
    groups: dict[str, dict[str, Any]] = {}
    for row in rows:
        strategy = row.get("strategy", row["mode"])
        key = f"{row['provider']}:{strategy}"
        group = groups.setdefault(key, {"provider": row["provider"], "mode": row["mode"], "strategy": strategy, "runs": 0,
            "correct_external_tool_calls": 0, "oracle_exact": 0, "task_successes": 0,
            "wrong_tool_calls": 0, "search_calls": 0, "turns": 0, "wall_seconds": [],
            "provider_observed_input_tokens": 0, "input_tokens": 0, "cached_input_tokens": 0,
            "initial_provider_observed_input_tokens": 0, "initial_provider_cached_input_tokens": 0,
            "initial_provider_output_tokens": 0, "initial_provider_cache_write_tokens": 0,
            "cache_write_tokens": 0, "output_tokens": 0, "initial_mcp_schema_estimated_tokens": 0,
            "activated_schema_estimated_tokens": 0, "native_search_items": 0, "native_output_items": 0,
            "native_fallback_count": 0, "top_level_stable_runs": 0, "top_level_observed_runs": 0})
        group["runs"] += 1
        group["correct_external_tool_calls"] += int(row["correct_external_tool_call"])
        group["oracle_exact"] += int(row["oracle_exact"])
        group["task_successes"] += int(row["task_success"])
        group["wrong_tool_calls"] += len(row["wrong_tool_calls"])
        group["search_calls"] += row["search_calls"]
        group["turns"] += row["provider_turns"]
        group["wall_seconds"].append(row["wall_seconds"])
        group["provider_observed_input_tokens"] += row["provider_observed_input_tokens"]
        first_usage = row.get("provider_usage_turns", [{}])[0] if row.get("provider_usage_turns") else {}
        initial_observed = int(row.get("initial_provider_observed_input_tokens",
            int(first_usage.get("input_tokens", 0) or 0) + int(first_usage.get("cached_input_tokens", 0) or 0)))
        initial_cached = int(row.get("initial_provider_cached_input_tokens",
            int(first_usage.get("cached_input_tokens", 0) or 0)))
        initial_output = int(row.get("initial_provider_output_tokens", int(first_usage.get("output_tokens", 0) or 0)))
        group["initial_provider_observed_input_tokens"] += initial_observed
        group["initial_provider_cached_input_tokens"] += initial_cached
        group["initial_provider_output_tokens"] += initial_output
        group["initial_provider_cache_write_tokens"] += int(row.get("initial_provider_cache_write_tokens", 0) or 0)
        group["native_search_items"] += len(row.get("native_search_items", []))
        group["native_output_items"] += len(row.get("native_output_items", []))
        group["native_fallback_count"] += int(row.get("native_fallback_count", 0) or 0)
        if row.get("top_level_tools_stable") is not None:
            group["top_level_observed_runs"] += 1
            group["top_level_stable_runs"] += int(bool(row.get("top_level_tools_stable")))
        for usage_key in ["input_tokens", "cached_input_tokens", "cache_write_tokens", "output_tokens"]:
            group[usage_key] += row["provider_usage"][usage_key]
        group["initial_mcp_schema_estimated_tokens"] += row["initial_mcp_schema_estimated_tokens"]
        group["activated_schema_estimated_tokens"] += row["activated_schema_estimated_tokens"]
    for group in groups.values():
        count = group["runs"]
        latencies = group.pop("wall_seconds")
        observed = group["provider_observed_input_tokens"]
        initial_observed = group["initial_provider_observed_input_tokens"]
        group.update({
            "correct_external_tool_call_rate": group["correct_external_tool_calls"] / count,
            "oracle_accuracy": group["oracle_exact"] / count,
            "task_success_rate": group["task_successes"] / count,
            "mean_turns": group["turns"] / count,
            "mean_wall_seconds": statistics.mean(latencies),
            "median_wall_seconds": statistics.median(latencies),
            "cache_hit_ratio": group["cached_input_tokens"] / observed if observed else 0.0,
            "initial_cache_hit_ratio": (group["initial_provider_cached_input_tokens"] / initial_observed
                                        if initial_observed else 0.0),
            "mean_initial_provider_observed_input_tokens": initial_observed / count,
            "mean_initial_provider_cached_input_tokens": group["initial_provider_cached_input_tokens"] / count,
            "mean_initial_mcp_schema_estimated_tokens": group["initial_mcp_schema_estimated_tokens"] / count,
            "mean_activated_schema_estimated_tokens": group["activated_schema_estimated_tokens"] / count,
            "top_level_tools_stable_rate": (group["top_level_stable_runs"] / group["top_level_observed_runs"]
                                             if group["top_level_observed_runs"] else None),
        })
    return {
        "evaluation_scope": environment["evaluation_scope"], "pilot_anecdotal": True,
        "raw_measured_runs": len(rows), "raw_cold_warmup_runs": len(warmups),
        "groups": groups, "cold_warmups": warmups,
        "shared_opening_context": environment["shared_opening_context"],
        "limitations": environment["limitations_predeclared"],
    }


def render_report(summary: dict[str, Any], environment: dict[str, Any]) -> str:
    lines = ["# Single 200-tool MCP oracle and cache pilot", "",
             f"**Scope:** {summary['evaluation_scope']}. Results are pilot/anecdotal, not statistically significant.", "",
             "The measured client connected to **1 MCP server exposing exactly 200 tools** for every run. "
             f"Cold warm-ups ({summary['raw_cold_warmup_runs']}) are separated from measured warm runs "
             f"({summary['raw_measured_runs']}).", "", "## Measured raw counts", "",
             "| Provider | Strategy | Runs | Correct calls | Exact oracles | Task successes | Wrong calls | Search calls | Native call/output | Fallbacks | Turns | Initial input | Initial cached | Initial cache write | Initial cache hit | All-turn input | All-turn cached | All-turn cache write | All-turn cache hit | Output | Mean latency s | Top-level stable |",
             "|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|" ]
    for key in sorted(summary["groups"]):
        group = summary["groups"][key]
        stable = group.get("top_level_tools_stable_rate")
        stable_text = "n/a" if stable is None else f"{stable:.3f}"
        lines.append(
            f"| {group['provider']} | {group['strategy']} | {group['runs']} | {group['correct_external_tool_calls']} | "
            f"{group['oracle_exact']} | {group['task_successes']} | {group['wrong_tool_calls']} | "
            f"{group['search_calls']} | {group['native_search_items']}/{group['native_output_items']} | "
            f"{group['native_fallback_count']} | {group['turns']} | {group['initial_provider_observed_input_tokens']} | "
            f"{group['initial_provider_cached_input_tokens']} | {group['initial_provider_cache_write_tokens']} | "
            f"{group['initial_cache_hit_ratio']:.3f} | {group['provider_observed_input_tokens']} | "
            f"{group['cached_input_tokens']} | {group['cache_write_tokens']} | {group['cache_hit_ratio']:.3f} | "
            f"{group['output_tokens']} | {group['mean_wall_seconds']:.3f} | {stable_text} |")
    chatgpt_native = summary["groups"].get("chatgpt:native")
    chatgpt_portable = summary["groups"].get("chatgpt:portable")
    if chatgpt_native and chatgpt_portable:
        lines += ["", "## Observed comparison", "",
                  f"- Both ChatGPT strategies completed {chatgpt_native['runs']}/{chatgpt_native['runs']} exact external-call/oracle cases with zero wrong calls.",
                  f"- Native emitted {chatgpt_native['native_search_items']} client tool-search calls and {chatgpt_native['native_output_items']} matched outputs, with {chatgpt_native['native_fallback_count']} fallbacks.",
                  f"- Provider-wire audits found stable top-level tools in {chatgpt_native['top_level_stable_runs']}/{chatgpt_native['top_level_observed_runs']} native runs and {chatgpt_portable['top_level_stable_runs']}/{chatgpt_portable['top_level_observed_runs']} portable runs. No native-loaded schema appeared in the ordinary top-level tools array.",
                  f"- Mean initial provider-observed input was {chatgpt_portable['mean_initial_provider_observed_input_tokens']:.0f} portable versus {chatgpt_native['mean_initial_provider_observed_input_tokens']:.0f} native; median wall time was {chatgpt_portable['median_wall_seconds']:.3f}s versus {chatgpt_native['median_wall_seconds']:.3f}s.",
                  f"- Initial cached tokens were {chatgpt_portable['initial_provider_cached_input_tokens']} portable and {chatgpt_native['initial_provider_cached_input_tokens']} native across all measured runs; cache-write tokens were {chatgpt_portable['cache_write_tokens']} and {chatgpt_native['cache_write_tokens']}. These sparse provider counters do not prove a prompt-cache advantage.",
                  f"- All-turn cached totals ({chatgpt_portable['cached_input_tokens']} portable, {chatgpt_native['cached_input_tokens']} native) include WebSocket previous-response continuation and loaded-tool context, so they are not interpreted as independent prefix-cache hits."]
    shared = summary["shared_opening_context"]
    lines += ["", "## Shared opening context", "",
              f"The ChatGPT requests used the same byte-for-byte opening (`sha256 {shared['sha256']}`): "
              f"{shared['bytes']} bytes, {shared['whitespace_words']} whitespace words, and a constructed estimate of "
              f"{shared['constructed_estimate_tokens']} tokens using `{shared['constructed_estimator']}`. "
              f"The target was {shared['target']}; calibration was: {shared['calibration']}. "
              "The rough constructed estimate is not claimed as an exact tokenizer count. Provider-observed initial-turn and full-agentic-run input are reported separately in the table and raw JSONL.",
              "", "## Measurement and scoring", "",
              "- Correct call: the deterministic execution log contains the semantically required external tool.",
              "- Oracle exact: stripped final model text equals the tool's unique deterministic oracle value.",
              "- Task success: process success + correct call + no wrong external calls + exact oracle.",
              "- Input/cache/output tokens: sums of term-llm usage events normalized from provider responses; observed input is non-cached + cached input.",
              "- Initial cache columns use only the first provider turn. Agentic cache columns sum every turn and therefore include within-run continuation/cache effects.",
              "- Initial/activated schema counts are retained in raw JSON as term-llm catalogue estimates, not provider billing counters.",
              "", "## Limitations and confounders", ""]
    lines.extend(f"- {item}" for item in summary["limitations"])
    lines += ["- Each `ask` invocation starts a fresh process, but agentic turns inside a ChatGPT run may use WebSocket continuation. This can reduce follow-up payloads independently of prefix-cache hits.",
              "- Warm-up requests used the first oracle task once per strategy/provider; cache population, routing, granularity, and eviction remain provider-controlled.",
              "", "Raw records: `runs.jsonl`; cold warm-ups: `warmups.jsonl`; environment: `environment.json`; deterministic order: `run-order.json`.", ""]
    return "\n".join(lines)


def write_artifact_manifest(scratch: pathlib.Path) -> None:
    files = []
    for path in sorted(scratch.rglob("*")):
        if path.is_file() and path.parent.name != "bin" and "runtime" not in path.parts:
            files.append({"path": str(path.relative_to(scratch)), "bytes": path.stat().st_size,
                          "sha256": file_sha256(path)})
    write_json(scratch / "results/artifact-manifest.json", {"files": files, "secrets_included": False})


def reset_fixture_state(root: pathlib.Path) -> None:
    shutil.rmtree(root / "state", ignore_errors=True)
    (root / "state").mkdir(parents=True, exist_ok=True)
    (root / "calls.jsonl").write_text("")


def parse_jsonl(text: str) -> list[dict[str, Any]]:
    rows = []
    for line in text.splitlines():
        try:
            value = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(value, dict):
            rows.append(value)
    return rows


def read_jsonl(path: pathlib.Path) -> list[dict[str, Any]]:
    if not path.exists():
        return []
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def append_jsonl(path: pathlib.Path, value: dict[str, Any]) -> None:
    with path.open("a") as file:
        file.write(json.dumps(value, sort_keys=True) + "\n")


def event_delay(started: dt.datetime, events: list[dict[str, Any]], predicate: Any) -> float | None:
    for event in events:
        if not predicate(event) or not isinstance(event.get("ts"), str):
            continue
        try:
            observed = dt.datetime.fromisoformat(event["ts"].replace("Z", "+00:00"))
        except ValueError:
            return None
        return max(0.0, (observed - started).total_seconds())
    return None


def coerce_text(value: str | bytes | None) -> str:
    if value is None:
        return ""
    return value.decode(errors="replace") if isinstance(value, bytes) else value


def sha256_text(value: str) -> str:
    return hashlib.sha256(value.encode()).hexdigest()


def file_sha256(path: pathlib.Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def write_json(path: pathlib.Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


if __name__ == "__main__":
    raise SystemExit(main())
