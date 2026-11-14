package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/testutil"
)

// compositionRunner will not finish either worker until both have started.
// This catches accidental sequential orchestration without timing sleeps.
type compositionRunner struct {
	mu     sync.Mutex
	models []string
	ready  chan struct{}
}

func (r *compositionRunner) RunAgent(ctx context.Context, name, prompt string, depth int) (SpawnAgentRunResult, error) {
	return r.run(ctx, SpawnAgentRunOptions{})
}
func (r *compositionRunner) RunAgentWithCallback(ctx context.Context, name, prompt string, depth int, id string, cb SubagentEventCallback) (SpawnAgentRunResult, error) {
	return r.run(ctx, SpawnAgentRunOptions{})
}
func (r *compositionRunner) RunAgentWithOptions(ctx context.Context, name, prompt string, depth int, opts SpawnAgentRunOptions) (SpawnAgentRunResult, error) {
	return r.run(ctx, opts)
}
func (r *compositionRunner) RunAgentWithCallbackAndOptions(ctx context.Context, name, prompt string, depth int, id string, cb SubagentEventCallback, opts SpawnAgentRunOptions) (SpawnAgentRunResult, error) {
	return r.run(ctx, opts)
}
func (r *compositionRunner) run(ctx context.Context, opts SpawnAgentRunOptions) (SpawnAgentRunResult, error) {
	r.mu.Lock()
	r.models = append(r.models, opts.ModelOverride)
	if len(r.models) == 2 {
		close(r.ready)
	}
	r.mu.Unlock()
	select {
	case <-r.ready:
		return SpawnAgentRunResult{Output: "OK " + opts.ModelOverride, SessionID: opts.ChildSessionID}, nil
	case <-ctx.Done():
		return SpawnAgentRunResult{}, ctx.Err()
	}
}

func TestCodeModeParallelModelAgentComposition(t *testing.T) {
	for _, toolName := range []string{CodeModeToolName, CodeExecToolName} {
		t.Run(toolName, func(t *testing.T) {
			cfg := DefaultToolConfig()
			cfg.Enabled = []string{toolName, SpawnAgentToolName, UpdatePlanToolName}
			manager, err := NewToolManager(&cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(manager.ApprovalMgr.Close)
			runner := &compositionRunner{ready: make(chan struct{})}
			manager.GetSpawnAgentTool().SetRunner(runner)
			h := testutil.NewEngineHarness()
			t.Cleanup(h.Engine.CloseCodeMode)
			manager.Registry.RegisterWithEngine(h.Engine)
			code := `
const replies = await Promise.all(['mock:first', 'mock:second'].map(model =>
  tools.spawn_agent({agent_name:'developer',prompt:'smoke test',model,wait:0})));
const ids = replies.map(r => {
  if (r.isError || r.timedOut) throw Error(r.text);
  const child = JSON.parse(r.text);
  if (!child.agent_id) throw Error(r.text);
  return child.agent_id;
});
const result = await tools.wait_agent({agent_ids:ids,max_wait:5});
if (result.isError || result.timedOut) throw Error(result.text);
const children = JSON.parse(result.text);
if (children.length !== 2 || children.some(c => c.status !== 'completed')) throw Error(result.text);
const plan = await tools.update_plan({plan:[{step:'Test both workers',status:'completed'}]});
if (plan.isError || plan.timedOut) throw Error(plan.text);
text(result.text);
`
			args := map[string]any{"code": code}
			if toolName == CodeExecToolName {
				args["max_wait"] = 10
			}
			h.Provider.AddToolCall("compose", toolName, args).AddTextResponse("done")
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			const sessionID = "parallel-script-agents"
			_, err = h.Run(core.ContextWithSessionID(ctx, sessionID), core.Request{SessionID: sessionID, Messages: []core.Message{core.UserText("Test both mock models through a script")}, Tools: manager.GetSpecs(), MaxTurns: 3})
			if err != nil {
				t.Fatal(err)
			}
			runner.mu.Lock()
			models := append([]string(nil), runner.models...)
			runner.mu.Unlock()
			if len(models) != 2 {
				t.Fatalf("script did not start both workers: %v", models)
			}
			var found bool
			for _, request := range h.Provider.RecordedRequests() {
				for _, message := range request.Messages {
					for _, part := range message.Parts {
						result := part.ToolResult
						if result == nil || result.ID != "compose" {
							continue
						}
						found = true
						content := core.ToolResultTextContent(result)
						if result.IsError || !strings.Contains(content, "mock:first") || !strings.Contains(content, "mock:second") || !strings.Contains(content, `"status":"completed"`) {
							t.Fatalf("script composition failed: %s", content)
						}
					}
				}
			}
			if !found {
				t.Fatal("provider did not receive script tool result")
			}
			tool, _ := manager.Registry.Get(UpdatePlanToolName)
			state, err := tool.(*UpdatePlanTool).controller.load(ctx, sessionID)
			if err != nil || !state.present {
				t.Fatalf("nested plan was not persisted to host controller: %+v, %v", state, err)
			}
			data, _ := json.Marshal(state.snapshot)
			if !strings.Contains(string(data), "Test both workers") {
				t.Fatalf("host plan has wrong state: %s", data)
			}
		})
	}
}
