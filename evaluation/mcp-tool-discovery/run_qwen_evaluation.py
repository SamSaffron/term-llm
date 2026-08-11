#!/usr/bin/env python3
"""Reproducible local-Qwen MCP discovery evaluation.

This deliberately uses real stdio MCP processes. It never reads or writes the
normal term-llm config: EVAL_BASE_CONFIG is copied into a dated scratch tree.
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import pathlib
import platform
import random
import shutil
import subprocess
import time
import urllib.parse
from typing import Any

ROOT = pathlib.Path(__file__).resolve().parents[2]
SIZES = [6, 12, 18, 24, 25, 32, 42, 64, 100, 200]
THRESHOLDS = [16, 20, 24, 28, 32]
PILOT_SIZES = [6, 24, 25, 42, 200]
PILOT_TASK_IDS_BY_SIZE = {
    6: ["single-source-pr"],
    24: ["single-source-pr", "sequence-merge-pr"],
    25: ["single-source-pr", "sequence-merge-pr"],
    42: ["single-source-pr", "sequence-merge-pr", "sequence-triage-issue"],
    200: ["single-source-pr", "sequence-merge-pr", "sequence-triage-issue", "cross-pr-chat", "adversarial-issue-vs-error"],
}


def run(cmd: list[str], *, env: dict[str, str] | None = None, timeout: int = 300, capture: bool = True) -> subprocess.CompletedProcess[str]:
    return subprocess.run(cmd, cwd=ROOT, env=env, timeout=timeout, check=True, text=True,
                          stdout=subprocess.PIPE if capture else None,
                          stderr=subprocess.PIPE if capture else None)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--scratch", type=pathlib.Path, default=pathlib.Path.home() / "scratch" / f"{dt.date.today().isoformat()}-mcp-tool-discovery-eval")
    parser.add_argument("--repetitions", type=int, default=3)
    parser.add_argument("--max-turns", type=int, default=8)
    parser.add_argument("--seed", type=int, default=240825)
    parser.add_argument("--quick", action="store_true", help="one 200-tool deferred run for infrastructure smoke only")
    parser.add_argument("--pilot", action="store_true", help="predeclared anecdotal 6/24/25/42/200 eager/deferred/auto subset")
    args = parser.parse_args()
    if args.quick and args.pilot:
        parser.error("--quick and --pilot are mutually exclusive")

    base_config = os.environ.get("EVAL_BASE_CONFIG", "")
    provider = os.environ.get("EVAL_PROVIDER", "")
    model = os.environ.get("EVAL_MODEL", "")
    if not base_config or not pathlib.Path(base_config).is_file() or not provider or not model:
        parser.error("set EVAL_BASE_CONFIG to an isolated Qwen-capable config template and set EVAL_PROVIDER/EVAL_MODEL")
    metadata_vars = ["EVAL_MODEL_DIGEST", "EVAL_QUANTISATION", "EVAL_CONTEXT_LIMIT",
                     "EVAL_TOOL_CALL_SETTINGS", "EVAL_TEMPERATURE", "EVAL_PROVIDER_ENDPOINT"]
    missing_metadata = [name for name in metadata_vars if not os.environ.get(name)]
    if missing_metadata:
        parser.error("set required reproducibility metadata: " + ", ".join(missing_metadata))
    try:
        int(os.environ["EVAL_CONTEXT_LIMIT"])
        float(os.environ["EVAL_TEMPERATURE"])
    except ValueError:
        parser.error("EVAL_CONTEXT_LIMIT must be an integer and EVAL_TEMPERATURE must be numeric")
    endpoint = urllib.parse.urlsplit(os.environ["EVAL_PROVIDER_ENDPOINT"])
    if endpoint.username or endpoint.password or endpoint.query or endpoint.fragment:
        parser.error("EVAL_PROVIDER_ENDPOINT must not contain credentials, query parameters, or fragments")
    base_config_path = pathlib.Path(base_config)
    if any(line.strip().startswith("tool_discovery:") for line in base_config_path.read_text().splitlines()):
        parser.error("EVAL_BASE_CONFIG must not define tool_discovery; the evaluation owns mode and threshold")

    scratch = args.scratch.expanduser().resolve()
    required_name = f"{dt.date.today().isoformat()}-mcp-tool-discovery-eval"
    if scratch.name != required_name:
        parser.error(f"scratch directory must be dated and named {required_name!r}")
    for name in ["config", "fixtures", "repos", "databases", "documents", "results", "transcripts", "bin", "profiles"]:
        (scratch / name).mkdir(parents=True, exist_ok=True)
    shutil.copy2(ROOT / "evaluation/mcp-tool-discovery/tasks.json", scratch / "fixtures/tasks.json")
    shutil.copy2(ROOT / "evaluation/mcp-tool-discovery/retrieval.json", scratch / "fixtures/retrieval.json")

    term_bin = scratch / "bin/term-llm"
    eval_bin = scratch / "bin/mcp-discovery-eval"
    run(["go", "build", "-o", str(term_bin), "."], capture=False)
    run(["go", "build", "-o", str(eval_bin), "./internal/tooldiscovery/evalcmd"], capture=False)
    run([str(eval_bin), "init", "--root", str(scratch)])

    commit = run(["git", "rev-parse", "HEAD"]).stdout.strip()
    dirty = bool(run(["git", "status", "--porcelain"]).stdout.strip())
    repetitions = 1 if args.quick or args.pilot else args.repetitions
    scope = "quick infrastructure smoke" if args.quick else ("pilot/anecdotal" if args.pilot else "full")
    environment: dict[str, Any] = {
        "term_llm_commit": commit,
        "working_tree_dirty": dirty,
        "provider": provider,
        "model": model,
        "model_digest": os.environ["EVAL_MODEL_DIGEST"],
        "quantisation": os.environ["EVAL_QUANTISATION"],
        "context_limit": int(os.environ["EVAL_CONTEXT_LIMIT"]),
        "provider_endpoint": os.environ["EVAL_PROVIDER_ENDPOINT"],
        "tool_call_settings": os.environ["EVAL_TOOL_CALL_SETTINGS"],
        "temperature": float(os.environ["EVAL_TEMPERATURE"]),
        "maximum_turns": args.max_turns,
        "repetitions": repetitions,
        "evaluation_scope": scope,
        "warmup": "one unscored ask before randomized measured runs",
        "cache_state": "warm after one unscored request; run order randomized",
        "host": {"platform": platform.platform(), "machine": platform.machine(), "processor": platform.processor()},
        "gpu": command_or_unknown(["nvidia-smi", "--query-gpu=name,memory.total,driver_version", "--format=csv,noheader"]),
        "seed": args.seed,
        "federation": {"servers": 10, "tools_full_profile": 200, "transport": "stdio", "fixture_version": 1},
    }
    write_json(scratch / "results/environment.json", environment)

    sizes = [200] if args.quick else (PILOT_SIZES if args.pilot else SIZES)
    matrix: list[dict[str, Any]] = []
    catalogue_profiles: dict[str, Any] = {}
    all_tasks = json.loads((scratch / "fixtures/tasks.json").read_text())["tasks"]
    for size in sizes:
        profile_dir = scratch / "profiles" / str(size)
        profile_dir.mkdir(parents=True, exist_ok=True)
        mcp_json = profile_dir / "mcp.json"
        run([str(eval_bin), "generate-config", "--root", str(scratch), "--binary", str(eval_bin), "--size", str(size), "--output", str(mcp_json)])
        manifest = profile_dir / "catalogue.json"
        run([str(eval_bin), "catalogue", "--mcp-config", str(mcp_json), "--output", str(manifest)], timeout=120)
        retrieval = profile_dir / "retrieval.json"
        run([str(eval_bin), "retrieval", "--manifest", str(manifest), "--tasks", str(scratch / "fixtures/retrieval.json"), "--output", str(retrieval)])
        catalogue = json.loads(manifest.read_text())
        catalogue_profiles[str(size)] = catalogue
        available = {tool["name"] for tool in catalogue["tools"]}
        tasks = [task for task in all_tasks if set(task["required_tools"]).issubset(available)]
        if args.quick:
            tasks = [next(task for task in tasks if task["category"] == category) for category in ["single", "sequence", "cross", "adversarial"]]
        elif args.pilot:
            applicable = {task["id"]: task for task in tasks}
            selected = PILOT_TASK_IDS_BY_SIZE[size]
            missing = [task_id for task_id in selected if task_id not in applicable]
            if missing:
                raise SystemExit(f"pilot tasks unavailable at catalogue size {size}: {missing}")
            tasks = [applicable[task_id] for task_id in selected]
        if args.quick:
            modes = [("deferred", 24)]
        elif args.pilot:
            modes = [("eager", 24), ("deferred", 24), ("auto", 24)]
        else:
            modes = [("eager", 24), ("deferred", 24)] + [("auto", threshold) for threshold in THRESHOLDS]
        for mode, threshold in modes:
            for repetition in range(repetitions):
                for task in tasks:
                    matrix.append({"size": size, "mode": mode, "threshold": threshold, "repetition": repetition + 1, "task": task, "mcp_json": str(mcp_json), "manifest": str(manifest)})

    environment["catalogue_profiles"] = catalogue_profiles
    environment["matrix_runs"] = len(matrix)
    if args.pilot:
        environment["pilot_task_ids_by_size"] = {str(size): task_ids for size, task_ids in PILOT_TASK_IDS_BY_SIZE.items()}
        environment["evidence_note"] = "Predeclared representative subset; pilot/anecdotal, not a complete or statistically significant matrix."
    write_json(scratch / "results/environment.json", environment)
    rng = random.Random(args.seed)
    rng.shuffle(matrix)
    if not matrix:
        raise SystemExit("no applicable evaluation tasks in selected profiles")

    # Warm the local Qwen runtime with a no-tool request under isolated config.
    warm_env = isolated_env(scratch, base_config_path, mode="eager", threshold=24)
    provider_model = f"{provider}:{model}"
    run([str(term_bin), "ask", "--text", "--provider", provider_model, "--max-turns", "1", "Reply with the word warm."], env=warm_env, timeout=600)

    results_path = scratch / "results/runs.jsonl"
    results_path.write_text("")
    for ordinal, item in enumerate(matrix, 1):
        task = item["task"]
        reset_fixture_state(scratch)
        env = isolated_env(scratch, base_config_path, mode=item["mode"], threshold=item["threshold"])
        shutil.copy2(item["mcp_json"], scratch / "config/term-llm/mcp.json")
        mcp_cfg = json.loads(pathlib.Path(item["mcp_json"]).read_text())
        servers = ",".join(sorted(mcp_cfg["servers"]))
        transcript = scratch / "transcripts" / f"{ordinal:06d}-{task['id']}-n{item['size']}-{item['mode']}-t{item['threshold']}-r{item['repetition']}.txt"
        started_at = dt.datetime.now(dt.timezone.utc)
        started = time.perf_counter()
        run_timeout = int(os.environ.get("EVAL_RUN_TIMEOUT", "900"))
        timed_out = False
        try:
            proc = subprocess.run([str(term_bin), "ask", "--json", "--provider", provider_model, "--mcp", servers,
                                  "--max-turns", str(args.max_turns), task["prompt"]], cwd=ROOT, env=env,
                                  timeout=run_timeout, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        except subprocess.TimeoutExpired as exc:
            timed_out = True
            proc = subprocess.CompletedProcess(exc.cmd, 124, stdout=coerce_text(exc.stdout),
                                               stderr=coerce_text(exc.stderr) + f"\nevaluation timeout after {run_timeout}s")
        elapsed = time.perf_counter() - started
        transcript.write_text("# stdout JSONL\n" + proc.stdout + "\n# stderr\n" + proc.stderr)
        events = parse_jsonl_text(proc.stdout)
        first_output_seconds = event_delay_seconds(started_at, events, lambda event: event.get("type") in {"text.delta", "tool.started"})
        first_external_seconds = event_delay_seconds(started_at, events, lambda event: event.get("type") == "tool.started" and event.get("name") != "tool_search")
        calls = read_jsonl(scratch / "calls.jsonl")
        called = [f"{call['server']}__{call['tool']}" for call in calls]
        required = task["required_tools"]
        forbidden = task.get("forbidden_tools", [])
        required_sequence = task.get("required_sequence", [])
        permitted = set(task.get("permitted_expected_tools", required))
        mutation_observed = any(call.get("result_class") == "mutation" for call in calls)
        expected_state = task.get("expected_final_state", {})
        final_state_ok = mutation_observed == bool(expected_state.get("mutation_logged", task.get("mutation", False)))
        if expected_state.get("catalogue_refresh_emitted"):
            final_state_ok = final_state_ok and any(call.get("catalogue_refresh_emitted") is True for call in calls)
        sequence_ok = not required_sequence or is_subsequence(required_sequence, called)
        wrong_calls = [name for name in called if name not in permitted]
        first_expected = {required_sequence[0]} if required_sequence else permitted
        success = (proc.returncode == 0 and all(name in called for name in required)
                   and not any(name in called for name in forbidden) and sequence_ok and final_state_ok)
        manifest = json.loads(pathlib.Path(item["manifest"]).read_text())
        eager_tokens = sum(tool["estimated_tokens"] for tool in manifest["tools"])
        resolved = item["mode"]
        if resolved == "auto":
            resolved = "eager" if item["size"] <= item["threshold"] else "deferred"
        initial_tokens = eager_tokens if resolved == "eager" else 0
        search = analyze_search_events(eval_bin, scratch, pathlib.Path(item["manifest"]), manifest, events, ordinal, required)
        tool_events = [event for event in events if event.get("type") == "tool.started"]
        tool_failures = [event for event in events if event.get("type") == "tool.completed" and event.get("success") is False]
        usage_events = [event for event in events if event.get("type") == "usage"]
        activated_set = set(search["activated_tools"])
        activated_tokens = sum(tool["estimated_tokens"] for tool in manifest["tools"] if tool["name"] in activated_set)
        record = {k: item[k] for k in ["size", "mode", "threshold", "repetition"]}
        record.update({"task_id": task["id"], "category": task["category"], "success": success,
                       "return_code": proc.returncode, "called_tools": called, "required_tools": required,
                       "required_sequence": required_sequence, "sequence_ok": sequence_ok,
                       "forbidden_tools": forbidden, "wrong_tool_calls": wrong_calls,
                       "mutation_expected": bool(task.get("mutation", False)), "mutation_observed": mutation_observed,
                       "expected_final_state": expected_state, "final_state_ok": final_state_ok,
                       "correct_first_external_tool": bool(called and called[0] in first_expected),
                       "external_tool_calls": len(called), "search_calls": search["search_calls"],
                       "empty_searches": search["empty_searches"], "search_details": search["details"],
                       "activated_tools": search["activated_tools"],
                       "activated_schema_tokens": activated_tokens,
                       "required_tool_recall_at_5": search["required_tool_recall_at_5"],
                       "wall_seconds": elapsed, "time_to_first_provider_output_seconds": first_output_seconds,
                       "time_to_first_external_tool_seconds": first_external_seconds,
                       "provider_turns": len(usage_events), "timed_out": timed_out,
                       "provider_usage": sum_usage(usage_events),
                       "max_turns_exhausted": "maximum turns" in proc.stderr.lower(),
                       "eager_schema_tokens": eager_tokens, "initial_mcp_schema_tokens": initial_tokens,
                       "initial_token_reduction": 0 if eager_tokens == 0 else 1 - initial_tokens / eager_tokens,
                       "tool_event_count": len(tool_events), "tool_execution_failures": len(tool_failures),
                       "transcript": str(transcript.relative_to(scratch))})
        with results_path.open("a") as out:
            out.write(json.dumps(record, sort_keys=True) + "\n")
        print(f"[{ordinal}/{len(matrix)}] {task['id']} n={item['size']} {item['mode']} t={item['threshold']} r={item['repetition']} success={success}", flush=True)

    summary_path = scratch / "results/summary.json"
    summarize(results_path, summary_path)
    if args.pilot:
        summary = json.loads(summary_path.read_text())
        summary["evaluation_scope"] = "pilot/anecdotal"
        summary["evidence_note"] = "Predeclared representative subset; not a complete or statistically significant matrix."
        write_json(summary_path, summary)
    print(f"Results: {summary_path}")
    return 0


def isolated_env(scratch: pathlib.Path, base_config: pathlib.Path, *, mode: str, threshold: int) -> dict[str, str]:
    config_dir = scratch / "config/term-llm"
    config_dir.mkdir(parents=True, exist_ok=True)
    text = base_config.read_text().rstrip() + f"\n\ntool_discovery:\n  mode: {mode}\n  threshold: {threshold}\n"
    (config_dir / "config.yaml").write_text(text)
    env = os.environ.copy()
    env.update({"HOME": str(scratch), "XDG_CONFIG_HOME": str(scratch / "config"),
                "XDG_DATA_HOME": str(scratch / "data"), "XDG_CACHE_HOME": str(scratch / "cache")})
    return env


def reset_fixture_state(root: pathlib.Path) -> None:
    shutil.rmtree(root / "state", ignore_errors=True)
    (root / "state").mkdir(parents=True, exist_ok=True)
    (root / "calls.jsonl").write_text("")


def coerce_text(value: str | bytes | None) -> str:
    if value is None:
        return ""
    if isinstance(value, bytes):
        return value.decode(errors="replace")
    return value


def parse_jsonl_text(text: str) -> list[dict[str, Any]]:
    events: list[dict[str, Any]] = []
    for line in text.splitlines():
        if not line.strip():
            continue
        try:
            value = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(value, dict):
            events.append(value)
    return events


def event_delay_seconds(started_at: dt.datetime, events: list[dict[str, Any]], predicate: Any) -> float | None:
    for event in events:
        if not predicate(event):
            continue
        timestamp = event.get("ts")
        if not isinstance(timestamp, str):
            return None
        try:
            observed = dt.datetime.fromisoformat(timestamp.replace("Z", "+00:00"))
        except ValueError:
            return None
        return max(0.0, (observed - started_at).total_seconds())
    return None


def is_subsequence(required: list[str], actual: list[str]) -> bool:
    cursor = iter(actual)
    return all(any(candidate == wanted for candidate in cursor) for wanted in required)


def sum_usage(events: list[dict[str, Any]]) -> dict[str, int]:
    keys = ["input_tokens", "output_tokens", "cached_input_tokens", "cache_write_tokens"]
    return {key: sum(int(event.get(key, 0) or 0) for event in events) for key in keys}


def analyze_search_events(eval_bin: pathlib.Path, scratch: pathlib.Path, manifest_path: pathlib.Path,
                          manifest: dict[str, Any], events: list[dict[str, Any]], ordinal: int,
                          required: list[str]) -> dict[str, Any]:
    tools = manifest.get("tools", [])
    by_name = {tool["name"]: tool for tool in tools}
    by_original: dict[str, list[str]] = {}
    for tool in tools:
        by_original.setdefault(tool["original_name"].lower(), []).append(tool["name"])
    for names in by_original.values():
        names.sort()

    actions: list[dict[str, Any]] = []
    retrieval_tasks: list[dict[str, Any]] = []
    for index, event in enumerate(events):
        if event.get("type") != "tool.started" or event.get("name") != "tool_search":
            continue
        args = event.get("args")
        if isinstance(args, str):
            try:
                args = json.loads(args)
            except json.JSONDecodeError:
                args = {}
        if not isinstance(args, dict):
            args = {}
        query = str(args.get("query", "")).strip()
        names = [str(name).strip() for name in args.get("tool_names", []) if str(name).strip()]
        limit = int(args.get("max_results", 5) or 5)
        limit = max(1, min(8, limit))
        if query:
            case_id = f"search-{index}"
            retrieval_tasks.append({"id": case_id, "prompt": query, "required_tools": required, "max_results": limit})
            actions.append({"id": case_id, "query": query, "tool_names": [], "limit": limit})
        else:
            actions.append({"id": f"exact-{index}", "query": "", "tool_names": names, "limit": len(names)})

    query_results: dict[str, dict[str, Any]] = {}
    if retrieval_tasks:
        work = scratch / "results" / "search-details"
        work.mkdir(parents=True, exist_ok=True)
        tasks_path = work / f"{ordinal:06d}-tasks.json"
        output_path = work / f"{ordinal:06d}-results.json"
        write_json(tasks_path, {"tasks": retrieval_tasks})
        run([str(eval_bin), "retrieval", "--manifest", str(manifest_path), "--tasks", str(tasks_path), "--output", str(output_path)])
        query_results = {case["id"]: case for case in json.loads(output_path.read_text())["cases"]}

    activated: list[str] = []
    active: set[str] = set()
    top5_seen: set[str] = set()
    empty = 0
    details: list[dict[str, Any]] = []
    for action in actions:
        candidates: list[str] = []
        top5: list[str] = []
        if action["query"]:
            result = query_results.get(action["id"], {})
            candidates = list(result.get("results", []))[:action["limit"]]
            top5 = list(result.get("top5", []))
        else:
            for requested in action["tool_names"]:
                if requested in by_name:
                    candidates.append(requested)
                    continue
                matches = by_original.get(requested.lower(), [])
                if len(matches) == 1:
                    candidates.append(matches[0])
            top5 = candidates[:5]
        top5_seen.update(top5)
        if not candidates:
            empty += 1
        for name in candidates:
            if name in active or len(active) >= 16:
                continue
            active.add(name)
            activated.append(name)
        details.append({**action, "candidates": candidates, "top5": top5})
    recall = None if not actions or not required else sum(name in top5_seen for name in required) / len(required)
    return {"search_calls": len(actions), "empty_searches": empty, "activated_tools": activated,
            "required_tool_recall_at_5": recall, "details": details}


def read_jsonl(path: pathlib.Path) -> list[dict[str, Any]]:
    if not path.exists():
        return []
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def summarize(path: pathlib.Path, output: pathlib.Path) -> None:
    rows = read_jsonl(path)
    grouped: dict[str, dict[str, Any]] = {}
    for row in rows:
        key = f"n={row['size']} mode={row['mode']} threshold={row['threshold']}"
        group = grouped.setdefault(key, {"runs": 0, "successes": 0, "correct_first_external": 0,
                                         "wrong_tool_calls": 0, "search_calls": 0, "empty_searches": 0,
                                         "recall_sum": 0.0, "recall_runs": 0, "wall_seconds": 0.0,
                                         "ttfo_sum": 0.0, "ttfo_runs": 0, "ttfext_sum": 0.0, "ttfext_runs": 0,
                                         "initial_mcp_schema_tokens": 0, "eager_schema_tokens": 0,
                                         "activated_schema_tokens": 0})
        group["runs"] += 1
        group["successes"] += int(row["success"])
        group["correct_first_external"] += int(row["correct_first_external_tool"])
        group["wrong_tool_calls"] += len(row["wrong_tool_calls"])
        group["search_calls"] += row["search_calls"]
        group["empty_searches"] += row["empty_searches"]
        if row["required_tool_recall_at_5"] is not None:
            group["recall_sum"] += row["required_tool_recall_at_5"]
            group["recall_runs"] += 1
        group["wall_seconds"] += row["wall_seconds"]
        if row["time_to_first_provider_output_seconds"] is not None:
            group["ttfo_sum"] += row["time_to_first_provider_output_seconds"]
            group["ttfo_runs"] += 1
        if row["time_to_first_external_tool_seconds"] is not None:
            group["ttfext_sum"] += row["time_to_first_external_tool_seconds"]
            group["ttfext_runs"] += 1
        group["initial_mcp_schema_tokens"] += row["initial_mcp_schema_tokens"]
        group["eager_schema_tokens"] += row["eager_schema_tokens"]
        group["activated_schema_tokens"] += row["activated_schema_tokens"]
    for group in grouped.values():
        group["success_rate"] = group["successes"] / group["runs"]
        group["correct_first_external_rate"] = group["correct_first_external"] / group["runs"]
        group["mean_wall_seconds"] = group["wall_seconds"] / group["runs"]
        group["mean_time_to_first_provider_output_seconds"] = (None if group["ttfo_runs"] == 0 else group["ttfo_sum"] / group["ttfo_runs"])
        group["mean_time_to_first_external_tool_seconds"] = (None if group["ttfext_runs"] == 0 else group["ttfext_sum"] / group["ttfext_runs"])
        group["mean_search_calls"] = group["search_calls"] / group["runs"]
        group["mean_activated_schema_tokens"] = group["activated_schema_tokens"] / group["runs"]
        group["mean_required_tool_recall_at_5"] = (None if group["recall_runs"] == 0
                                                     else group["recall_sum"] / group["recall_runs"])
        group["initial_token_reduction"] = 1 - group["initial_mcp_schema_tokens"] / max(1, group["eager_schema_tokens"])
        del group["recall_sum"]
        del group["ttfo_sum"]
        del group["ttfext_sum"]
    write_json(output, {"groups": grouped, "raw_runs": len(rows)})


def command_or_unknown(command: list[str]) -> str:
    try:
        return subprocess.run(command, timeout=10, check=False, text=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL).stdout.strip() or "unknown"
    except (OSError, subprocess.TimeoutExpired):
        return "unknown"


def write_json(path: pathlib.Path, value: Any) -> None:
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


if __name__ == "__main__":
    raise SystemExit(main())
