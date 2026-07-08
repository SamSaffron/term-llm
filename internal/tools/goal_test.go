package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/session"
)

func TestGoalToolsCreateUpdateGet(t *testing.T) {
	create := NewCreateGoalTool()
	out, err := create.Execute(context.Background(), json.RawMessage(`{"objective":"ship /goal","token_budget":123}`))
	if err != nil {
		t.Fatalf("create Execute() error = %v", err)
	}
	if !strings.Contains(out.Content, "123") {
		t.Fatalf("create output = %q, want budget", out.Content)
	}

	update := NewUpdateGoalTool()
	out, err = update.Execute(context.Background(), json.RawMessage(`{"status":"complete","reason":"done"}`))
	if err != nil {
		t.Fatalf("update Execute() error = %v", err)
	}
	if !strings.Contains(out.Content, "complete") {
		t.Fatalf("update output = %q, want complete", out.Content)
	}

	now := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	goal := session.NewGoal("ship /goal", 123, now)
	goal.TokensUsed = 10
	get := NewGetGoalTool(func() *session.Goal { return goal.Clone() })
	out, err = get.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("get Execute() error = %v", err)
	}
	var decoded session.Goal
	if err := json.Unmarshal([]byte(out.Content), &decoded); err != nil {
		t.Fatalf("get output is not goal JSON: %v: %q", err, out.Content)
	}
	if decoded.Objective != "ship /goal" || decoded.TokenBudget != 123 || decoded.TokensUsed != 10 {
		t.Fatalf("decoded goal = %+v", decoded)
	}
}

func TestGoalToolValidation(t *testing.T) {
	if _, err := NewCreateGoalTool().Execute(context.Background(), json.RawMessage(`{"objective":""}`)); err == nil {
		t.Fatal("create_goal with empty objective: expected error")
	}
	if _, err := NewCreateGoalTool().Execute(context.Background(), json.RawMessage(`{"objective":"x","token_budget":-1}`)); err == nil {
		t.Fatal("create_goal with negative budget: expected error")
	}
	if _, err := NewUpdateGoalTool().Execute(context.Background(), json.RawMessage(`{"status":"active"}`)); err == nil {
		t.Fatal("update_goal with invalid status: expected error")
	}
	if _, err := NewUpdateGoalTool().Execute(context.Background(), json.RawMessage(`{"status":"blocked"}`)); err != nil {
		t.Fatalf("update_goal blocked should be valid: %v", err)
	}
}
