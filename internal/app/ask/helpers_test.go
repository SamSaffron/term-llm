package ask

import (
	"context"
	"encoding/json"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

type progressiveTestTool struct{}

func (t *progressiveTestTool) Spec() core.ToolSpec {
	return core.ToolSpec{Name: "progressive_test_tool", Description: "test tool", Schema: map[string]any{"type": "object"}}
}

func (t *progressiveTestTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	return core.TextOutput("tool ok"), nil
}

func (t *progressiveTestTool) Preview(args json.RawMessage) string { return "" }

func (t *progressiveTestTool) IsFinishingTool() bool { return true }
