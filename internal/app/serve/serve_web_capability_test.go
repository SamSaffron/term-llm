package serve

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/engine"
	"github.com/term-llm/term-llm/internal/tools"
)

type webCapabilityTestTool struct{ name string }

func (t webCapabilityTestTool) Spec() core.ToolSpec { return core.ToolSpec{Name: t.name} }
func (t webCapabilityTestTool) Execute(context.Context, json.RawMessage) (core.ToolOutput, error) {
	return core.TextOutput(""), nil
}
func (t webCapabilityTestTool) Preview(json.RawMessage) string { return "" }

func TestRuntimeApprovalPolicyReportsApprovalActions(t *testing.T) {
	if policy := runtimeApprovalPolicy(nil); policy["approval_actions"] != nil {
		t.Fatalf("unknown runtime must not claim an approval capability: %#v", policy)
	}
	runtime := &serveRuntime{approvalDefault: tools.ModePrompt}
	if policy := runtimeApprovalPolicy(runtime); policy["approval_actions"] != nil {
		t.Fatalf("uninspected runtime must stay unknown: %#v", policy)
	}

	registered := engine.NewEngine(nil, engine.NewToolRegistry())
	registered.Tools().Register(webCapabilityTestTool{name: tools.ActivateSkillToolName})
	policy := runtimeApprovalPolicy(&serveRuntime{approvalDefault: tools.ModePrompt, engine: registered})
	if policy["approval_actions"] != false || policy["controls_available"] != false {
		t.Fatalf("skill-only runtime policy = %#v, want no approval-governed actions", policy)
	}

	governed := engine.NewEngine(nil, engine.NewToolRegistry())
	governed.Tools().Register(webCapabilityTestTool{name: tools.ShellToolName})
	policy = runtimeApprovalPolicy(&serveRuntime{approvalDefault: tools.ModePrompt, engine: governed})
	if policy["approval_actions"] != true {
		t.Fatalf("shell runtime policy = %#v, want approval-governed actions", policy)
	}
	governed.SetAllowedToolsFilter([]string{})
	policy = runtimeApprovalPolicy(&serveRuntime{approvalDefault: tools.ModePrompt, engine: governed})
	if policy["approval_actions"] != false {
		t.Fatalf("filtered shell runtime policy = %#v, want no executable approval-governed actions", policy)
	}

	mcpOnly := engine.NewEngine(nil, engine.NewToolRegistry())
	mcpOnly.Tools().Register(webCapabilityTestTool{name: "mcp__github__search"})
	policy = runtimeApprovalPolicy(&serveRuntime{approvalDefault: tools.ModePrompt, engine: mcpOnly})
	if policy["approval_actions"] != true {
		t.Fatalf("MCP runtime policy = %#v, want approval-governed actions", policy)
	}
}
