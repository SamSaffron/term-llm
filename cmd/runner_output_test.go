package cmd

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestResultCollectorsDiscardAbandonedAttempts(t *testing.T) {
	for _, earlierTurn := range []bool{false, true} {
		name := "first_turn"
		wantText, wantThinking := "correct result", "accepted reasoning"
		var events []core.Event
		if earlierTurn {
			name = "after_tool_turn"
			wantText = "earlier output; " + wantText
			// Reusing the earlier item ID ensures discard restores item-boundary state.
			wantThinking = "earlier reasoning" + wantThinking
			events = append(events,
				core.Event{Type: core.EventTextDelta, Text: "earlier output; ", ProviderTurnIndexSet: true},
				core.Event{Type: core.EventReasoningDelta, Text: "earlier reasoning", ReasoningItemID: "earlier", ProviderTurnIndexSet: true},
				core.Event{Type: core.EventToolCall, Tool: &core.ToolCall{ID: "tool", Name: "test"}, ProviderTurnIndexSet: true},
				core.Event{Type: core.EventUsage, Use: &core.Usage{InputTokens: 1}},
				core.Event{Type: core.EventToolExecStart, ToolCallID: "tool"},
				core.Event{Type: core.EventToolExecEnd, ToolCallID: "tool", ToolSuccess: true},
			)
		}
		// Simple engine streams have no turn index. Agentic streams tag model
		// events, but engine-generated retry discards can be untagged.
		events = append(events,
			core.Event{Type: core.EventTextDelta, Text: "obsolete draft", ProviderTurnIndex: 1, ProviderTurnIndexSet: earlierTurn},
			core.Event{Type: core.EventReasoningDelta, Text: "obsolete reasoning", ReasoningItemID: "abandoned", ProviderTurnIndex: 1, ProviderTurnIndexSet: earlierTurn},
			core.Event{Type: core.EventUsage, Use: &core.Usage{InputTokens: 2}},
			core.Event{Type: core.EventAttemptDiscard},
			core.Event{Type: core.EventAttemptDiscard},
			core.Event{Type: core.EventTextDelta, Text: "correct result", ProviderTurnIndex: 1, ProviderTurnIndexSet: earlierTurn},
			core.Event{Type: core.EventReasoningDelta, Text: "accepted reasoning", ReasoningItemID: "earlier", ProviderTurnIndex: 1, ProviderTurnIndexSet: earlierTurn},
			core.Event{Type: core.EventUsage, Use: &core.Usage{InputTokens: 3}},
		)
		t.Run(name, func(t *testing.T) {
			sink := &spawnRunSink{}
			collector := &runnerEventCollector{sink: sink}
			for _, ev := range events {
				if err := collector.Event(ev); err != nil {
					t.Fatal(err)
				}
			}
			result := collector.Result("session")
			if result.Response != wantText || result.Thinking != wantThinking {
				t.Errorf("collector result = %q / %q, want %q / %q", result.Response, result.Thinking, wantText, wantThinking)
			}
			output, err := completeChildAgent(nil, result, sink.Output(), "", true)
			if err != nil || output != wantText {
				t.Errorf("child output = %q, %v; want %q", output, err, wantText)
			}

			runner := &jobsV2LLMRunner{exec: func(_ context.Context, _ jobsV2LLMConfig, onEvent func(core.Event)) (serveJobsExecResult, error) {
				for _, ev := range events {
					onEvent(ev)
				}
				return serveJobsExecResult{}, nil
			}}
			job := jobsV2Job{RunnerConfig: json.RawMessage(`{"agent_name":"test","instructions":"test","cwd":"."}`)}
			var flushed string
			res, err := runner.Run(context.Background(), job, func(kind, message string, _ any) {
				if kind == "response_flush" {
					flushed = message
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			if res.Response != wantText || res.Thinking != wantThinking || flushed != wantText {
				t.Errorf("job result = %q / %q, flush = %q; want %q / %q", res.Response, res.Thinking, flushed, wantText, wantThinking)
			}

			mgr := newJobsV2ManagerWithoutLoops(t)
			storedJob, err := mgr.CreateJob(jobsV2Job{Name: "retry-result", Enabled: true, RunnerType: jobsV2RunnerLLM, RunnerConfig: job.RunnerConfig, TriggerType: jobsV2TriggerManual, TriggerConfig: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			run, err := mgr.TriggerJob(storedJob.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := mgr.finishRun(run.ID, jobsV2RunSucceeded, res, nil, run.Attempt); err != nil {
				t.Fatal(err)
			}
			var response, thinking string
			if err := mgr.db.QueryRow(`SELECT response, thinking FROM job_runs_v2 WHERE id = ?`, run.ID).Scan(&response, &thinking); err != nil {
				t.Fatal(err)
			}
			if response != wantText || thinking != wantThinking {
				t.Errorf("persisted result = %q / %q; want %q / %q", response, thinking, wantText, wantThinking)
			}
		})
	}
}
