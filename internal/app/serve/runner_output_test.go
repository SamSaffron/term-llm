package serve

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

			runner := &jobsV2LLMRunner{exec: func(_ context.Context, _ JobsV2LLMConfig, onEvent func(core.Event)) (serveJobsExecResult, error) {
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

			completions := make(chan jobsV2Run, 1)
			mgr := newJobsV2HostManager(t, 1, map[jobsV2RunnerType]jobsV2Runner{
				jobsV2RunnerLLM: jobsV2HostRunnerFunc(func(context.Context, jobsV2Job, progressWriter) (jobsV2RunResult, error) {
					return res, nil
				}),
			}, func(_ context.Context, run jobsV2Run, _ jobsV2Job, _ jobsV2RunStatus, _ jobsV2RunResult, _ string, _ bool, _ string) error {
				completions <- run
				return nil
			})
			storedJob, err := mgr.CreateJob(jobsV2Job{Name: "retry-result", Enabled: true, RunnerType: jobsV2RunnerLLM, RunnerConfig: job.RunnerConfig, TriggerType: jobsV2TriggerManual, TriggerConfig: json.RawMessage(`{}`)})
			if err != nil {
				t.Fatal(err)
			}
			run, err := mgr.TriggerJob(storedJob.ID)
			if err != nil {
				t.Fatal(err)
			}
			completed := waitJobsV2HostCompletion(t, completions)
			if completed.ID != run.ID {
				t.Fatalf("completed run ID = %q, want %q", completed.ID, run.ID)
			}
			stored, err := mgr.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Response != wantText || stored.Thinking != wantThinking {
				t.Errorf("persisted result = %q / %q; want %q / %q", stored.Response, stored.Thinking, wantText, wantThinking)
			}
		})
	}
}
