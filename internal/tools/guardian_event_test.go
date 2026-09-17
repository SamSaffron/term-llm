package tools

import (
	"context"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestGuardianEventRetainsShellCorrelation(t *testing.T) {
	mgr := NewApprovalManager(nil)
	mgr.SetAutoHeadless(true)
	wantUsage := llm.Usage{InputTokens: 31, OutputTokens: 7, CachedInputTokens: 20, CacheWriteTokens: 4}
	mgr.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		return PolicyDecision{Allowed: true, RiskLevel: "low", UserAuthorization: "high", Model: "guardian-model", Usage: wantUsage, StateBytes: 456}, nil
	}, nil)
	var got GuardianEvent
	mgr.GuardianEventFunc = func(event GuardianEvent) { got = event }
	ctx := llm.ContextWithCallID(context.Background(), "shell-call")

	outcome, handled, err := mgr.checkShellGuardianApproval(ctx, "echo hello", "/tmp/work", nil)
	if err != nil || !handled || outcome != ProceedAlways {
		t.Fatalf("review result = (%v, %v, %v)", outcome, handled, err)
	}
	if got.ToolCallID != "shell-call" || got.Command != "echo hello" || got.WorkDir != "/tmp/work" || got.Outcome != GuardianApproved {
		t.Fatalf("guardian event lost correlation: %#v", got)
	}
	if got.DurationMS <= 0 || got.StateBytes != 456 {
		t.Fatalf("lost metrics: %#v", got)
	}
	if got.Model != "guardian-model" || got.Usage != wantUsage {
		t.Fatalf("guardian event lost accounting data: %#v", got)
	}
}

func TestGuardianTimingOnErrorAndInheritedCallback(t *testing.T) {
	parent := NewApprovalManager(nil)
	defer parent.Close()
	parent.SetPolicyReviewFunc(func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		return PolicyDecision{StateBytes: 123}, context.Canceled
	}, nil)
	child := NewApprovalManager(nil)
	defer child.Close()
	if err := child.SetParent(parent); err != nil {
		t.Fatal(err)
	}
	d, err := child.ReviewPolicy(context.Background(), PolicyReviewRequest{})
	if err != context.Canceled || d.DurationMS <= 0 || d.StateBytes != 123 {
		t.Fatalf("%+v %v", d, err)
	}
	event := child.guardianPathDecisionEvent(context.Background(), ManageWorkspaceToolName, "/workspace", true, GuardianError, "failed", d)
	if event.DurationMS != d.DurationMS || event.StateBytes != 123 || event.Path != "/workspace" {
		t.Fatalf("%+v", event)
	}
	ctx, reviews := llm.ContextWithGuardianReviewCapture(llm.ContextWithCallID(context.Background(), "workspace-call"))
	event.ToolCallID = "workspace-call"
	child.emitGuardianEventForContext(ctx, event)
	if len(reviews()) != 1 || reviews()[0].DurationMS != d.DurationMS || reviews()[0].StateBytes != 123 {
		t.Fatalf("%+v", reviews())
	}
}
