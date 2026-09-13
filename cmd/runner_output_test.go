package cmd

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestResultCollectorsDiscardAbandonedAttempts(t *testing.T) {
	for _, earlierTurn := range []bool{false, true} {
		name := "first_turn"
		wantText, wantThinking := "correct result", "accepted reasoning"
		var events []llm.Event
		if earlierTurn {
			name = "after_tool_turn"
			wantText = "earlier output; " + wantText
			// Reusing the earlier item ID ensures discard restores item-boundary state.
			wantThinking = "earlier reasoning" + wantThinking
			events = append(events,
				llm.Event{Type: llm.EventTextDelta, Text: "earlier output; ", ProviderTurnIndexSet: true},
				llm.Event{Type: llm.EventReasoningDelta, Text: "earlier reasoning", ReasoningItemID: "earlier", ProviderTurnIndexSet: true},
				llm.Event{Type: llm.EventToolCall, Tool: &llm.ToolCall{ID: "tool", Name: "test"}, ProviderTurnIndexSet: true},
				llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 1}},
				llm.Event{Type: llm.EventToolExecStart, ToolCallID: "tool"},
				llm.Event{Type: llm.EventToolExecEnd, ToolCallID: "tool", ToolSuccess: true},
			)
		}
		// Simple engine streams have no turn index. Agentic streams tag model
		// events, but engine-generated retry discards can be untagged.
		events = append(events,
			llm.Event{Type: llm.EventTextDelta, Text: "obsolete draft", ProviderTurnIndex: 1, ProviderTurnIndexSet: earlierTurn},
			llm.Event{Type: llm.EventReasoningDelta, Text: "obsolete reasoning", ReasoningItemID: "abandoned", ProviderTurnIndex: 1, ProviderTurnIndexSet: earlierTurn},
			llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 2}},
			llm.Event{Type: llm.EventAttemptDiscard},
			llm.Event{Type: llm.EventAttemptDiscard},
			llm.Event{Type: llm.EventTextDelta, Text: "correct result", ProviderTurnIndex: 1, ProviderTurnIndexSet: earlierTurn},
			llm.Event{Type: llm.EventReasoningDelta, Text: "accepted reasoning", ReasoningItemID: "earlier", ProviderTurnIndex: 1, ProviderTurnIndexSet: earlierTurn},
			llm.Event{Type: llm.EventUsage, Use: &llm.Usage{InputTokens: 3}},
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

			runner := &jobsV2LLMRunner{exec: func(_ context.Context, _ jobsV2LLMConfig, onEvent func(llm.Event)) (serveJobsExecResult, error) {
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

func TestRunnerOutputTaggedDiscardBeforeNewTurnOutput(t *testing.T) {
	var output runnerOutput
	output.Event(llm.Event{Type: llm.EventTextDelta, Text: "accepted", ProviderTurnIndexSet: true})
	// A restart discard can identify a new turn before it has emitted text.
	output.Event(llm.Event{Type: llm.EventAttemptDiscard, ProviderTurnIndex: 2, ProviderTurnIndexSet: true})
	output.Event(llm.Event{Type: llm.EventTextDelta, Text: "abandoned", ProviderTurnIndex: 2, ProviderTurnIndexSet: true})
	output.Event(llm.Event{Type: llm.EventAttemptDiscard, ProviderTurnIndex: 2, ProviderTurnIndexSet: true})
	output.Event(llm.Event{Type: llm.EventTextDelta, Text: " replacement", ProviderTurnIndex: 2, ProviderTurnIndexSet: true})
	if got := output.response.String(); got != "accepted replacement" {
		t.Fatalf("response = %q, want accepted replacement", got)
	}
}
