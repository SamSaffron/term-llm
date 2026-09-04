package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

type shellCollaborationTestController struct {
	mode       CollaborativeShellMode
	result     ShellResult
	err        error
	executions int
	args       SharedShellArgs
	contextErr error
}

func (c *shellCollaborationTestController) Mode(context.Context, string) CollaborativeShellMode {
	return c.mode
}
func (c *shellCollaborationTestController) Execute(ctx context.Context, _ string, args SharedShellArgs) (ShellResult, error) {
	c.executions++
	c.args = args
	c.contextErr = ctx.Err()
	return c.result, c.err
}
func (c *shellCollaborationTestController) PrepareRequestContext(_ context.Context, _ string, messages []llm.Message) ([]llm.Message, error) {
	return messages, nil
}
func (c *shellCollaborationTestController) PrepareCompactionContext(context.Context, string, *llm.CompactionResult) error {
	return nil
}

func TestSharedShellGuardianScopeDoesNotAuthorizeLocalShell(t *testing.T) {
	manager := NewApprovalManager(NewToolPermissions())
	manager.SetApprovalMode(ModeAuto)
	var request PolicyReviewRequest
	manager.SetPolicyReviewFunc(func(_ context.Context, got PolicyReviewRequest) (PolicyDecision, error) {
		request = got
		return PolicyDecision{Allowed: true, RiskLevel: "low", UserAuthorization: "high"}, nil
	}, nil)
	outcome, err := manager.CheckSharedShellApprovalWithContext(context.Background(), "echo guardian", nil)
	if err != nil || outcome != ProceedAlways {
		t.Fatalf("shared guardian outcome=%v err=%v", outcome, err)
	}
	if request.ApprovalScope != sharedShellApprovalScope || request.WorkDir != "" || !strings.Contains(request.ApprovalContext, "may be remote") {
		t.Fatalf("guardian request = %+v", request)
	}
	if manager.isGuardianExactShellApproved("echo guardian", "") {
		t.Fatal("shared guardian approval authorized local shell")
	}
}

func TestSharedShellApprovalCacheIsIsolatedFromLocalShell(t *testing.T) {
	manager := NewApprovalManager(NewToolPermissions())
	if err := manager.shellCache.AddCommand("echo local", "/tmp"); err != nil {
		t.Fatal(err)
	}
	prompts := 0
	manager.SharedShellPromptUIFunc = func(string) (ApprovalResult, error) {
		prompts++
		return ApprovalResult{Choice: ApprovalChoiceCommand}, nil
	}
	outcome, err := manager.CheckSharedShellApprovalWithContext(context.Background(), "echo local", nil)
	if err != nil || outcome != ProceedAlways || prompts != 1 {
		t.Fatalf("shared approval = %v, prompts=%d, err=%v", outcome, prompts, err)
	}
	if _, err := manager.CheckSharedShellApprovalWithContext(context.Background(), "echo shared", nil); err != nil {
		t.Fatal(err)
	}
	if local, ok := manager.checkShellApprovalNoPrompt("echo shared", "/tmp"); ok {
		t.Fatalf("shared cache unexpectedly authorized local command: %v", local)
	}
	if shared, err := manager.CheckSharedShellApprovalWithContext(context.Background(), "echo local", nil); err != nil || shared != ProceedAlways || prompts != 2 {
		t.Fatalf("remembered shared approval = %v, prompts=%d, err=%v", shared, prompts, err)
	}
}

func TestShellToolSharedDispatchNeverFallsBackLocal(t *testing.T) {
	controller := &shellCollaborationTestController{err: NewCollaborativeShellError("stale_shell", "generation changed")}
	tool := NewShellTool(nil, nil, DefaultOutputLimits())
	tool.setCollaborativeShell(controller, ShellRoutingControllerRequired)
	ctx := llm.ContextWithSessionID(context.Background(), "session")
	ctx = llm.ContextWithCallID(ctx, "call")
	ctx = ContextWithCollaborativeShellRunBinding(ctx, CollaborativeShellRunBinding{Required: true, ShellID: "sh_old"})
	output, err := tool.Execute(ctx, json.RawMessage(`{"command":"printf should-never-run > /definitely/not/a/path"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !output.IsError || controller.executions != 1 || controller.args.ExpectedShellID != "sh_old" || controller.args.ToolCallID != "call" {
		t.Fatalf("output=%+v executions=%d args=%+v", output, controller.executions, controller.args)
	}
}

func TestShellToolReportsTerminalChangeWithoutDroppingActivity(t *testing.T) {
	fence := NewCollaborativeShellActivityFence(12)
	controller := &shellCollaborationTestController{
		result: ShellResult{Stdout: "terminal_activity_before_command:\nls output", ExitCode: -1},
		err:    NewCollaborativeShellError("terminal_changed", "command was not executed"),
	}
	tool := NewShellTool(nil, nil, DefaultOutputLimits())
	tool.setCollaborativeShell(controller, ShellRoutingControllerRequired)
	ctx := ContextWithCollaborativeShellRunBinding(context.Background(), CollaborativeShellRunBinding{Required: true, ShellID: "sh_one", Fence: fence})
	output, err := tool.Execute(ctx, json.RawMessage(`{"command":"stale command"}`))
	if err != nil || !output.IsError || !strings.Contains(output.Content, "terminal_changed") || !strings.Contains(output.Content, "ls output") || controller.args.ActivityFence != fence {
		t.Fatalf("output=%+v err=%v args=%+v", output, err, controller.args)
	}
}

func TestShellToolReportsTimeoutWithRecoveryFailure(t *testing.T) {
	controller := &shellCollaborationTestController{
		result: ShellResult{Stdout: "partial", ExitCode: -1, TimedOut: true, RecoveryFailed: true},
		err:    NewCollaborativeShellError("recovery_failed", "timed-out command could not recover"),
	}
	tool := NewShellTool(nil, nil, DefaultOutputLimits())
	tool.setCollaborativeShell(controller, ShellRoutingControllerRequired)
	ctx := ContextWithCollaborativeShellRunBinding(context.Background(), CollaborativeShellRunBinding{Required: true, ShellID: "sh_one"})
	output, err := tool.Execute(ctx, json.RawMessage(`{"command":"sleep 10"}`))
	if err != nil || !output.IsError || !output.TimedOut || !strings.Contains(output.Content, "recovery_failed") || !strings.Contains(output.Content, "partial") {
		t.Fatalf("output=%+v err=%v", output, err)
	}
}

func TestBuildSharedShellOptionsAvoidsLocalWorkingDirectoryClaims(t *testing.T) {
	options := BuildSharedShellOptions("echo remote")
	if len(options) == 0 {
		t.Fatal("missing shared shell options")
	}
	for _, option := range options {
		if strings.Contains(strings.ToLower(option.Description), "working directory") {
			t.Fatalf("misleading shared option: %+v", option)
		}
	}
}

func TestShellToolSharedRejectsLocalOnlyFieldsBeforeController(t *testing.T) {
	for name, args := range map[string]string{
		"working_dir":    `{"command":"pwd","working_dir":"/missing"}`,
		"env":            `{"command":"pwd","env":{"TOKEN":"secret"}}`,
		"affected_paths": `{"command":"pwd","affected_paths":["/missing"]}`,
		"output_claims":  `{"command":"pwd","output_claims":[{"path":"/missing","kind":"generate"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			controller := &shellCollaborationTestController{}
			tool := NewShellTool(nil, nil, DefaultOutputLimits())
			tool.setCollaborativeShell(controller, ShellRoutingControllerRequired)
			ctx := ContextWithCollaborativeShellRunBinding(context.Background(), CollaborativeShellRunBinding{Required: true, ShellID: "sh_one"})
			output, err := tool.Execute(ctx, json.RawMessage(args))
			if err != nil || !output.IsError || controller.executions != 0 || !strings.Contains(output.Content, "INVALID_PARAMS") {
				t.Fatalf("output=%+v err=%v executions=%d", output, err, controller.executions)
			}
		})
	}
}

func TestShellToolSharedDispatchPassesCancellationAndNeedsNoLocalDirectory(t *testing.T) {
	controller := &shellCollaborationTestController{result: ShellResult{Stdout: "remote", ExitCode: 0}}
	tool := NewShellTool(nil, nil, DefaultOutputLimits())
	tool.setCollaborativeShell(controller, ShellRoutingControllerRequired)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ctx = llm.ContextWithSessionID(ctx, "session")
	ctx = llm.ContextWithCallID(ctx, "call")
	ctx = ContextWithCollaborativeShellRunBinding(ctx, CollaborativeShellRunBinding{Required: true, ShellID: "sh_remote"})
	output, err := tool.Execute(ctx, json.RawMessage(`{"command":"pwd","timeout_seconds":9}`))
	if err != nil || output.IsError || !strings.Contains(output.Content, "remote") || !errors.Is(controller.contextErr, context.Canceled) {
		t.Fatalf("output=%+v err=%v controller=%+v", output, err, controller)
	}
	if controller.args.ExpectedShellID != "sh_remote" || controller.args.ToolCallID != "call" || controller.args.TimeoutSeconds != 9 {
		t.Fatalf("controller args=%+v", controller.args)
	}
}

func TestShellToolControllerRequiredMissingBindingFailsClosed(t *testing.T) {
	for _, ctx := range []context.Context{
		context.Background(),
		ContextWithCollaborativeShellRunBinding(context.Background(), CollaborativeShellRunBinding{Required: false}),
	} {
		tool := NewShellTool(nil, nil, DefaultOutputLimits())
		tool.setCollaborativeShell(nil, ShellRoutingControllerRequired)
		output, err := tool.Execute(ctx, json.RawMessage(`{"command":"echo local"}`))
		if err != nil || !output.IsError {
			t.Fatalf("output=%+v err=%v", output, err)
		}
	}
}

func TestRegistryReappliesCollaborativeControllerAfterLimits(t *testing.T) {
	cfg := &ToolConfig{Enabled: []string{ShellToolName}, ShellAllow: []string{"echo *"}}
	registry, err := NewLocalToolRegistry(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	controller := &shellCollaborationTestController{mode: CollaborativeShellMode{Enabled: true, State: CollaborativeShellReady, ShellID: "sh_one"}}
	registry.SetCollaborativeShellController(controller, ShellRoutingControllerRequired)
	registry.SetLimits(DefaultOutputLimits())
	mode := registry.CollaborativeShellMode(context.Background(), "session")
	if mode.ShellID != "sh_one" {
		t.Fatalf("mode=%+v", mode)
	}
	tool, ok := registry.Get(ShellToolName)
	if !ok {
		t.Fatal("shell tool missing")
	}
	ctx := ContextWithCollaborativeShellRunBinding(context.Background(), CollaborativeShellRunBinding{Required: true, ShellID: "sh_one"})
	controller.err = errors.New("sentinel")
	output, err := tool.Execute(ctx, json.RawMessage(`{"command":"echo hi"}`))
	if err != nil || !output.IsError || controller.executions != 1 {
		t.Fatalf("output=%+v err=%v executions=%d", output, err, controller.executions)
	}
}
