package ask

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestEnsureOutputToolCapturedCoaxesModelWithoutAssistantText(t *testing.T) {
	provider := mockprovider.NewMockProvider("mock").WithCapabilities(core.Capabilities{ToolCalls: true, SupportsToolChoice: true})
	provider.AddToolCall("call_1", "submit_result", map[string]string{"result_json": `{"ok":true}`})
	outputTool := tools.NewSetOutputTool("submit_result", "result_json", "Submit the result", nil)

	if err := EnsureOutputToolCaptured(context.Background(), provider, nil, core.Request{}, outputTool, ""); err != nil {
		t.Fatalf("EnsureOutputToolCaptured() error = %v", err)
	}
	if got := outputTool.Value(); got != `{"ok":true}` {
		t.Fatalf("outputTool.Value() = %q", got)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(provider.Requests))
	}
	last := provider.Requests[0].Messages[len(provider.Requests[0].Messages)-1]
	if last.Role != core.RoleUser || !strings.Contains(last.Parts[0].Text, "MUST now make exactly one tool call") {
		t.Fatalf("finalizer prompt = %#v", last)
	}
}

func TestEnsureOutputToolCapturedNoopsAfterToolCapturesValue(t *testing.T) {
	provider := mockprovider.NewMockProvider("mock").WithCapabilities(core.Capabilities{ToolCalls: true, SupportsToolChoice: true})
	outputTool := tools.NewSetOutputTool("submit_result", "result_json", "Submit the result", nil)

	args, _ := json.Marshal(map[string]string{"result_json": `{"ok":true}`})
	if _, err := outputTool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if err := EnsureOutputToolCaptured(context.Background(), provider, nil, core.Request{}, outputTool, ""); err != nil {
		t.Fatalf("EnsureOutputToolCaptured() error = %v", err)
	}
	if len(provider.Requests) != 0 {
		t.Fatalf("provider saw %d requests, want 0", len(provider.Requests))
	}
}

func TestEnsureOutputToolCapturedAllowsEmptyStringValue(t *testing.T) {
	provider := mockprovider.NewMockProvider("mock").WithCapabilities(core.Capabilities{ToolCalls: true, SupportsToolChoice: true})
	outputTool := tools.NewSetOutputTool("submit_result", "result_json", "Submit the result", nil)

	args, _ := json.Marshal(map[string]string{"result_json": ""})
	if _, err := outputTool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if err := EnsureOutputToolCaptured(context.Background(), provider, nil, core.Request{}, outputTool, ""); err != nil {
		t.Fatalf("EnsureOutputToolCaptured() error = %v", err)
	}
	if got := outputTool.Value(); got != "" {
		t.Fatalf("outputTool.Value() = %q, want empty string", got)
	}
	if !outputTool.Captured() {
		t.Fatal("expected output tool to report captured")
	}
	if len(provider.Requests) != 0 {
		t.Fatalf("provider saw %d requests, want 0", len(provider.Requests))
	}
}

func TestRunOutputToolFinalizationUsesOnlyOutputToolAndForcesIt(t *testing.T) {
	provider := mockprovider.NewMockProvider("mock").WithCapabilities(core.Capabilities{ToolCalls: true, SupportsToolChoice: true})
	provider.AddToolCall("call_1", "submit_result", map[string]string{"result_json": `{"status":"ok"}`})
	outputTool := tools.NewSetOutputTool("submit_result", "result_json", "Submit the result", nil)
	baseReq := core.Request{
		Model:      "mock-model",
		WorkingDir: t.TempDir(),
		Messages: []core.Message{
			core.SystemText("You are a test agent."),
			core.UserText("Review the thing."),
		},
		Tools: []core.ToolSpec{
			outputTool.Spec(),
			{Name: "read_file", Description: "Read a file"},
		},
		ToolChoice:        core.ToolChoice{Mode: core.ToolChoiceAuto},
		ParallelToolCalls: true,
		MaxTurns:          20,
	}

	registry := engine.NewToolRegistry()
	registry.Register(outputTool)
	if err := runOutputToolFinalization(context.Background(), provider, registry, baseReq, outputTool, "The review is complete."); err != nil {
		t.Fatalf("runOutputToolFinalization() error = %v", err)
	}
	if got := outputTool.Value(); got != `{"status":"ok"}` {
		t.Fatalf("outputTool.Value() = %q", got)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(provider.Requests))
	}
	finalReq := provider.Requests[0]
	if finalReq.WorkingDir != baseReq.WorkingDir {
		t.Fatalf("finalizer WorkingDir = %q, want %q", finalReq.WorkingDir, baseReq.WorkingDir)
	}
	if len(finalReq.Tools) != 1 || finalReq.Tools[0].Name != "submit_result" {
		t.Fatalf("finalizer tools = %#v, want only submit_result", finalReq.Tools)
	}
	if finalReq.ToolChoice.Mode != core.ToolChoiceName || finalReq.ToolChoice.Name != "submit_result" {
		t.Fatalf("ToolChoice = %#v, want forced submit_result", finalReq.ToolChoice)
	}
	if finalReq.ParallelToolCalls {
		t.Fatal("finalizer should disable parallel tool calls")
	}
	if finalReq.MaxTurns != 3 {
		t.Fatalf("MaxTurns = %d, want 3", finalReq.MaxTurns)
	}
	last := finalReq.Messages[len(finalReq.Messages)-1]
	if last.Role != core.RoleUser || !strings.Contains(last.Parts[0].Text, "The task is DONE") || !strings.Contains(last.Parts[0].Text, "MUST now make exactly one tool call") {
		t.Fatalf("finalizer prompt = %#v", last)
	}
}

func TestRunOutputToolFinalizationFallsBackToAutoWhenToolChoiceUnsupported(t *testing.T) {
	provider := mockprovider.NewMockProvider("mock").WithCapabilities(core.Capabilities{ToolCalls: true, SupportsToolChoice: false})
	provider.AddToolCall("call_1", "submit_result", map[string]string{"result_json": `{"status":"ok"}`})
	outputTool := tools.NewSetOutputTool("submit_result", "result_json", "Submit the result", nil)

	if err := runOutputToolFinalization(context.Background(), provider, nil, core.Request{}, outputTool, "Done."); err != nil {
		t.Fatalf("runOutputToolFinalization() error = %v", err)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(provider.Requests))
	}
	if provider.Requests[0].ToolChoice.Mode != core.ToolChoiceAuto {
		t.Fatalf("ToolChoice = %#v, want auto", provider.Requests[0].ToolChoice)
	}
}

func TestRunOutputToolFinalizationRetriesWhenModelRespondsWithText(t *testing.T) {
	provider := mockprovider.NewMockProvider("mock").WithCapabilities(core.Capabilities{ToolCalls: true, SupportsToolChoice: true})
	provider.AddTextResponse("Sorry, here is the final answer in prose.")
	provider.AddTextResponse("Still prose instead of a tool call.")
	provider.AddTextResponse("I continue to be prose-only.")
	provider.AddToolCall("call_4", "submit_result", map[string]string{"result_json": `{"status":"ok"}`})
	outputTool := tools.NewSetOutputTool("submit_result", "result_json", "Submit the result", nil)

	if err := runOutputToolFinalization(context.Background(), provider, nil, core.Request{}, outputTool, "Done."); err != nil {
		t.Fatalf("runOutputToolFinalization() error = %v", err)
	}
	if got := outputTool.Value(); got != `{"status":"ok"}` {
		t.Fatalf("outputTool.Value() = %q", got)
	}
	if len(provider.Requests) != 4 {
		t.Fatalf("provider saw %d requests, want 4", len(provider.Requests))
	}
	last := provider.Requests[3].Messages[len(provider.Requests[3].Messages)-1]
	if !strings.Contains(last.Parts[0].Text, "FINALIZATION RETRY 2") {
		t.Fatalf("retry prompt = %#v", last)
	}
}

func TestRunOutputToolFinalizationFailsWhenToolStillMissing(t *testing.T) {
	provider := mockprovider.NewMockProvider("mock").WithCapabilities(core.Capabilities{ToolCalls: true, SupportsToolChoice: true})
	provider.AddTextResponse("Done.")
	provider.AddTextResponse("Still prose.")
	provider.AddTextResponse("Still not a tool call.")
	outputTool := tools.NewSetOutputTool("submit_result", "result_json", "Submit the result", nil)
	baseReq := core.Request{Messages: []core.Message{core.UserText("Review the thing.")}}

	err := runOutputToolFinalization(context.Background(), provider, nil, baseReq, outputTool, "Done.")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), `output tool "submit_result" was not called`) {
		t.Fatalf("error = %v", err)
	}
}
