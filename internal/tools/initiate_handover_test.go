package tools

import (
	"context"
	"encoding/json"
	"testing"
)

func TestInitiateHandoverTool_Spec(t *testing.T) {
	tool := NewInitiateHandoverTool()
	spec := tool.Spec()
	if spec.Name != InitiateHandoverToolName {
		t.Errorf("expected name %q, got %q", InitiateHandoverToolName, spec.Name)
	}
}

func TestInitiateHandoverTool_Preview(t *testing.T) {
	tool := NewInitiateHandoverTool()

	args, _ := json.Marshal(InitiateHandoverArgs{Agent: "developer"})
	if got := tool.Preview(args); got != "@developer" {
		t.Errorf("expected @developer, got %q", got)
	}

	// With @ prefix already present
	args, _ = json.Marshal(InitiateHandoverArgs{Agent: "@planner"})
	if got := tool.Preview(args); got != "@planner" {
		t.Errorf("expected @planner, got %q", got)
	}
}

func TestInitiateHandoverTool_Execute_EmptyAgent(t *testing.T) {
	tool := NewInitiateHandoverTool()
	args, _ := json.Marshal(InitiateHandoverArgs{Agent: ""})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	var result InitiateHandoverResult
	if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "error" {
		t.Errorf("expected error status, got %q", result.Status)
	}
}

func TestInitiateHandoverTool_Execute_NoUIFunc(t *testing.T) {
	// Ensure no global handler is set
	ClearHandoverUIFunc()

	tool := NewInitiateHandoverTool()
	args, _ := json.Marshal(InitiateHandoverArgs{Agent: "developer"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	var result InitiateHandoverResult
	if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "error" {
		t.Errorf("expected error status, got %q", result.Status)
	}
}

func TestInitiateHandoverTool_Execute_Confirmed(t *testing.T) {
	tool := NewInitiateHandoverTool()
	SetHandoverUIFunc(func(_ context.Context, agent string) (bool, error) {
		if agent != "developer" {
			t.Errorf("expected agent developer, got %q", agent)
		}
		return true, nil
	})
	defer ClearHandoverUIFunc()

	args, _ := json.Marshal(InitiateHandoverArgs{Agent: "developer"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	var result InitiateHandoverResult
	if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "confirmed" {
		t.Errorf("expected confirmed, got %q", result.Status)
	}
}

func TestInitiateHandoverTool_Execute_Cancelled(t *testing.T) {
	tool := NewInitiateHandoverTool()
	SetHandoverUIFunc(func(_ context.Context, agent string) (bool, error) {
		return false, nil
	})
	defer ClearHandoverUIFunc()

	args, _ := json.Marshal(InitiateHandoverArgs{Agent: "developer"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	var result InitiateHandoverResult
	if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "cancelled" {
		t.Errorf("expected cancelled, got %q", result.Status)
	}
}

func TestInitiateHandoverTool_Execute_ContextHandler(t *testing.T) {
	tool := NewInitiateHandoverTool()
	// Ensure global is not set
	ClearHandoverUIFunc()

	ctx := ContextWithHandoverFunc(context.Background(), func(_ context.Context, agent string) (bool, error) {
		if agent != "reviewer" {
			t.Errorf("expected agent reviewer, got %q", agent)
		}
		return true, nil
	})

	args, _ := json.Marshal(InitiateHandoverArgs{Agent: "@reviewer"})
	out, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	var result InitiateHandoverResult
	if err := json.Unmarshal([]byte(out.Content), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "confirmed" {
		t.Errorf("expected confirmed, got %q", result.Status)
	}
}
