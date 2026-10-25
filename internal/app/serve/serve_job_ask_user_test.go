package serve

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/tools"
)

func TestJobAskUserFailsFastWithoutHumanAuthorization(t *testing.T) {
	const unavailableMessage = "ask_user is unavailable in this background context because no user is present to answer. No answer or selection was made."
	args := map[string]any{"questions": []map[string]any{{
		"header": "Permission", "question": "May I proceed?", "options": []map[string]any{
			{"label": "Yes", "description": "Proceed"}, {"label": "No", "description": "Stop"},
		},
	}}}
	provider := mockprovider.NewMockProvider("mock").AddToolCall("job-question", tools.AskUserToolName, args).AddTextResponse("No user is present to answer; no answer or selection was made.")
	cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}
	runner := newCmdRunner(cfg, cmdRunnerOptions{}).(*cmdRunner)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	env, err := runner.prepare(ctx, runpkg.Request{
		Platform: runpkg.PlatformJob, ProviderInstance: provider, Tools: tools.AskUserToolName,
		Prompt: "Ask before proceeding", Cwd: t.TempDir(), DeferSession: true,
	}, eventSinkFunc(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer env.Close()
	if env.runtime.platform != "jobs" {
		t.Fatalf("job platform=%q", env.runtime.platform)
	}
	// Check the actual tool output as well as the provider-visible continuation:
	// unavailable input must never manufacture trusted first-party approval.
	req := env.llmReq
	toolCtx, _, _ := env.runtime.prepareRunContext(ctx, tools.CollaborativeShellRunBinding{}, &req)
	raw, _ := json.Marshal(args)
	output, err := tools.NewAskUserTool().Execute(toolCtx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if output.TrustedUserInput != "" || !strings.Contains(output.Content, unavailableMessage) {
		t.Fatalf("background output must report unavailable input without an answer: %+v", output)
	}
	if role, text, ok := core.ApprovalTurnForTool(tools.AskUserToolName, output); ok {
		t.Fatalf("background failure became trusted approval: role=%s text=%q", role, text)
	}
	result, err := env.RunWithEvents(ctx, false, false, env.inputMessages, env.llmReq, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Err() != nil {
		t.Fatalf("job waited for input: %v", ctx.Err())
	}
	if len(env.runtime.pendingAskUserPrompts()) != 0 {
		t.Fatal("job created an input gate")
	}
	if !strings.Contains(result.Text.String(), "no answer or selection was made") {
		t.Fatalf("job did not continue: %q", result.Text.String())
	}
	requests := provider.RecordedRequests()
	if len(requests) != 2 {
		t.Fatalf("provider turns=%d", len(requests))
	}
	exposed := false
	for _, tool := range requests[0].Tools {
		if tool.Name == tools.AskUserToolName {
			exposed = true
		}
	}
	if !exposed {
		t.Fatal("ask_user removed from background tools")
	}
	found := false
	for _, msg := range requests[1].Messages {
		text := core.CollectToolResultText(msg.Parts)
		if strings.Contains(text, unavailableMessage) {
			found = true
		}
	}
	if !found {
		t.Fatal("provider did not receive unavailable-input tool result")
	}
}
