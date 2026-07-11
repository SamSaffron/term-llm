package tools

import (
	"context"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestGuardianEventRetainsShellCorrelation(t *testing.T) {
	mgr := NewApprovalManager(nil)
	mgr.SetAutoHeadless(true)
	mgr.PolicyReviewFunc = func(context.Context, PolicyReviewRequest) (PolicyDecision, error) {
		return PolicyDecision{Allowed: true, RiskLevel: "low", UserAuthorization: "high"}, nil
	}
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
}
