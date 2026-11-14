package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/testutil"
)

func TestCodeModeGetGoalKeepsOriginatingScopeAndExpires(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "loop_exit", true: "next_goal_loop"}[replace], func(t *testing.T) {
			cfg := DefaultToolConfig()
			cfg.Enabled = []string{CodeExecToolName}
			manager, err := NewToolManager(&cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(manager.ApprovalMgr.Close)
			h := testutil.NewEngineHarness()
			t.Cleanup(h.Engine.CloseCodeMode)
			manager.Registry.RegisterWithEngine(h.Engine)
			h.AddTool(NewGetGoalTool(func() *session.Goal { return &session.Goal{Objective: "originating goal"} }))
			endLoop := testutil.NewMockToolWithSchema("end_goal_loop", "end the scoped fixture", map[string]any{"type": "object"}, func(context.Context, json.RawMessage) (core.ToolOutput, error) {
				h.Engine.UnregisterTool(GetGoalToolName)
				if replace {
					if err := h.Engine.RegisterTool(NewGetGoalTool(func() *session.Goal { return &session.Goal{Objective: "other session goal"} })); err != nil {
						return core.ToolOutput{}, err
					}
				}
				return core.TextOutput("loop ended"), nil
			})
			endLoop.Exposure = core.CodeModeExposure{Allowed: true}
			h.AddTool(endLoop)
			code := `text((await tools.get_goal({})).text); await tools.end_goal_loop({});
let denied=false; try { await tools.get_goal({}); } catch (e) {
 if(e.code!=='schema_changed' && e.code!=='permission_denied') throw e;
 denied=true;
} if(!denied) throw Error('scoped goal survived loop exit'); text('goal access expired');`
			h.Provider.AddToolCall("goal-script", CodeExecToolName, map[string]any{"code": code, "max_wait": 5}).AddTextResponse("done")
			if _, err := h.Run(context.Background(), core.Request{SessionID: "originating-session", Messages: []core.Message{core.UserText("inspect scoped goal")}, Tools: h.Registry.AllSpecs(), MaxTurns: 3}); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, request := range h.Provider.RecordedRequests() {
				for _, message := range request.Messages {
					for _, part := range message.Parts {
						if result := part.ToolResult; result != nil && result.ID == "goal-script" {
							found = true
							text := core.ToolResultTextContent(result)
							if result.IsError || !strings.Contains(text, "originating goal") || !strings.Contains(text, "goal access expired") || strings.Contains(text, "other session goal") {
								t.Fatalf("goal scope was lost: %s", text)
							}
						}
					}
				}
			}
			if !found {
				t.Fatal("missing scoped goal script result")
			}
		})
	}
}
