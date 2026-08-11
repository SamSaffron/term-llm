#!/usr/bin/env python3
import importlib.util
import pathlib
import unittest

SCRIPT = pathlib.Path(__file__).with_name("run_oracle_evaluation.py")
SPEC = importlib.util.spec_from_file_location("oracle_eval", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(MODULE)


class OracleSummaryTest(unittest.TestCase):
    def test_initial_cache_metrics_are_separate_from_agentic_totals(self) -> None:
        row = {
            "provider": "chatgpt", "mode": "deferred", "task_success": True,
            "correct_external_tool_call": True, "oracle_exact": True,
            "wrong_tool_calls": [], "search_calls": 1, "provider_turns": 3,
            "wall_seconds": 2.0, "provider_observed_input_tokens": 2000,
            "provider_usage": {"input_tokens": 1200, "cached_input_tokens": 800,
                               "cache_write_tokens": 0, "output_tokens": 50},
            "provider_usage_turns": [
                {"input_tokens": 800, "cached_input_tokens": 200, "output_tokens": 10},
                {"input_tokens": 200, "cached_input_tokens": 300, "output_tokens": 20},
                {"input_tokens": 200, "cached_input_tokens": 300, "output_tokens": 20},
            ],
            "initial_mcp_schema_estimated_tokens": 0,
            "activated_schema_estimated_tokens": 400,
        }
        environment = {
            "evaluation_scope": "test", "shared_opening_context": {},
            "limitations_predeclared": [],
        }
        group = MODULE.summarize([row], [], environment)["groups"]["chatgpt:deferred"]
        self.assertEqual(group["initial_provider_observed_input_tokens"], 1000)
        self.assertEqual(group["initial_provider_cached_input_tokens"], 200)
        self.assertAlmostEqual(group["initial_cache_hit_ratio"], 0.2)
        self.assertAlmostEqual(group["cache_hit_ratio"], 0.4)
    def test_protocol_audit_detects_stable_native_top_level_tools(self) -> None:
        def section(payload: dict) -> str:
            import json
            return ("[x] Responses WebSocket Request (reused=true)\n" + json.dumps(payload) +
                    "\n[x] END Responses WebSocket Request (reused=true)\n")
        native_tool = {"type": "tool_search", "execution": "client"}
        stderr = section({"tools": [native_tool], "input": [{"type": "message"}]}) + section({
            "tools": [native_tool], "previous_response_id": "resp_1",
            "input": [{"type": "tool_search_output", "tools": [
                {"type": "function", "name": "eta", "defer_loading": True}
            ]}],
        })
        audit = MODULE.analyze_debug_protocol(stderr)
        self.assertTrue(audit["top_level_tools_stable"])
        self.assertFalse(audit["loaded_schemas_in_top_level_tools"])
        self.assertEqual(audit["previous_response_requests"], 1)

    def test_strategy_is_separate_summary_dimension(self) -> None:
        base = {
            "provider": "chatgpt", "mode": "auto", "task_success": True,
            "correct_external_tool_call": True, "oracle_exact": True,
            "wrong_tool_calls": [], "search_calls": 1, "provider_turns": 1,
            "wall_seconds": 1.0, "provider_observed_input_tokens": 10,
            "provider_usage": {"input_tokens": 10, "cached_input_tokens": 0,
                               "cache_write_tokens": 0, "output_tokens": 1},
            "provider_usage_turns": [{"input_tokens": 10}],
            "initial_mcp_schema_estimated_tokens": 0,
            "activated_schema_estimated_tokens": 1,
        }
        environment = {"evaluation_scope": "test", "shared_opening_context": {},
                       "limitations_predeclared": []}
        portable = dict(base, strategy="portable")
        native = dict(base, strategy="native")
        groups = MODULE.summarize([portable, native], [], environment)["groups"]
        self.assertEqual(set(groups), {"chatgpt:portable", "chatgpt:native"})


if __name__ == "__main__":
    unittest.main()
