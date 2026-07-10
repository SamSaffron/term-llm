package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/spf13/viper"
)

func TestAskOutputToolRunsOnCompleteAfterCentralizedRunner(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	repo := t.TempDir()
	git := exec.Command("git", "init", "-q", repo)
	if output, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	gitDir := filepath.Join(repo, ".git")
	for _, name := range []string{"COMMIT_EDITMSG", "GITGUI_MSG"} {
		if err := os.WriteFile(filepath.Join(gitDir, name), []byte("stale\n"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	agentDir := filepath.Join(t.TempDir(), "output-agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("mkdir agent: %v", err)
	}
	agentYAML := `name: output-agent
output_tool:
  name: set_commit_message
  param: message
  description: Set the commit message
on_complete: |
  tee .git/COMMIT_EDITMSG > .git/GITGUI_MSG
`
	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte(agentYAML), 0o644); err != nil {
		t.Fatalf("write agent: %v", err)
	}

	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	configDir := filepath.Join(configHome, "term-llm")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	configYAML := `default_provider: mock
providers:
  mock:
    model: mock-model
sessions:
  enabled: false
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })

	const message = "chore: replace stale message"
	provider := llm.NewMockProvider("mock").
		WithCapabilities(llm.Capabilities{ToolCalls: true, SupportsToolChoice: true}).
		AddToolCall("call-1", "set_commit_message", map[string]string{"message": message})
	oldProviderFactory := newAskProvider
	newAskProvider = func(*config.Config, bool) (llm.Provider, error) { return provider, nil }
	t.Cleanup(func() { newAskProvider = oldProviderFactory })

	oldAgent, oldText, oldPorcelain := askAgent, askText, askPorcelain
	oldJSON, oldProgressive, oldFast := askJSON, askProgressive, askFast
	askAgent, askText, askPorcelain = agentDir, true, true
	askJSON, askProgressive, askFast = false, false, false
	t.Cleanup(func() {
		askAgent, askText, askPorcelain = oldAgent, oldText, oldPorcelain
		askJSON, askProgressive, askFast = oldJSON, oldProgressive, oldFast
	})

	var stdout, stderr bytes.Buffer
	oldStdout, oldStderr := askCmd.OutOrStdout(), askCmd.ErrOrStderr()
	askCmd.SetOut(&stdout)
	askCmd.SetErr(&stderr)
	t.Cleanup(func() {
		askCmd.SetOut(oldStdout)
		askCmd.SetErr(oldStderr)
	})
	if err := runAsk(askCmd, []string{"prepare a commit message"}); err != nil {
		t.Fatalf("runAsk: %v\nstderr: %s", err, stderr.String())
	}

	for _, name := range []string{"COMMIT_EDITMSG", "GITGUI_MSG"} {
		got, err := os.ReadFile(filepath.Join(gitDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != message {
			t.Errorf("%s = %q, want %q", name, got, message)
		}
	}
}

func TestEnsureOutputToolCapturedCoaxesModelWithoutAssistantText(t *testing.T) {
	provider := llm.NewMockProvider("mock").WithCapabilities(llm.Capabilities{ToolCalls: true, SupportsToolChoice: true})
	provider.AddToolCall("call_1", "submit_result", map[string]string{"result_json": `{"ok":true}`})
	outputTool := tools.NewSetOutputTool("submit_result", "result_json", "Submit the result")

	if err := ensureOutputToolCaptured(context.Background(), provider, nil, llm.Request{}, outputTool, ""); err != nil {
		t.Fatalf("ensureOutputToolCaptured() error = %v", err)
	}
	if got := outputTool.Value(); got != `{"ok":true}` {
		t.Fatalf("outputTool.Value() = %q", got)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(provider.Requests))
	}
	last := provider.Requests[0].Messages[len(provider.Requests[0].Messages)-1]
	if last.Role != llm.RoleUser || !strings.Contains(last.Parts[0].Text, "MUST now make exactly one tool call") {
		t.Fatalf("finalizer prompt = %#v", last)
	}
}

func TestEnsureOutputToolCapturedNoopsAfterToolCapturesValue(t *testing.T) {
	provider := llm.NewMockProvider("mock").WithCapabilities(llm.Capabilities{ToolCalls: true, SupportsToolChoice: true})
	outputTool := tools.NewSetOutputTool("submit_result", "result_json", "Submit the result")

	args, _ := json.Marshal(map[string]string{"result_json": `{"ok":true}`})
	if _, err := outputTool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if err := ensureOutputToolCaptured(context.Background(), provider, nil, llm.Request{}, outputTool, ""); err != nil {
		t.Fatalf("ensureOutputToolCaptured() error = %v", err)
	}
	if len(provider.Requests) != 0 {
		t.Fatalf("provider saw %d requests, want 0", len(provider.Requests))
	}
}

func TestEnsureOutputToolCapturedAllowsEmptyStringValue(t *testing.T) {
	provider := llm.NewMockProvider("mock").WithCapabilities(llm.Capabilities{ToolCalls: true, SupportsToolChoice: true})
	outputTool := tools.NewSetOutputTool("submit_result", "result_json", "Submit the result")

	args, _ := json.Marshal(map[string]string{"result_json": ""})
	if _, err := outputTool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if err := ensureOutputToolCaptured(context.Background(), provider, nil, llm.Request{}, outputTool, ""); err != nil {
		t.Fatalf("ensureOutputToolCaptured() error = %v", err)
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

	registry := llm.NewToolRegistry()
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
	if len(finalReq.Tools) != 1 || finalReq.Tools[0].Name != "submit_result" {
		t.Fatalf("finalizer tools = %#v, want only submit_result", finalReq.Tools)
	}
	if finalReq.ToolChoice.Mode != llm.ToolChoiceName || finalReq.ToolChoice.Name != "submit_result" {
		t.Fatalf("ToolChoice = %#v, want forced submit_result", finalReq.ToolChoice)
	}
	if finalReq.ParallelToolCalls {
		t.Fatal("finalizer should disable parallel tool calls")
	}
	if finalReq.MaxTurns != 3 {
		t.Fatalf("MaxTurns = %d, want 3", finalReq.MaxTurns)
	}
	last := finalReq.Messages[len(finalReq.Messages)-1]
	if last.Role != llm.RoleUser || !strings.Contains(last.Parts[0].Text, "The task is DONE") || !strings.Contains(last.Parts[0].Text, "MUST now make exactly one tool call") {
		t.Fatalf("finalizer prompt = %#v", last)
	}
}

func TestRunOutputToolFinalizationFallsBackToAutoWhenToolChoiceUnsupported(t *testing.T) {
	provider := llm.NewMockProvider("mock").WithCapabilities(llm.Capabilities{ToolCalls: true, SupportsToolChoice: false})
	provider.AddToolCall("call_1", "submit_result", map[string]string{"result_json": `{"status":"ok"}`})
	outputTool := tools.NewSetOutputTool("submit_result", "result_json", "Submit the result")

	if err := runOutputToolFinalization(context.Background(), provider, nil, llm.Request{}, outputTool, "Done."); err != nil {
		t.Fatalf("runOutputToolFinalization() error = %v", err)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(provider.Requests))
	}
	if provider.Requests[0].ToolChoice.Mode != llm.ToolChoiceAuto {
		t.Fatalf("ToolChoice = %#v, want auto", provider.Requests[0].ToolChoice)
	}
}

func TestRunOutputToolFinalizationRetriesWhenModelRespondsWithText(t *testing.T) {
	provider := llm.NewMockProvider("mock").WithCapabilities(llm.Capabilities{ToolCalls: true, SupportsToolChoice: true})
	provider.AddTextResponse("Sorry, here is the final answer in prose.")
	provider.AddTextResponse("Still prose instead of a tool call.")
	provider.AddTextResponse("I continue to be prose-only.")
	provider.AddToolCall("call_4", "submit_result", map[string]string{"result_json": `{"status":"ok"}`})
	outputTool := tools.NewSetOutputTool("submit_result", "result_json", "Submit the result")

	if err := runOutputToolFinalization(context.Background(), provider, nil, llm.Request{}, outputTool, "Done."); err != nil {
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
	provider := llm.NewMockProvider("mock").WithCapabilities(llm.Capabilities{ToolCalls: true, SupportsToolChoice: true})
	provider.AddTextResponse("Done.")
	provider.AddTextResponse("Still prose.")
	provider.AddTextResponse("Still not a tool call.")
	outputTool := tools.NewSetOutputTool("submit_result", "result_json", "Submit the result")
	baseReq := llm.Request{Messages: []llm.Message{llm.UserText("Review the thing.")}}

	err := runOutputToolFinalization(context.Background(), provider, nil, baseReq, outputTool, "Done.")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), `output tool "submit_result" was not called`) {
		t.Fatalf("error = %v", err)
	}
}
