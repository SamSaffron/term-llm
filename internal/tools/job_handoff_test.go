package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestJobHandoffToolUsesContextHandler(t *testing.T) {
	tool := NewJobHandoffTool()
	called := false
	ctx := ContextWithJobHandoffFunc(context.Background(), func(ctx context.Context, req JobHandoffRequest) (JobHandoffResult, error) {
		called = true
		if req.Instructions != "continue carefully" {
			t.Fatalf("instructions = %q", req.Instructions)
		}
		if req.AgentName != "developer" {
			t.Fatalf("agent_name = %q", req.AgentName)
		}
		return JobHandoffResult{Status: "started", JobID: "job_1", RunID: "run_1", SessionID: "sess_1"}, nil
	})

	args, _ := json.Marshal(JobHandoffRequest{Instructions: " continue carefully ", AgentName: " developer "})
	out, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !called {
		t.Fatal("handler was not called")
	}
	if !strings.Contains(out.Content, `"job_id":"job_1"`) || !strings.Contains(out.Content, `"run_id":"run_1"`) {
		t.Fatalf("unexpected output: %s", out.Content)
	}
}

func TestJobHandoffToolRequiresHandler(t *testing.T) {
	tool := NewJobHandoffTool()
	out, err := tool.Execute(context.Background(), json.RawMessage(`{"instructions":"go long"}`))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !strings.Contains(out.Content, "job handoff is not available") {
		t.Fatalf("unexpected output: %s", out.Content)
	}
}
