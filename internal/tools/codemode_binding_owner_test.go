package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/testutil"
)

type foreignNamedScriptTool struct{ name string }

func (t foreignNamedScriptTool) Spec() core.ToolSpec {
	return core.ToolSpec{Name: t.name, Schema: map[string]any{"type": "object"}}
}
func (t foreignNamedScriptTool) Execute(context.Context, json.RawMessage) (core.ToolOutput, error) {
	return core.TextOutput("foreign:" + t.name), nil
}
func (foreignNamedScriptTool) Preview(json.RawMessage) string { return "" }
func (foreignNamedScriptTool) CodeModeExposure() core.CodeModeExposure {
	return core.CodeModeExposure{Allowed: true}
}
func (foreignNamedScriptTool) ToolOrigin() core.ToolOrigin {
	return core.ToolOrigin{Class: core.ToolOriginHost, ID: "foreign script fixture"}
}

func TestCodeModeAuthorityCannotReplaceForeignSameNamedTools(t *testing.T) {
	for _, name := range []string{ReadFileToolName, ShellToolName, SpawnAgentToolName} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/local=%t", name, enabled), func(t *testing.T) {
				cfg := DefaultToolConfig()
				cfg.Enabled = []string{CodeExecToolName}
				if enabled {
					cfg.Enabled = append(cfg.Enabled, name)
				}
				manager, err := NewToolManager(&cfg, nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(manager.ApprovalMgr.Close)
				h := testutil.NewEngineHarness()
				t.Cleanup(h.Engine.CloseCodeMode)
				manager.Registry.RegisterWithEngine(h.Engine)
				if err := h.Engine.RegisterToolOverride(foreignNamedScriptTool{name: name}, "foreign-origin regression fixture"); err != nil {
					t.Fatal(err)
				}
				h.Provider.AddToolCall("foreign-script", CodeExecToolName, map[string]any{"code": fmt.Sprintf("text((await tools[%q]({})).text)", name), "max_wait": 5}).AddTextResponse("done")
				_, err = h.Run(context.Background(), core.Request{SessionID: "foreign-origin", Messages: []core.Message{core.UserText("call the admitted foreign tool")}, Tools: h.Registry.AllSpecs(), MaxTurns: 3})
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, request := range h.Provider.RecordedRequests() {
					for _, message := range request.Messages {
						for _, part := range message.Parts {
							if result := part.ToolResult; result != nil && result.ID == "foreign-script" {
								found = true
								if text := core.ToolResultTextContent(result); result.IsError || !strings.Contains(text, "foreign:"+name) {
									t.Fatalf("local authority replaced foreign execution: %s", text)
								}
							}
						}
					}
				}
				if !found {
					t.Fatal("script result missing")
				}
			})
		}
	}
}
