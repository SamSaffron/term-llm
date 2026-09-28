package tools

import (
	"context"
	"github.com/samsaffron/term-llm/internal/llm"
	"testing"
)

type approvalScopeRunner struct {
	*lifecycleRunner
	parent *ApprovalManager
	scopes chan *ApprovalManager
	prompt bool
}

func (r *approvalScopeRunner) AgentApprovalScope(parent string) *ApprovalManager {
	return r.parent.CloneForAgentRun(parent)
}
func (r *approvalScopeRunner) RunAgentWithCallbackAndOptions(ctx context.Context, name, prompt string, depth int, id string, cb SubagentEventCallback, opts SpawnAgentRunOptions) (SpawnAgentRunResult, error) {
	scope := AgentApprovalScopeFromContext(ctx)
	r.scopes <- scope
	if r.prompt {
		_, _ = scope.PromptUIFunc("file", true, false, "")
	}
	return r.lifecycleRunner.RunAgentWithCallbackAndOptions(ctx, name, prompt, depth, id, cb, opts)
}

func TestDetachedChildrenFromTwoParentsKeepSeparateApprovalScopes(t *testing.T) {
	parent := NewApprovalManager(&ToolPermissions{})
	parent.BindWorkspaceSessionID("parent-one")
	runner := &approvalScopeRunner{lifecycleRunner: &lifecycleRunner{entered: make(chan string, 2), release: make(chan struct{})}, parent: parent, scopes: make(chan *ApprovalManager, 2)}
	tool := NewSpawnAgentTool(SpawnConfig{MaxParallel: 2, MaxDepth: 2, DefaultTimeout: 300}, 0)
	tool.SetRunner(runner)
	first := lifecycleResult(t, lifecycleCall(t, tool, llm.ContextWithSessionID(context.Background(), "parent-one"), `{"agent_name":"developer","prompt":"first","wait":0}`))
	firstScope := <-runner.scopes
	<-runner.entered
	parent.workspaceMu.Lock()
	parent.workspaceSessionID = "parent-two"
	parent.workspaceMu.Unlock()
	second := lifecycleResult(t, lifecycleCall(t, tool, llm.ContextWithSessionID(context.Background(), "parent-two"), `{"agent_name":"developer","prompt":"second","wait":0}`))
	secondScope := <-runner.scopes
	<-runner.entered
	if firstScope == nil || secondScope == nil || firstScope.workspaceSessionID != "parent-one" || secondScope.workspaceSessionID != "parent-two" {
		t.Fatalf("scopes for %s/%s: %#v, %#v", first.AgentID, second.AgentID, firstScope, secondScope)
	}
	close(runner.release)
	if err := tool.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
}
