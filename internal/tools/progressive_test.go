package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestUpdateProgressToolRejectsNonObjectState(t *testing.T) {
	tool := NewUpdateProgressTool()

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"state":"bad"}`))
	if err == nil {
		t.Fatal("expected error for non-object state")
	}
}

func TestFinalizeProgressToolIsFinishing(t *testing.T) {
	tool := NewFinalizeProgressTool()

	finishing, ok := any(tool).(llm.FinishingTool)
	if !ok {
		t.Fatal("expected finalize progress tool to implement FinishingTool")
	}
	if !finishing.IsFinishingTool() {
		t.Fatal("expected finalize progress tool to be a finishing tool")
	}
}
