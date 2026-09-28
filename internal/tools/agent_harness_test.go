package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/testutil"
)

type scriptedLifecycleProvider struct {
	*llm.MockProvider
	turn int
}

func (p *scriptedLifecycleProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.turn++
	switch p.turn {
	case 1:
		p.AddToolCall("spawn", "spawn_agent", map[string]any{"agent_name": "developer", "prompt": "work", "wait": 0})
	case 2, 3:
		id := ""
		for _, msg := range req.Messages {
			for _, part := range msg.Parts {
				if part.Type != llm.PartToolResult || part.ToolResult == nil {
					continue
				}
				var result SpawnAgentResult
				content := part.ToolResult.Content
				if strings.HasPrefix(content, "[") {
					var items []json.RawMessage
					if json.Unmarshal([]byte(content), &items) == nil && len(items) > 0 {
						content = string(items[0])
					}
				}
				if json.Unmarshal([]byte(content), &result) == nil && result.AgentID != "" {
					id = result.AgentID
				}
			}
		}
		if p.turn == 2 {
			p.AddToolCall("wait", "wait_agent", map[string]any{"agent_ids": []string{id}, "max_wait": 1})
		} else {
			p.AddToolCall("continue", "continue_agent", map[string]any{"agent_id": id, "instructions": "finish review", "wait": 1})
		}
	default:
		p.AddTextResponse("all done")
	}
	return p.MockProvider.Stream(ctx, req)
}

func TestAgentLifecycleEngineHarness(t *testing.T) {
	cfg := DefaultToolConfig()
	cfg.Enabled = []string{SpawnAgentToolName}
	manager, err := NewToolManager(&cfg, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runner := &lifecycleRunner{entered: make(chan string, 2), release: make(chan struct{})}
	close(runner.release)
	manager.GetSpawnAgentTool().SetRunner(runner)
	harness := testutil.NewEngineHarness()
	provider := &scriptedLifecycleProvider{MockProvider: harness.Provider}
	harness.Engine = llm.NewEngine(provider, harness.Registry)
	for _, name := range []string{SpawnAgentToolName, WaitAgentToolName, ContinueAgentToolName, CancelAgentToolName, ListAgentsToolName} {
		tool, ok := manager.Registry.Get(name)
		if !ok {
			t.Fatalf("missing lifecycle tool %s", name)
		}
		harness.AddTool(tool)
	}
	text, err := harness.Run(llm.ContextWithSessionID(context.Background(), "parent"), llm.Request{Messages: []llm.Message{llm.UserText("delegate then check")}, Tools: manager.GetSpecs(), MaxTurns: 8})
	if err != nil {
		t.Fatal(err)
	}
	runner.mu.Lock()
	resumed := runner.resumed
	runner.mu.Unlock()
	if text != "all done" || provider.turn != 4 || resumed != 1 {
		t.Fatalf("harness result=%q turns=%d resumes=%d", text, provider.turn, resumed)
	}
}
