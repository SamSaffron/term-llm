package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

func TestUpdateProgressToolRejectsNonObjectState(t *testing.T) {
	t.Parallel()

	tool := NewUpdateProgressTool()

	_, err := tool.Execute(context.Background(), json.RawMessage(`{"state":"bad"}`))
	if err == nil {
		t.Fatal("expected error for non-object state")
	}
}

func TestFinalizeProgressToolIsFinishing(t *testing.T) {
	t.Parallel()

	tool := NewFinalizeProgressTool()

	finishing, ok := any(tool).(core.FinishingTool)
	if !ok {
		t.Fatal("expected finalize progress tool to implement FinishingTool")
	}
	if !finishing.IsFinishingTool() {
		t.Fatal("expected finalize progress tool to be a finishing tool")
	}
}
