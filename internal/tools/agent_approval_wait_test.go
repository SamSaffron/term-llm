package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestDetachedAgentReportsAwaitingApproval(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	parent := NewApprovalManager(&ToolPermissions{})
	parent.BindWorkspaceSessionID("parent")
	parent.PromptUIFunc = func(_ string, _, _ bool, _ string) (ApprovalResult, error) {
		close(entered)
		<-release
		return ApprovalResult{}, nil
	}
	runner := &approvalScopeRunner{lifecycleRunner: &lifecycleRunner{entered: make(chan string, 1), release: make(chan struct{})}, parent: parent, scopes: make(chan *ApprovalManager, 1), prompt: true}
	close(runner.release)
	tool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 1, MaxDepth: 2, DefaultTimeout: 300}, 0)
	tool.SetRunner(runner)
	ctx := llm.ContextWithSessionID(context.Background(), "parent")
	spawned := lifecycleResult(t, lifecycleCall(t, tool, ctx, `{"agent_name":"developer","prompt":"needs approval","wait":0}`))
	<-entered
	listed := lifecycleCall(t, &agentControlTool{name: ListAgentsToolName, spawn: tool}, ctx, `{}`)
	if !strings.Contains(listed.Content, `"status":"awaiting_approval"`) {
		t.Fatalf("approval status = %s", listed.Content)
	}
	close(release)
	if err := tool.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := lifecycleCall(t, &agentControlTool{name: WaitAgentToolName, spawn: tool}, ctx, `{"agent_ids":["`+spawned.AgentID+`"]}`)
	if !strings.Contains(done.Content, `"status":"completed"`) {
		t.Fatalf("approved child = %s", done.Content)
	}
}
