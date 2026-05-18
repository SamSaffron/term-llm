package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/agents"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestRequiredOutputMissing(t *testing.T) {
	outputTool := tools.NewSetOutputTool("submit_result", "result_json", "Submit the result")
	agent := &agents.Agent{OutputTool: agents.OutputToolConfig{Name: "submit_result", Required: true}}

	if !requiredOutputMissing(agent, outputTool) {
		t.Fatal("expected required output to be missing before the tool captures a value")
	}

	args, _ := json.Marshal(map[string]string{"result_json": `{"ok":true}`})
	if _, err := outputTool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if requiredOutputMissing(agent, outputTool) {
		t.Fatal("did not expect required output to be missing after the tool captures a value")
	}
}

func TestRequiredOutputMissingIgnoresOptionalOutputTool(t *testing.T) {
	outputTool := tools.NewSetOutputTool("submit_result", "result_json", "Submit the result")
	agent := &agents.Agent{OutputTool: agents.OutputToolConfig{Name: "submit_result"}}

	if requiredOutputMissing(agent, outputTool) {
		t.Fatal("optional output tools should preserve existing fallback behavior")
	}
}

func TestRunRequiredOutputFinalizationForcesOnlyOutputTool(t *testing.T) {
	provider := llm.NewMockProvider("mock").WithCapabilities(llm.Capabilities{ToolCalls: true, SupportsToolChoice: true})
	provider.AddToolCall("call_1", "submit_result", map[string]string{"result_json": `{"status":"ok"}`})
	outputTool := tools.NewSetOutputTool("submit_result", "result_json", "Submit the result")
	baseReq := llm.Request{
		Model: "mock-model",
		Messages: []llm.Message{
			llm.SystemText("You are a test agent."),
			llm.UserText("Review the thing."),
		},
		Tools: []llm.ToolSpec{
			outputTool.Spec(),
			{Name: "read_file", Description: "Read a file"},
		},
		ToolChoice:        llm.ToolChoice{Mode: llm.ToolChoiceAuto},
		ParallelToolCalls: true,
		MaxTurns:          20,
	}

	if err := runRequiredOutputFinalization(context.Background(), provider, baseReq, outputTool, "The review is complete."); err != nil {
		t.Fatalf("runRequiredOutputFinalization() error = %v", err)
	}
	if got := outputTool.Value(); got != `{"status":"ok"}` {
		t.Fatalf("outputTool.Value() = %q", got)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(provider.Requests))
	}
	finalReq := provider.Requests[0]
	if len(finalReq.Tools) != 1 || finalReq.Tools[0].Name != "submit_result" {
		t.Fatalf("finalizer tools = %#v, want only submit_result", finalReq.Tools)
	}
	if finalReq.ToolChoice.Mode != llm.ToolChoiceName || finalReq.ToolChoice.Name != "submit_result" {
		t.Fatalf("ToolChoice = %#v, want forced submit_result", finalReq.ToolChoice)
	}
	if finalReq.ParallelToolCalls {
		t.Fatal("finalizer should disable parallel tool calls")
	}
	if finalReq.MaxTurns != 2 {
		t.Fatalf("MaxTurns = %d, want 2", finalReq.MaxTurns)
	}
	last := finalReq.Messages[len(finalReq.Messages)-1]
	if last.Role != llm.RoleUser || !strings.Contains(last.Parts[0].Text, "did not call the required output tool") {
		t.Fatalf("finalizer prompt = %#v", last)
	}
}

func TestRunRequiredOutputFinalizationFailsWhenToolStillMissing(t *testing.T) {
	provider := llm.NewMockProvider("mock").WithCapabilities(llm.Capabilities{ToolCalls: true, SupportsToolChoice: true})
	provider.AddTextResponse("Done.")
	outputTool := tools.NewSetOutputTool("submit_result", "result_json", "Submit the result")
	baseReq := llm.Request{Messages: []llm.Message{llm.UserText("Review the thing.")}}

	err := runRequiredOutputFinalization(context.Background(), provider, baseReq, outputTool, "Done.")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), `required output tool "submit_result" was not called`) {
		t.Fatalf("error = %v", err)
	}
}
