package tools

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSetOutputToolCalledWithEmptyValue(t *testing.T) {
	tool := NewSetOutputTool("submit_review", "review_json", "Submit review")

	if tool.Called() {
		t.Fatal("new output tool should not be marked called")
	}

	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"review_json":""}`)); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	if !tool.Called() {
		t.Fatal("empty output value should still mark output tool called")
	}
	if got := tool.Value(); got != "" {
		t.Fatalf("Value() = %q, want empty string", got)
	}
}
